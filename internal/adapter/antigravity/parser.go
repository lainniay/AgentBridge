package antigravity

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type step struct {
	Index     *int   `json:"step_index"`
	Type      string `json:"type"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
}

func decodeStep(line []byte) (step, bool) {
	var s step
	if json.Unmarshal(line, &s) != nil {
		return step{}, false
	}
	switch s.Type {
	case "USER_INPUT", "PLANNER_RESPONSE":
		return s, true
	default:
		return step{}, false
	}
}

func (s step) user() bool {
	return s.Type == "USER_INPUT" && (s.Source == "" || s.Source == "USER_EXPLICIT")
}

func stepID(index int) string { return "step:" + strconv.Itoa(index) }

type turn struct {
	user      blackboard.Entry
	assistant blackboard.Entry
	files     map[string]struct{}
	lastIndex *int
}

func unwrapUserContent(content string) string {
	trimmed := strings.TrimSpace(content)
	const openTag = "<USER_REQUEST>"
	const closeTag = "</USER_REQUEST>"
	if !strings.HasPrefix(trimmed, openTag) {
		return content
	}
	rest := trimmed[len(openTag):]
	end := strings.LastIndex(rest, closeTag)
	if end < 0 {
		return content
	}
	return strings.TrimSpace(rest[:end])
}

func newTurn(s step, offset int64) turn {
	id := "offset:" + strconv.FormatInt(offset, 10)
	if s.Index != nil {
		id = stepID(*s.Index)
	}
	ts, _ := time.Parse(time.RFC3339Nano, s.CreatedAt)
	return turn{user: blackboard.Entry{
		NativeTurnID: id, Provider: "antigravity", Role: "user", TS: ts, Content: unwrapUserContent(s.Content),
	}}
}

func (t *turn) accept(s step) {
	if t.user.NativeTurnID == "" || s.Type != "PLANNER_RESPONSE" || (s.Source != "" && s.Source != "MODEL") {
		return
	}
	// Only the last planner step can terminate the turn.
	t.assistant = blackboard.Entry{}
	if s.Status != "" && s.Status != "DONE" {
		return
	}
	if len(s.ToolCalls) > 0 {
		for _, call := range s.ToolCalls {
			t.noteEdits(call.Name, call.Args)
		}
		return
	}
	if strings.TrimSpace(s.Content) != "" {
		ts, _ := time.Parse(time.RFC3339Nano, s.CreatedAt)
		t.assistant = blackboard.Entry{
			NativeTurnID: t.user.NativeTurnID, Provider: "antigravity",
			Role: "assistant", TS: ts, Content: s.Content,
		}
	}
}

func (t *turn) final() blackboard.Entry {
	result := t.assistant
	if result.NativeTurnID != "" {
		for path := range t.files {
			result.FilesTouched = append(result.FilesTouched, path)
		}
		slices.Sort(result.FilesTouched)
	}
	return result
}

func (t *turn) noteEdits(name string, raw json.RawMessage) {
	switch strings.ToLower(name) {
	case "write_file", "edit_file", "replace_content", "replace_file_content", "multi_replace_file_content", "create_file", "delete_file", "move_file", "rename_file", "apply_patch":
	default:
		return
	}
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return
		}
		raw = []byte(encoded)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return
	}
	for key, value := range args {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "path", "filepath", "targetfile", "targetfilepath", "sourcepath", "destinationpath", "oldpath", "newpath":
			var path string
			if json.Unmarshal(value, &path) == nil && strings.TrimSpace(path) != "" {
				if t.files == nil {
					t.files = make(map[string]struct{})
				}
				t.files[filepath.Clean(path)] = struct{}{}
			}
		}
	}
}
