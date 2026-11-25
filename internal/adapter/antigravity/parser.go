package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

const maxPlanBytes = 16 << 10

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

type planArtifact struct {
	path    string
	content string
}

type turn struct {
	user      blackboard.Entry
	assistant blackboard.Entry
	files     map[string]struct{}
	plans     []planArtifact
	lastTS    time.Time
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
	if ts, err := time.Parse(time.RFC3339Nano, s.CreatedAt); err == nil {
		t.lastTS = ts
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
	planText := t.renderPlans(result.Content)
	if planText != "" {
		if result.NativeTurnID == "" {
			result = blackboard.Entry{
				NativeTurnID: t.user.NativeTurnID,
				Provider:     "antigravity",
				Role:         "assistant",
				TS:           t.lastTS,
				Content:      planText,
			}
		} else {
			result.Content = strings.TrimSpace(result.Content) + "\n\n" + planText
		}
	}
	if result.NativeTurnID != "" {
		for path := range t.files {
			result.FilesTouched = append(result.FilesTouched, path)
		}
		slices.Sort(result.FilesTouched)
	}
	return result
}

func (t *turn) renderPlans(existing string) string {
	if len(t.plans) == 0 {
		return ""
	}
	var sections []string
	seen := make(map[string]struct{}, len(t.plans))
	for _, p := range t.plans {
		if _, dup := seen[p.path]; dup {
			continue
		}
		seen[p.path] = struct{}{}
		body := p.content
		if data, err := os.ReadFile(p.path); err == nil {
			body = string(data)
		}
		body = strings.TrimSpace(body)
		if body == "" || strings.Contains(existing, body) {
			continue
		}
		if len(body) > maxPlanBytes {
			body = strings.TrimSpace(string([]rune(body)[:min(len([]rune(body)), maxPlanBytes/2)])) + "\n...(truncated)"
		}
		sections = append(sections, "[Plan: "+filepath.Base(p.path)+"]\n"+body)
	}
	return strings.Join(sections, "\n\n")
}

func decodeArgString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	// Antigravity transcripts sometimes store stringified JSON literals inside args.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var inner string
		if json.Unmarshal([]byte(s), &inner) == nil {
			return inner
		}
	}
	return s
}

func isPlanFileName(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(base, ".md") && strings.Contains(base, "plan")
}

func isPlanArtifact(path string, metaRaw json.RawMessage) bool {
	hasMeta := false
	requestFeedback := false
	if len(metaRaw) > 0 && strings.TrimSpace(string(metaRaw)) != "null" {
		raw := metaRaw
		if raw[0] == '"' {
			var inner string
			if json.Unmarshal(raw, &inner) == nil {
				raw = []byte(inner)
			}
		}
		var meta struct {
			RequestFeedback bool `json:"RequestFeedback"`
		}
		if json.Unmarshal(raw, &meta) == nil {
			hasMeta = true
			requestFeedback = meta.RequestFeedback
		}
	}
	if requestFeedback {
		return true
	}
	if !isPlanFileName(path) {
		return false
	}
	if hasMeta {
		return true
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains(clean, "/.gemini/antigravity") && strings.Contains(clean, "/brain/")
}

func (t *turn) recordPlan(path, content string) {
	for i := range t.plans {
		if t.plans[i].path == path {
			if content != "" {
				t.plans[i].content = content
			}
			return
		}
	}
	t.plans = append(t.plans, planArtifact{path: path, content: content})
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
	var primaryPath, content string
	var artifactMeta json.RawMessage
	for key, value := range args {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "path", "filepath", "targetfile", "targetfilepath", "sourcepath", "destinationpath", "oldpath", "newpath":
			if path := strings.TrimSpace(decodeArgString(value)); path != "" {
				clean := filepath.Clean(path)
				if t.files == nil {
					t.files = make(map[string]struct{})
				}
				t.files[clean] = struct{}{}
				if primaryPath == "" {
					primaryPath = clean
				}
			}
		case "content":
			content = decodeArgString(value)
		case "artifactmetadata":
			artifactMeta = value
		}
	}
	if primaryPath != "" && isPlanArtifact(primaryPath, artifactMeta) {
		t.recordPlan(primaryPath, content)
	}
}
