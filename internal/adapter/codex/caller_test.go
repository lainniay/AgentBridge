package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTitleFallbackAndTrimWhitespace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	header := `{"type":"session_meta","payload":{"id":"session","cwd":"/project/example"}}` + "\n"
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(root)
	sessions, err := a.ListSessions("/project/example")
	if err != nil || len(sessions) != 1 || sessions[0].Title != "" {
		t.Fatalf("catalog before first prompt: %+v, %v", sessions, err)
	}
	promptTail := strings.Repeat("问", 40)
	prompt := "   首次提问\n\t  " + promptTail + "\n\n "
	data := header + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>not a prompt"}]}}` + "\n" +
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":%q}}`+"\n", prompt) +
		`{"type":"event_msg","payload":{"type":"user_message","message":"A later prompt"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err = a.ListSessions("/project/example")
	expectedTitle := "首次提问 " + strings.Repeat("问", 35)
	if err != nil || len(sessions) != 1 || sessions[0].Title != expectedTitle {
		t.Fatalf("catalog after first prompt: %+v, %v", sessions, err)
	}
	entries, err := a.ReadHistory("session", 0)
	if err != nil || len(entries) != 1 || entries[0].Content != strings.TrimSpace(prompt) {
		t.Fatalf("trimmed entries: %+v, %v", entries, err)
	}
}

func TestIdentifyToolCaller(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	a := New(root)
	const workspace = "/project/example"
	const tool = "bridge_manage"
	const requestID = "request-one"
	write := func(id, cwd string, records ...string) {
		t.Helper()
		record := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`+"\n", id, cwd)
		path := filepath.Join(root, id+".jsonl")
		if err := os.WriteFile(path, []byte(record+strings.Join(records, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(typ string, at time.Time, payload string) string {
		return fmt.Sprintf(`{"type":%q,"timestamp":%q,"payload":%s}`+"\n", typ, at.Format(time.RFC3339Nano), payload)
	}
	function := entry("response_item", now, `{"type":"function_call","name":"mcp__agentbridge__bridge_manage","call_id":"call-1","arguments":"{\"request_id\":\"request-one\"}"}`)
	wrong := entry("response_item", now, `{"type":"function_call","name":"mcp__other__bridge_manage","call_id":"unrelated","arguments":{"request_id":"request-one"}}`)
	copied := entry("response_item", now, `{"type":"message","role":"user","content":"mcp__agentbridge__bridge_manage("}`)
	js := entry("response_item", now.Add(-4*time.Minute), `{"type":"custom_tool_call","name":"exec","call_id":"exec-1","input":"const result = await tools.mcp__agentbridge__bridge_manage({\"action\":\"list\",\"request_id\":\"request-one\"}); text(result);"}`)
	comment := entry("response_item", now, `{"type":"custom_tool_call","name":"exec","call_id":"exec-2","input":"// await tools.mcp__agentbridge__bridge_manage({\"request_id\":\"request-one\"})"}`)
	noID := entry("response_item", now, `{"type":"function_call","name":"mcp__agentbridge__bridge_manage","call_id":"missing","arguments":{"action":"list"}}`)
	otherID := entry("response_item", now, `{"type":"function_call","name":"mcp__agentbridge__bridge_manage","call_id":"different","arguments":{"request_id":"request-two"}}`)
	starter := entry("event_msg", now, `{"type":"item_started","item":{"type":"McpToolCall","id":"started","server":"agentbridge","tool":"bridge_manage","arguments":{"request_id":"request-one"}}}`)
	completed := entry("event_msg", now, `{"type":"item_completed","item":{"type":"McpToolCall","id":"other-id","server":"agentbridge","tool":"bridge_manage"}}`)
	completedStarter := entry("event_msg", now, `{"type":"item_completed","item":{"type":"McpToolCall","id":"started","server":"agentbridge","tool":"bridge_manage","arguments":{"request_id":"request-one"}}}`)
	output := entry("response_item", now, `{"type":"function_call_output","call_id":"call-1"}`)
	stale := entry("response_item", now.Add(-callerWindow-time.Second), `{"type":"function_call","name":"mcp__agentbridge__bridge_manage","call_id":"stale","arguments":{"request_id":"request-one"}}`)
	check := func(want string, wantErr error) {
		t.Helper()
		got, err := a.IdentifyToolCaller(workspace, tool, requestID)
		if got != want || !errors.Is(err, wantErr) {
			t.Fatalf("caller = %q, %v; want %q, %v", got, err, want, wantErr)
		}
	}
	write("foreign", "/project/elsewhere", function)
	write("non-call", workspace, stale, wrong, copied, comment, noID)
	write("wrong-request", workspace, otherID)
	write("completed", workspace, function, output)
	write("completed-desktop", workspace, js, completed)
	write("completed-item", workspace, starter, completedStarter)
	check("", ErrToolCallerNotFound)

	write("desktop", workspace, js)
	check("desktop", nil)

	write("cli", workspace, function)
	check("", ErrToolCallerAmbiguous)
	write("desktop", workspace, js, completed)
	check("cli", nil)
	if got, err := a.IdentifyToolCaller(workspace, tool, ""); got != "" || !errors.Is(err, ErrToolCallerNotFound) {
		t.Fatalf("caller without request ID = %q, %v", got, err)
	}
	write("cli", workspace, starter)
	check("cli", nil)
}

func TestExecRequestIDRequiresLiteralArguments(t *testing.T) {
	const name = "mcp__agentbridge__bridge_manage"
	for _, input := range []string{
		`await tools.mcp__agentbridge__bridge_manage({"request_id":"real"}, addition)`,
		`await tools.mcp__agentbridge__bridge_manage({"request_id":"real"} || fallback)`,
		`await tools.mcp__agentbridge__bridge_manage(variable)`,
		`await tools.mcp__agentbridge__bridge_manage({"nested":{"request_id":"real"}})`,
		`text("await tools.mcp__agentbridge__bridge_manage({})")`,
	} {
		if id := execRequestID(input, name); id != "" {
			t.Errorf("unexpected request ID %q in %s", id, input)
		}
	}
	if id := execRequestID(`text(await tools.mcp__agentbridge__bridge_manage( {"request_id":"real"} ));`, name); id != "real" {
		t.Fatalf("inline request ID = %q", id)
	}
}
