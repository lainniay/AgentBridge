package mcp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lainniay/AgentBridge/internal/blackboard"
	"github.com/lainniay/AgentBridge/internal/hub"
	"github.com/lainniay/AgentBridge/internal/mcp"
)

func TestStreamableHTTP(t *testing.T) {
	homeDir := t.TempDir()
	h, err := hub.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.Server{Hub: h, Workspace: workspace})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := server.Client()
	post := func(query, body string, status int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp"+query, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("HTTP %d, want %d", res.StatusCode, status)
		}
		if status == http.StatusAccepted {
			return nil
		}
		var reply map[string]any
		if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if reply["jsonrpc"] != "2.0" || reply["error"] != nil {
			t.Fatalf("unexpected JSON-RPC response: %#v", reply)
		}
		return reply["result"].(map[string]any)
	}
	init := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`, 200)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol negotiation: %#v", init)
	}
	post("", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202)
	list := post("", `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`, 200)
	tools := list["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "bridge_status" || tools[1].(map[string]any)["name"] != "bridge_read" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	call := func(query, tool, args string) map[string]any {
		t.Helper()
		reply := post(query, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args), 200)
		if reply["isError"] == true || len(reply["content"].([]any)) != 1 {
			t.Fatalf("unexpected tool result: %#v", reply)
		}
		return reply["structuredContent"].(map[string]any)
	}
	if status := call("", "bridge_status", "{}"); len(status) != 1 || status["bound"] != false {
		t.Fatalf("unbound status: %#v", status)
	}
	room, err := h.BindRoom(workspace, []hub.Binding{
		{Provider: "antigravity", SessionID: "conversation"},
		{Provider: "codex", SessionID: "thread"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range room.Participants {
		if err := h.IngestEntry(room.ID, p.ID, blackboard.Entry{
			Role: "assistant", NativeTurnID: fmt.Sprint(i), Content: p.Provider,
		}); err != nil {
			t.Fatal(err)
		}
	}
	query := "?provider=codex&workspace=" + url.QueryEscape(workspace) + "&session_id=thread"
	if status := call(query, "bridge_status", `{"_conversation_id":"conversation"}`); status["participant"].(map[string]any)["provider"] != "antigravity" {
		t.Fatalf("hook identity precedence: %#v", status)
	}
	if status := call(query, "bridge_status", `{"_conversation_id":"unknown"}`); status["bound"] != false {
		t.Fatalf("unknown explicit identity must stay unbound: %#v", status)
	}
	if status := call("", "bridge_status", "{}"); status["bound"] != false {
		t.Fatalf("ambiguous workspace fallback: %#v", status)
	}
	fallback := "?provider=codex&workspace=" + url.QueryEscape(workspace)
	if status := call(fallback+"&require_session=true", "bridge_status", "{}"); status["bound"] != false {
		t.Fatalf("missing required native identity: %#v", status)
	}
	if status := call(fallback+"&require_session=true", "bridge_status", `{"_session_id":"thread"}`); status["bound"] != true {
		t.Fatalf("injected session identity: %#v", status)
	}
	for _, args := range []string{`{"peek":true}`, "{}"} {
		got := call(fallback, "bridge_read", args)
		entries := got["entries"].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["content"] != "antigravity" {
			t.Fatalf("diff read: %#v", got)
		}
	}
	if got := call(query, "bridge_read", "{}"); got["bound"] != true || got["entries"] != nil && len(got["entries"].([]any)) != 0 {
		t.Fatalf("consumed diff: %#v", got)
	}
	if got := call(query, "bridge_read", `{"mode":"full","limit":1}`); got["entries"].([]any)[0].(map[string]any)["content"] != "codex" || got["has_more"] != true || got["latest_seq"] != float64(2) {
		t.Fatalf("full read: %#v", got)
	}
	if got := call(query, "bridge_read", `{"mode":"full","before_seq":2,"limit":1}`); got["entries"].([]any)[0].(map[string]any)["content"] != "antigravity" {
		t.Fatalf("before_seq read: %#v", got)
	}
	entry0 := call(query, "bridge_read", `{"mode":"full","after_seq":1,"limit":1}`)["entries"].([]any)[0].(map[string]any)
	if entry0["content"] != "codex" || entry0["room_id"] != nil || entry0["participant_id"] != nil || entry0["native_turn_id"] != nil {
		t.Fatalf("expected compact entry without redundant IDs: %#v", entry0)
	}
	if got := call(query, "bridge_read", `{"mode":"path"}`); got["path"] != filepath.Join(homeDir, "rooms", room.ID+".md") || got["room_id"] != room.ID {
		t.Fatalf("path read: %#v", got)
	}
}

func TestReadPaginationInProcess(t *testing.T) {
	homeDir := t.TempDir()
	h, err := hub.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	room, err := h.BindRoom(workspace, []hub.Binding{
		{Provider: "antigravity", SessionID: "c1"},
		{Provider: "codex", SessionID: "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		p := room.Participants[i%2]
		if err := h.IngestEntry(room.ID, p.ID, blackboard.Entry{
			Role: "assistant", NativeTurnID: fmt.Sprintf("turn-%d", i), Content: fmt.Sprintf("msg-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := mcp.Server{Hub: h, Workspace: workspace}
	call := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp?provider=antigravity", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var reply map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply["result"].(map[string]any)["structuredContent"].(map[string]any)
	}
	got := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bridge_read","arguments":{"_conversation_id":"c1","mode":"full","after_seq":1,"limit":2}}}`)
	entries := got["entries"].([]any)
	if len(entries) != 2 || entries[0].(map[string]any)["seq"] != float64(2) || entries[1].(map[string]any)["seq"] != float64(3) || got["has_more"] != true {
		t.Fatalf("forward pagination: %#v", got)
	}
	pathRes := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bridge_read","arguments":{"_conversation_id":"c1","mode":"path"}}}`)
	if pathRes["path"] != filepath.Join(homeDir, "rooms", room.ID+".md") {
		t.Fatalf("path: %#v", pathRes)
	}
}
