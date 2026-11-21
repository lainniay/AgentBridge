package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/api"
)

var hookClient = httpClient(250*time.Millisecond, 30*time.Millisecond)

func runHook(in io.Reader, out io.Writer, endpoint, event string) {
	_, _ = out.Write(append(hookResponse(in, endpoint, event), '\n'))
}

func hookResponse(in io.Reader, endpoint, event string) []byte {
	empty := []byte("{}")
	switch event {
	case "pre-tool-use", "pre-invocation", "stop":
	default:
		return empty
	}
	ctx, cancel := context.WithTimeout(context.Background(), 275*time.Millisecond)
	defer cancel()
	type bodyResult struct {
		raw []byte
		err error
	}
	body := make(chan bodyResult, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(in, (4<<20)+1))
		body <- bodyResult{raw, err}
	}()
	var raw []byte
	select {
	case <-ctx.Done():
		return empty
	case result := <-body:
		if result.err != nil || len(result.raw) > 4<<20 {
			return empty
		}
		raw = result.raw
	}
	var input antigravity.HookInput
	if json.Unmarshal(raw, &input) != nil || input.ConversationID == "" {
		return empty
	}
	if (event == "pre-tool-use" && !api.IsBridgeToolCall(input.ToolCall.Name, input.ToolCall.Args)) ||
		(event == "pre-invocation" && !input.IsInitialInvocation()) ||
		(event == "stop" && input.TerminationReason != "NO_TOOL_CALL") {
		return empty
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/hooks/antigravity?event="+event, bytes.NewReader(raw))
	if err != nil {
		return empty
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hookClient.Do(req)
	if err != nil {
		return empty
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || event == "stop" {
		return empty
	}
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	var obj map[string]json.RawMessage
	if err != nil || json.Unmarshal(reply, &obj) != nil || obj == nil {
		return empty
	}
	result, err := json.Marshal(obj)
	if err != nil {
		return empty
	}
	return result
}
