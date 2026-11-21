package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
)

const callerWindow = 10 * time.Minute
const callerTailSize = 128 << 10

var (
	ErrToolCallerNotFound  = errors.New("no recent in-flight Codex tool call in this workspace")
	ErrToolCallerAmbiguous = errors.New("multiple Codex sessions have a recent in-flight tool call in this workspace")
)

// IdentifyToolCaller identifies one session with an in-flight AgentBridge call
// carrying the request ID. Other pending calls to the same tool do not count.
func (a *Adapter) IdentifyToolCaller(workspace, toolName, requestID string) (string, error) {
	if workspace == "" || toolName == "" || requestID == "" {
		return "", ErrToolCallerNotFound
	}
	catalog, err := a.sessions(true)
	if err != nil {
		return "", err
	}

	now := time.Now()
	since := now.Add(-callerWindow)
	var candidates []sessionFile
	for _, files := range catalog {
		for _, file := range files {
			if !file.session.UpdatedAt.Before(since) && adapter.SameWorkspace(file.session.Workspace, workspace) {
				candidates = append(candidates, file)
			}
		}
	}
	slices.SortFunc(candidates, func(x, y sessionFile) int {
		return y.session.UpdatedAt.Compare(x.session.UpdatedAt)
	})

	var caller string
	for _, file := range candidates {
		matches, err := hasPendingCall(file, toolName, requestID, since, now)
		if err != nil {
			return "", err
		}
		if !matches {
			continue
		}
		if caller != "" && caller != file.session.ID {
			return "", ErrToolCallerAmbiguous
		}
		caller = file.session.ID
	}
	if caller == "" {
		return "", ErrToolCallerNotFound
	}
	return caller, nil
}

type callRecord struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Input     string          `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	Item      struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"item"`
	StartedAtMS int64 `json:"started_at_ms"`
}

func hasPendingCall(file sessionFile, toolName, requestID string, since, now time.Time) (bool, error) {
	f, err := os.Open(file.path)
	if err != nil {
		return false, fmt.Errorf("read candidate Codex rollout: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect candidate Codex rollout: %w", err)
	}
	if !os.SameFile(file.info, info) || info.Size() < file.info.Size() {
		return false, errors.New("candidate Codex rollout changed during caller identification")
	}
	length := min(int64(callerTailSize), info.Size())
	if length == 0 {
		return false, nil
	}
	raw := make([]byte, length)
	if _, err := f.ReadAt(raw, info.Size()-length); err != nil {
		return false, fmt.Errorf("read candidate Codex rollout tail: %w", err)
	}
	if length < info.Size() {
		_, raw, _ = bytes.Cut(raw, []byte("\n"))
	}
	lastLineEnd := bytes.LastIndexByte(raw, '\n')
	if lastLineEnd < 0 {
		return false, nil
	}
	return pendingCallInTail(raw[:lastLineEnd], toolName, requestID, since, now), nil
}

// The Codex desktop executable tools mode persists an exec call before nested
// MCP dispatch. Recognize a leading awaited invocation rather than searching
// comments, quoted source or arbitrary tool output for the name.
var directExecCall = regexp.MustCompile(`^\s*(?:(?:const|let|var)\s+[A-Za-z_$][\w$]*\s*=\s*|(?:text|notify)\(\s*|return\s+)?await\s+tools\.(mcp__agentbridge__[a-z_]+)\s*\(`)

func requestIDInArguments(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return ""
		}
		raw = []byte(encoded)
	}
	var args struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	return args.RequestID
}

func execRequestID(input, directName string) string {
	match := directExecCall.FindStringSubmatchIndex(input)
	if len(match) != 4 || input[match[2]:match[3]] != directName {
		return ""
	}
	args := input[match[1]:]
	decoder := json.NewDecoder(strings.NewReader(args))
	var raw json.RawMessage
	if decoder.Decode(&raw) != nil || len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	// JSON is also a JS object literal. Verify that it is the sole argument
	// rather than part of a larger expression or a JavaScript spread.
	if !strings.HasPrefix(strings.TrimSpace(args[decoder.InputOffset():]), ")") {
		return ""
	}
	return requestIDInArguments(raw)
}

func pendingCallInTail(raw []byte, toolName, requestID string, since, now time.Time) bool {
	directName := "mcp__agentbridge__" + toolName
	type invocation struct {
		start     time.Time
		wrapped   bool
		requestID string
	}
	pending := make(map[string]invocation)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var record envelope
		if json.Unmarshal(line, &record) != nil || record.Metadata.Inherited {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
		if err != nil || at.Before(since) || at.After(now.Add(time.Second)) {
			continue
		}
		var call callRecord
		if json.Unmarshal(record.Payload, &call) != nil {
			continue
		}
		switch record.Type {
		case "response_item":
			switch call.Type {
			case "function_call":
				if call.CallID != "" && (call.Name == directName ||
					call.Name == toolName && slices.Contains([]string{"agentbridge", "mcp__agentbridge", "mcp__agentbridge__"}, call.Namespace)) {
					if id := requestIDInArguments(call.Arguments); id != "" {
						pending[call.CallID] = invocation{start: at, requestID: id}
					}
				}
			case "custom_tool_call":
				if call.Name == "exec" && call.CallID != "" {
					if id := execRequestID(call.Input, directName); id != "" {
						pending[call.CallID] = invocation{start: at, wrapped: true, requestID: id}
					}
				}
			case "function_call_output", "custom_tool_call_output":
				delete(pending, call.CallID)
			}
		case "event_msg":
			if !strings.EqualFold(call.Item.Type, "McpToolCall") || call.Item.Server != "agentbridge" || call.Item.Tool != toolName {
				continue
			}
			switch call.Type {
			case "item_started":
				if call.Item.ID != "" {
					if id := requestIDInArguments(call.Item.Arguments); id != "" {
						pending[call.Item.ID] = invocation{start: at, requestID: id}
					}
				}
			case "item_completed":
				delete(pending, call.Item.ID)
				// A desktop MCP item has a different ID from its enclosing
				// exec call. Associate it with the latest matching wrapper
				// that started before the nested call.
				if call.StartedAtMS != 0 {
					at = time.UnixMilli(call.StartedAtMS).Add(time.Millisecond)
				}
				var wrappedID string
				var newest time.Time
				completedRequest := requestIDInArguments(call.Item.Arguments)
				for id, candidate := range pending {
					if candidate.wrapped && !candidate.start.After(at) &&
						(completedRequest == "" || completedRequest == candidate.requestID) &&
						(candidate.start.After(newest) || candidate.start.Equal(newest) && id > wrappedID) {
						newest, wrappedID = candidate.start, id
					}
				}
				delete(pending, wrappedID)
			}
		}
	}
	for _, call := range pending {
		if call.requestID == requestID {
			return true
		}
	}
	return false
}
