package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/adapter/codex"
	"github.com/lainniay/AgentBridge/internal/api"
	"github.com/lainniay/AgentBridge/internal/blackboard"
	"github.com/lainniay/AgentBridge/internal/hub"
)

func TestControlPlaneLiveCaptureAfterRestart(t *testing.T) {
	workspace, stateDir, logs := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(logs, "rollout-test.jsonl")
	exactWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"thread","cwd":%q}}`+"\n", exactWorkspace)
	turn := func(id string, timestamp time.Time) string {
		var lines bytes.Buffer
		for _, event := range []string{
			fmt.Sprintf(`{"type":"task_started","turn_id":%q}`, id),
			fmt.Sprintf(`{"type":"user_message","turn_id":%q,"message":%q}`, id, id+" question"),
			fmt.Sprintf(`{"type":"task_complete","turn_id":%q,"last_agent_message":%q}`, id, id+" answer"),
		} {
			fmt.Fprintf(&lines, `{"timestamp":%q,"type":"event_msg","payload":%s}`+"\n", timestamp.Format(time.RFC3339Nano), event)
		}
		return lines.String()
	}
	if err := os.WriteFile(path, []byte(meta+turn("old", time.Now().Add(-time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}

	agy := antigravity.New(t.TempDir())
	cx := codex.New(logs)
	start := func() (*httptest.Server, func()) {
		t.Helper()
		h, err := hub.Open(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		s := api.New(ctx, h, workspace, agy, cx)
		server := httptest.NewServer(s.Handler("127.0.0.1"))
		return server, func() {
			server.Close()
			cancel()
			s.Wait()
		}
	}
	server, stop := start()
	defer func() { stop() }()
	do := func(method, path string, body any, target any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var result map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&result)
			t.Fatalf("%s %s: %s %#v", method, path, resp.Status, result)
		}
		if target != nil {
			if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
				t.Fatal(err)
			}
		}
	}
	q := "?workspace=" + url.QueryEscape(workspace)
	var discovery struct {
		Sessions []adapter.Session `json:"sessions"`
	}
	do(http.MethodGet, "/api/sessions"+q, nil, &discovery)
	if len(discovery.Sessions) != 1 || discovery.Sessions[0].ID != "thread" {
		t.Fatalf("discovery: %#v", discovery)
	}
	var room hub.Room
	do(http.MethodPost, "/api/rooms/bind", map[string]any{
		"workspace":    workspace,
		"participants": []map[string]string{{"provider": "codex", "session_id": "thread"}},
	}, &room)
	boardPath := "/api/rooms/" + room.ID + "/blackboard"

	appendTurn := func(id string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(turn(id, time.Now()))
		if closeErr := f.Close(); err != nil || closeErr != nil {
			t.Fatalf("append: %v, close: %v", err, closeErr)
		}
	}
	read := func() []blackboard.Entry {
		t.Helper()
		var board struct {
			Entries []blackboard.Entry `json:"entries"`
		}
		do(http.MethodGet, boardPath, nil, &board)
		return board.Entries
	}
	wait := func(turns ...string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			entries := read()
			if len(entries) == 2*len(turns) {
				for i, id := range turns {
					if entries[2*i].NativeTurnID != id || entries[2*i].Role != "user" ||
						entries[2*i+1].NativeTurnID != id || entries[2*i+1].Role != "assistant" {
						t.Fatalf("wrong entries after %v: %+v", turns, entries)
					}
				}
				return
			}
			if len(entries) > 2*len(turns) || time.Now().After(deadline) {
				t.Fatalf("expected %v, got %+v", turns, entries)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	appendTurn("live")
	wait("live")

	stop()
	appendTurn("while-offline")
	server, stop = start()
	wait("live", "while-offline")

	do(http.MethodPost, "/api/rooms/detach", map[string]string{
		"room_id": room.ID, "participant_id": room.Participants[0].ID,
	}, nil)
	appendTurn("after-detach")
	time.Sleep(300 * time.Millisecond)
	wait("live", "while-offline")
}

func TestRejectCrossOriginRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := api.New(ctx, h, t.TempDir(), antigravity.New(t.TempDir()))
	handler := s.Handler("127.0.0.1")
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:4319", "", 200},
		{"localhost:4319", "http://localhost:4319", 200},
		{"127.0.0.1:4319", "https://outside.example", 403},
		{"outside.example:4319", "", 403},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/healthz", nil)
		req.Header.Set("Origin", tc.origin)
		got := httptest.NewRecorder()
		handler.ServeHTTP(got, req)
		if got.Code != tc.want {
			t.Fatalf("Host %s, Origin %s: got %d, want %d", tc.host, tc.origin, got.Code, tc.want)
		}
	}
}

func TestBindAntigravityBeforeTranscript(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const session = "b47a7812-2b55-48b9-a5f5-580efff71c11"
	workspace, other := t.TempDir(), t.TempDir()
	agy := antigravity.New(filepath.Join(t.TempDir(), "brain"))
	service := api.New(ctx, h, workspace, agy)
	handler := service.Handler("127.0.0.1")
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4319"+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	bind := func(provider, id string, backfill int) *httptest.ResponseRecorder {
		t.Helper()
		return post("/api/rooms/bind", fmt.Sprintf(`{"workspace":%q,"backfill_turns":%d,"participants":[{"provider":%q,"session_id":%q}]}`,
			workspace, backfill, provider, id))
	}
	for _, w := range []*httptest.ResponseRecorder{
		bind("antigravity", session, 1), bind("antigravity", "../invalid", 0), bind("codex", session, 0),
	} {
		if w.Code < 400 {
			t.Fatalf("unexpected unavailable binding: %d %s", w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agent")); !os.IsNotExist(err) {
		t.Fatalf("rejected requests left workspace state: %v", err)
	}
	w := bind("antigravity", session, 0)
	if w.Code != http.StatusOK {
		t.Fatalf("pending binding: %d %s", w.Code, w.Body)
	}
	var room hub.Room
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	hook := func(root string) *httptest.ResponseRecorder {
		return post("/api/hooks/antigravity", fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"hookEventName":"PreInvocation","invocationNum":1,"initialNumSteps":1,"lastUserInput":"First prompt"}`, session, root))
	}
	for _, tc := range []struct {
		root string
		want int
	}{{other, 0}, {workspace, 1}} {
		if w := hook(tc.root); w.Code != http.StatusOK || w.Body.String() != "{}\n" {
			t.Fatalf("pending hook: %d %s", w.Code, w.Body)
		}
		entries, err := h.Blackboard(room.ID)
		if err != nil || len(entries) != tc.want {
			t.Fatalf("workspace %q: got %d entries, want %d, error %v", tc.root, len(entries), tc.want, err)
		}
		if tc.want != 0 && entries[0].Content != "First prompt" {
			t.Fatalf("unexpected first prompt: %+v", entries[0])
		}
	}
}

func TestPreToolUseOverwriteDirectAndCallMCPTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	const conv = "6a5c63de-e606-4982-8042-8c4a87f9e867"
	if _, err := h.BindRoom(workspace, []hub.Binding{{Provider: "antigravity", SessionID: conv}}); err != nil {
		t.Fatal(err)
	}
	service := api.New(ctx, h, workspace, antigravity.New(t.TempDir()))
	handler := service.Handler("127.0.0.1")

	callHook := func(toolCall string) map[string]any {
		t.Helper()
		body := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"hookEventName":"PreToolUse","toolCall":%s}`, conv, workspace, toolCall)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4319/api/hooks/antigravity?event=pre-tool-use", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("hook status %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 1. Direct bridge tool call
	direct := callHook(`{"name":"mcp__agentbridge__bridge_status","args":{}}`)
	if direct["decision"] != "allow" {
		t.Fatalf("direct tool decision: %+v", direct)
	}
	if ow := direct["overwrite"].(map[string]any); ow["_conversation_id"] != conv {
		t.Fatalf("direct overwrite: %+v", ow)
	}

	// 2. Lazy-loaded call_mcp_tool preserving existing Arguments fields
	lazy := callHook(`{"name":"call_mcp_tool","args":{"ServerName":"agentbridge_agentbridge","ToolName":"bridge_read","Arguments":{"mode":"diff","peek":true}}}`)
	if lazy["decision"] != "allow" {
		t.Fatalf("lazy tool decision: %+v", lazy)
	}
	argsMap := lazy["overwrite"].(map[string]any)["Arguments"].(map[string]any)
	if argsMap["_conversation_id"] != conv || argsMap["mode"] != "diff" || argsMap["peek"] != true {
		t.Fatalf("lazy overwrite arguments: %+v", argsMap)
	}

	// 3. Unrelated MCP server via call_mcp_tool should be ignored
	other := callHook(`{"name":"call_mcp_tool","args":{"ServerName":"github","ToolName":"get_issue","Arguments":{}}}`)
	if len(other) != 0 {
		t.Fatalf("unrelated MCP tool should return empty object, got: %+v", other)
	}
}
