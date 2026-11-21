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

	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/adapter/codex"
	"github.com/lainniay/AgentBridge/internal/api"
	"github.com/lainniay/AgentBridge/internal/hub"
)

func TestManageMCP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := hub.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, otherWorkspace := t.TempDir(), t.TempDir()
	agyRoot, codexRoot := t.TempDir(), t.TempDir()
	const conversation = "02b69dbc-8f33-4209-909a-36d03bfbfe24"
	transcript := filepath.Join(agyRoot, conversation, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"USER_INPUT","source":"USER_EXPLICIT","step_index":0,"content":"Antigravity question"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logFile := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"peer","cwd":%q}}`+"\n", workspace) +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"example"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"example","message":"  Codex question  "}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"example","last_agent_message":"Answer"}}` + "\n"
	rollout := filepath.Join(codexRoot, "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte(logFile), 0600); err != nil {
		t.Fatal(err)
	}

	service := api.New(ctx, h, otherWorkspace, antigravity.New(agyRoot), codex.New(codexRoot))
	defer func() { cancel(); service.Wait() }()
	handler := service.Handler("127.0.0.1")
	post := func(path, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4319"+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var response map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("unexpected HTTP response: %d %s", w.Code, w.Body)
		}
		return response
	}
	list := post("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 || tools[2].(map[string]any)["name"] != "bridge_manage" {
		t.Fatalf("tool discovery: %+v", tools)
	}
	hook := func(toolCall string) map[string]any {
		t.Helper()
		return post("/api/hooks/antigravity", fmt.Sprintf(
			`{"conversationId":%q,"workspacePaths":[%q],"hookEventName":"PreToolUse","toolCall":%s}`, conversation, workspace, toolCall))
	}
	// Unbound passive calls retain the no-op behavior.
	if response := hook(`{"name":"mcp__agentbridge__bridge_read","args":{}}`); len(response) != 0 {
		t.Fatalf("unbound passive overwrite: %+v", response)
	}
	direct := hook(`{"name":"mcp__agentbridge__bridge_manage","args":{"action":"list"}}`)
	overwrite := direct["overwrite"].(map[string]any)
	if direct["decision"] != "allow" || overwrite["_conversation_id"] != conversation || overwrite["_workspace"] != workspace {
		t.Fatalf("direct management overwrite: %+v", direct)
	}
	lazy := hook(`{"name":"call_mcp_tool","args":{"ServerName":"agentbridge_agentbridge","ToolName":"bridge_manage","Arguments":{"action":"bind","target_session_id":"latest","backfill_turns":1}}}`)
	args := lazy["overwrite"].(map[string]any)["Arguments"].(map[string]any)
	if lazy["decision"] != "allow" || args["_conversation_id"] != conversation || args["_workspace"] != workspace ||
		args["action"] != "bind" || args["target_session_id"] != "latest" || args["backfill_turns"] != float64(1) {
		t.Fatalf("lazy management overwrite: %+v", lazy)
	}
	call := func(path string, arguments map[string]any, wantError bool) map[string]any {
		t.Helper()
		data, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		response := post(path, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bridge_manage","arguments":%s}}`, data))
		result := response["result"].(map[string]any)
		if (result["isError"] == true) != wantError {
			t.Fatalf("management tool response: %+v", response)
		}
		if wantError {
			return result
		}
		return result["structuredContent"].(map[string]any)
	}

	agyURL := "/mcp?provider=antigravity&require_session=true"
	cxURL := "/mcp?provider=codex&workspace=" + url.QueryEscape(workspace)
	noIdentity := call(cxURL, map[string]any{"action": "list", "target_provider": "antigravity"}, false)
	if bound, present := noIdentity["bound"]; !present || bound != nil || noIdentity["self"] != nil ||
		noIdentity["identity_error"] == nil || len(noIdentity["candidates"].([]any)) != 1 {
		t.Fatalf("identity-free list: %+v", noIdentity)
	}
	call(cxURL, map[string]any{"action": "bind", "target_session_id": conversation}, true)
	overwrite["action"] = "list"
	info := call(agyURL, overwrite, false)
	peers := info["candidates"].([]any)
	if info["bound"] != false || info["self"].(map[string]any)["session_id"] != conversation || len(peers) != 1 {
		t.Fatalf("candidate list: %+v", info)
	}
	peer := peers[0].(map[string]any)
	if peer["session_id"] != "peer" || peer["title"] != "Codex question" || peer["already_bound"] != false {
		t.Fatalf("peer: %+v", peer)
	}
	bound := call(agyURL, args, false)
	data, _ := json.Marshal(bound["room"])
	var room hub.Room
	if err := json.Unmarshal(data, &room); err != nil || bound["bound"] != true || len(room.Participants) != 2 {
		t.Fatalf("binding response: %+v", bound)
	}
	entries, err := h.Blackboard(room.ID)
	if err != nil || len(entries) != 3 {
		t.Fatalf("backfill: %+v, %v", entries, err)
	}
	cx := call(cxURL+"&session_id=peer", map[string]any{"action": "list"}, false)
	if cx["bound"] != true || cx["candidates"].([]any)[0].(map[string]any)["already_bound"] != true {
		t.Fatalf("reverse status: %+v", cx)
	}
	explicit := call(cxURL, map[string]any{"action": "list", "caller_session_id": "peer"}, false)
	if explicit["bound"] != true || explicit["self"].(map[string]any)["session_id"] != "peer" {
		t.Fatalf("explicit host identity: %+v", explicit)
	}
	f, err := os.OpenFile(rollout, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "e5a2501c-f400-4424-802b-f2d942aa10fe"
	_, writeErr := fmt.Fprintf(f, `{"type":"response_item","timestamp":%q,"payload":{"type":"function_call","name":"mcp__agentbridge__bridge_manage","call_id":"unbind","arguments":{"action":"unbind","request_id":%q}}}`+"\n",
		time.Now().Add(-time.Minute).Format(time.RFC3339Nano), requestID)
	if closeErr := f.Close(); writeErr != nil || closeErr != nil {
		t.Fatalf("append native call: %v, %v", writeErr, closeErr)
	}
	call(cxURL, map[string]any{"action": "unbind"}, true)
	call(cxURL, map[string]any{"action": "unbind", "request_id": "5bbef103-97d6-4f9c-972f-cc5dc5258634"}, true)
	if _, _, stillBound := h.Resolve("", "codex", "peer"); !stillBound {
		t.Fatal("uncorrelated call removed the binding")
	}
	if result := call(cxURL, map[string]any{"action": "unbind", "request_id": requestID}, false); result["unbound"] != true {
		t.Fatalf("unbind response: %+v", result)
	}
	for _, p := range room.Participants {
		if _, _, bound := h.Resolve("", p.Provider, p.SessionID); bound {
			t.Fatalf("isolated participant left bound: %+v", p)
		}
	}
	after, err := h.Blackboard(room.ID)
	if err != nil || len(after) != len(entries) {
		t.Fatalf("unbind should preserve archived history: %+v, %v", after, err)
	}
}
