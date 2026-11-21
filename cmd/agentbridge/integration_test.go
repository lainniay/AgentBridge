package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/adapter/codex"
	"github.com/lainniay/AgentBridge/internal/api"
	"github.com/lainniay/AgentBridge/internal/blackboard"
	"github.com/lainniay/AgentBridge/internal/hub"
)

func TestLinkLatestSkipsBoundSessionsAndBackfills(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("optional Antigravity workspace index client unavailable")
	}
	workspace, agyBase, codexRoot := t.TempDir(), t.TempDir(), t.TempDir()
	agyRoot := filepath.Join(agyBase, "brain")
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	workspaceURI := url.URL{Scheme: "file", Path: workspace}
	workspaceURIs, err := json.Marshal([]string{workspaceURI.String()})
	if err != nil {
		t.Fatal(err)
	}
	var query strings.Builder
	query.WriteString("CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT);")
	for i, id := range []string{"oldest", "expected", "bound"} {
		fmt.Fprintf(&query, "INSERT INTO conversation_summaries VALUES ('%s','%[1]s','%s');", id, strings.ReplaceAll(string(workspaceURIs), "'", "''"))
		ts := now.Add(time.Duration(i-4) * time.Minute)
		agyLog := filepath.Join(agyRoot, id, ".system_generated", "logs", "transcript.jsonl")
		if err := os.MkdirAll(filepath.Dir(agyLog), 0700); err != nil {
			t.Fatal(err)
		}
		agyEntry := fmt.Sprintf(`{"step_index":0,"type":"USER_INPUT","created_at":%q,"content":"Antigravity question"}`+"\n", ts.Format(time.RFC3339Nano))
		codexLog := filepath.Join(codexRoot, id+".jsonl")
		codexEntries := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`+"\n"+
			`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","turn_id":"turn","message":"Codex question"}}`+"\n", id, workspace, ts.Format(time.RFC3339Nano))
		for path, contents := range map[string]string{agyLog: agyEntry, codexLog: codexEntries} {
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, ts, ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out, err := exec.Command(sqlite, filepath.Join(agyBase, "conversation_summaries.db"), query.String()).CombinedOutput(); err != nil {
		t.Fatalf("create summary fixture: %v %s", err, out)
	}
	if _, err := h.BindRoom(workspace, []hub.Binding{
		{Provider: "antigravity", SessionID: "bound"}, {Provider: "codex", SessionID: "bound"},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := api.New(ctx, h, workspace, antigravity.New(agyRoot), codex.New(codexRoot))
	server := httptest.NewServer(service.Handler("127.0.0.1"))
	defer func() {
		server.Close()
		cancel()
		service.Wait()
	}()

	var out bytes.Buffer
	if err := control("link", []string{"--workspace", workspace, "--latest", "--backfill-turns", "1"}, &out, &out, server.URL); err != nil {
		t.Fatal(err)
	}
	var room hub.Room
	if err := json.Unmarshal(out.Bytes(), &room); err != nil || len(room.Participants) != 2 {
		t.Fatalf("unexpected room: %s (%v)", &out, err)
	}
	providers := make(map[string]bool)
	for _, p := range room.Participants {
		if p.SessionID != "expected" {
			t.Fatalf("selected non-latest available session: %+v", p)
		}
		providers[p.Provider] = true
	}
	if !providers["antigravity"] || !providers["codex"] {
		t.Fatalf("missing provider: %+v", room.Participants)
	}
	entries, err := h.Blackboard(room.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected one backfilled entry from each peer: %+v (%v)", entries, err)
	}
}

func TestHookFailOpenAndFastPaths(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "{}")
	}))
	defer server.Close()
	cases := []struct{ event, stdin string }{
		{"pre-tool-use", `{"conversationId":"conversation","toolCall":{"name":"read_file"}}`},
		{"pre-tool-use", `{"conversationId":"conversation","toolCall":{"name":"call_mcp_tool","args":{"ServerName":"other","ToolName":"list_items","Arguments":{}}}}`},
		{"pre-invocation", `{"conversationId":"conversation","invocationNum":2,"lastUserInput":"already sent"}`},
		{"stop", `{"conversationId":"conversation","terminationReason":"USER_CANCELED"}`},
		{"pre-invocation", `{"conversationId":`},
		{"pre-invocation", `{"lastUserInput":"absent identity"}`},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		runHook(strings.NewReader(tc.stdin), &out, server.URL, tc.event)
		if out.String() != "{}\n" {
			t.Fatalf("%s should fail open: %q", tc.event, &out)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("local fast paths contacted the daemon")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	start := time.Now()
	runHook(strings.NewReader(`{"conversationId":"conversation"}`), &out, endpoint, "pre-invocation")
	if out.String() != "{}\n" || time.Since(start) >= 300*time.Millisecond {
		t.Fatalf("offline hook output %q after %s", &out, time.Since(start))
	}
}

func TestHookWithDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	root := t.TempDir()
	agy := antigravity.New(root)
	service := api.New(ctx, h, workspace, agy)
	server := httptest.NewServer(service.Handler("127.0.0.1"))
	defer func() {
		server.Close()
		cancel()
		service.Wait()
	}()

	conversation := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(root, conversation, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(
		`{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:31:00Z","content":"Question"}`+"\n"+
			`{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:31:01Z","thinking":"private","tool_calls":[{"name":"write_file","args":{"Path":"file.go","Content":"private"}}]}`+"\n"+
			`{"step_index":6,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:31:02Z","thinking":"private","content":"Answer"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hook := func(event, fields string) string {
		t.Helper()
		var out bytes.Buffer
		in := fmt.Sprintf(`{"conversationId":%q,"cwd":%q,"transcriptPath":%q,%s}`, conversation, workspace, path, fields)
		runHook(strings.NewReader(in), &out, server.URL, event)
		return strings.TrimSpace(out.String())
	}
	fields := `"lastUserInput":"Question","initialNumSteps":5,"invocationNum":0`
	if got := hook("pre-invocation", fields); got != "{}" {
		t.Fatalf("unbound result %s", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agent")); !os.IsNotExist(err) {
		t.Fatalf("unbound hook wrote to workspace: %v", err)
	}
	room, err := h.BindRoom(workspace, []hub.Binding{
		{Provider: "antigravity", SessionID: conversation},
		{Provider: "codex", SessionID: "peer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.IngestEntry(room.ID, room.Participants[1].ID, blackboard.Entry{Role: "assistant", NativeTurnID: "peer-turn", Content: "peer message"}); err != nil {
		t.Fatal(err)
	}
	if got := hook("pre-tool-use", `"toolCall":{"name":"mcp__agentbridge__bridge_read","args":{"peek":true}}`); got != fmt.Sprintf(`{"decision":"allow","overwrite":{"_conversation_id":%q}}`, conversation) {
		t.Fatalf("overwrite response %s", got)
	}
	got := hook("pre-invocation", fields)
	if !strings.Contains(got, `"injectSteps":[{"systemMessage":{"systemMessage":`) || !strings.Contains(got, "bridge_read") || strings.Contains(got, "peer message") {
		t.Fatalf("unread notice: %s", got)
	}
	for _, invocation := range []string{"0", "1", "2"} {
		if got := hook("pre-invocation", `"lastUserInput":"Question","initialNumSteps":5,"invocationNum":`+invocation); got != "{}" {
			t.Fatalf("invocation %s repeated the unread notice: %s", invocation, got)
		}
	}
	if got := hook("stop", `"terminationReason":"ERROR","lastUserInput":"Question"`); got != "{}" {
		t.Fatalf("abnormal Stop: %s", got)
	}
	check := func(count int) []blackboard.Entry {
		t.Helper()
		entries, err := h.Blackboard(room.ID)
		if err != nil || len(entries) != count {
			t.Fatalf("blackboard: %+v, %v; want %d entries", entries, err, count)
		}
		return entries
	}
	check(2)
	for i := 0; i < 2; i++ {
		if got := hook("stop", `"terminationReason":"NO_TOOL_CALL","lastUserInput":"Question"`); got != "{}" {
			t.Fatalf("Stop result: %s", got)
		}
	}
	entries := check(3)
	if entries[1].Role != "user" || entries[1].Content != "Question" || entries[2].Content != "Answer" ||
		entries[1].NativeTurnID != entries[2].NativeTurnID || len(entries[2].FilesTouched) != 1 || entries[2].FilesTouched[0] != "file.go" {
		t.Fatalf("collected turn: %+v", entries)
	}
	if got := hook("pre-invocation", `"lastUserInput":"Next question","initialNumSteps":8,"invocationNum":1`); !strings.Contains(got, "bridge_read") {
		t.Fatalf("next turn should be eligible for an unread notice: %s", got)
	}
	check(4)
	if err := h.DetachParticipant(room.ID, room.Participants[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := hook("pre-tool-use", `"toolCall":{"name":"bridge_read"}`); got != "{}" {
		t.Fatalf("detached hook result %s", got)
	}
	check(4)
}

func TestStdioProxy(t *testing.T) {
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := api.New(ctx, h, t.TempDir(), antigravity.New(t.TempDir()))
	server := httptest.NewServer(service.Handler("127.0.0.1"))
	defer func() {
		server.Close()
		cancel()
		service.Wait()
	}()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"tools/list","id":2}`,
	}, "\n")
	var out, stderr bytes.Buffer
	if err := stdio(nil, strings.NewReader(input), &out, &stderr, server.URL); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || stderr.Len() != 0 {
		t.Fatalf("stdio output %q, stderr %q", &out, &stderr)
	}
	for i, line := range lines {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal([]byte(line), &reply) != nil || reply.ID != i+1 || len(reply.Result) == 0 {
			t.Fatalf("invalid stdio response: %s", line)
		}
	}
}
