package codex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type envelope struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  struct {
		Inherited       bool   `json:"inherited_user_message"`
		UserOrder       *int64 `json:"user_input_order"`
		HistorySequence *int64 `json:"history_sequence"`
	} `json:"metadata"`
}

type item struct {
	Type    string          `json:"type"`
	Phase   string          `json:"phase"`
	Status  string          `json:"status"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
	Changes json.RawMessage `json:"changes"`
}

type payload struct {
	item
	Role        string          `json:"role"`
	TurnID      string          `json:"turn_id"`
	Item        item            `json:"item"`
	Message     string          `json:"message"`
	LastAgent   string          `json:"last_agent_message"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
	Input       json.RawMessage `json:"input"`
	Attribution struct {
		TurnID      string `json:"turn_id"`
		TurnTrigger string `json:"turn_trigger"`
	} `json:"turn_attribution"`
	Context struct {
		TurnID string `json:"turn_id"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type parsedEntry struct {
	entry     blackboard.Entry
	existing  bool
	published time.Time
}

type turn struct {
	id           string
	started      bool
	userEmitted  bool
	finalEmitted bool
	internal     bool
	files        map[string]struct{}
	plan         string
	final        parsedEntry
	legacyFinal  parsedEntry
}

type parser struct {
	active   string
	turns    map[string]*turn
	finished map[string]struct{}
	fileID   string
}

func newParser(path string) *parser {
	pathHash := sha256.Sum256([]byte(path))
	return &parser{
		turns: make(map[string]*turn), finished: make(map[string]struct{}),
		fileID: fmt.Sprintf("%x", pathHash[:8]),
	}
}

func (p *parser) line(line []byte, offset int64, existing bool) []parsedEntry {
	var env envelope
	if json.Unmarshal(line, &env) != nil {
		return nil
	}
	switch env.Type {
	case "event_msg", "response_item", "turn_context":
	default:
		return nil
	}
	var data payload
	if json.Unmarshal(env.Payload, &data) != nil || env.Metadata.Inherited {
		return nil
	}
	kind := data.Type
	if env.Type == "turn_context" {
		if data.TurnID != "" {
			p.active = data.TurnID
		}
		return nil
	}
	if !trackEvent(env.Type, data) {
		return nil
	}
	id := data.TurnID
	if id == "" {
		id = data.Context.TurnID
	}
	if id == "" {
		id = data.Attribution.TurnID
	}
	if id == "" {
		id = p.active
	}
	if id == "" {
		id = "offset:" + p.fileID + ":" + strconv.FormatInt(offset, 10)
	}
	if _, done := p.finished[id]; done {
		return nil
	}
	t := p.turns[id]
	if t == nil {
		t = &turn{id: id}
		p.turns[id] = t
	}
	p.active = id
	ts, _ := time.Parse(time.RFC3339Nano, env.Timestamp)

	if env.Type == "event_msg" {
		switch kind {
		case "task_started", "turn_started":
			t.started = true
			t.internal = data.Attribution.TurnTrigger == "guardian_review"
		case "turn_aborted", "task_aborted":
			p.finish(id)
		case "user_message":
			return t.user(data.Message, ts, existing)
		case "agent_message":
			if data.Phase == "commentary" {
				break
			}
			if data.Phase == "final_answer" {
				return t.recordFinal(data.Message, ts, existing)
			} else if strings.TrimSpace(data.Message) != "" {
				t.legacyFinal = t.entry("assistant", data.Message, ts, existing)
			}
		case "item_completed":
			switch data.Item.Type {
			case "UserMessage", "userMessage", "user_message":
				return t.user(messageText(data.Item.Content), ts, existing)
			case "AgentMessage", "agentMessage", "agent_message":
				if data.Item.Phase == "final_answer" {
					return t.recordFinal(messageText(data.Item.Content), ts, existing)
				}
			case "FileChange", "fileChange", "file_change":
				if data.Item.Status == "" || data.Item.Status == "completed" {
					t.addChanges(data.Item.Changes)
				}
			case "Plan", "plan", "ProposedPlan", "proposed_plan":
				text := strings.TrimSpace(data.Item.Text)
				if text == "" {
					text = strings.TrimSpace(messageText(data.Item.Content))
				}
				if text != "" {
					t.plan = text
				}
			}
		case "patch_apply_end":
			t.addChanges(data.Changes)
		case "task_complete", "turn_complete", "turn_completed":
			p.finish(id)
			return t.complete(data.LastAgent, ts, existing)
		}
		return nil
	}

	switch kind {
	case "message":
		switch data.Role {
		case "user":
			body := messageText(data.Content)
			// Codex persists injected workspace/developer context as user-role
			// messages in some versions. Distinguish those from human inputs.
			if env.Metadata.UserOrder == nil && (env.Metadata.HistorySequence != nil || internalContext(body)) {
				return nil
			}
			return t.user(body, ts, existing)
		case "assistant":
			if data.Phase == "final_answer" {
				return t.recordFinal(messageText(data.Content), ts, existing)
			}
		}
	case "custom_tool_call", "function_call":
		t.addToolCall(data)
	}
	return nil
}

func trackEvent(kind string, data payload) bool {
	switch kind {
	case "event_msg":
		switch data.Type {
		case "task_started", "turn_started", "task_complete", "turn_complete", "turn_completed", "turn_aborted", "task_aborted", "user_message", "agent_message", "item_completed", "patch_apply_end":
			return true
		}
	case "response_item":
		switch data.Type {
		case "message":
			return data.Role == "user" || data.Role == "assistant"
		case "custom_tool_call", "function_call":
			return data.Name == "apply_patch"
		}
	}
	return false
}

func (p *parser) finish(id string) {
	p.finished[id] = struct{}{}
	delete(p.turns, id)
	if p.active == id {
		p.active = ""
	}
}

func (t *turn) entry(role, content string, ts time.Time, existing bool) parsedEntry {
	return parsedEntry{entry: blackboard.Entry{
		Provider: "codex", Role: role, Content: content, NativeTurnID: t.id, TS: ts,
	}, existing: existing, published: ts}
}

func (t *turn) user(content string, ts time.Time, existing bool) []parsedEntry {
	content = strings.TrimSpace(content)
	if t.userEmitted || t.internal || content == "" {
		return nil
	}
	t.userEmitted = true
	return []parsedEntry{t.entry("user", content, ts, existing)}
}

func (t *turn) recordFinal(content string, ts time.Time, existing bool) []parsedEntry {
	if t.finalEmitted || strings.TrimSpace(content) == "" {
		return nil
	}
	t.final = t.entry("assistant", content, ts, existing)
	if t.started {
		return nil
	}
	// A legacy rollout without lifecycle events can still explicitly mark
	// its final response.
	return t.complete("", ts, existing)
}

func (t *turn) complete(lastAgent string, ts time.Time, existing bool) []parsedEntry {
	if t.finalEmitted || t.internal {
		return nil
	}
	result := t.final
	if strings.TrimSpace(result.entry.Content) == "" {
		result = t.entry("assistant", lastAgent, ts, existing)
	}
	if strings.TrimSpace(result.entry.Content) == "" {
		result = t.legacyFinal
	}
	if t.plan != "" {
		if strings.TrimSpace(result.entry.Content) == "" {
			result = t.entry("assistant", t.plan, ts, existing)
		} else if !strings.Contains(result.entry.Content, t.plan) {
			result.entry.Content = strings.TrimSpace(result.entry.Content) + "\n\n" + t.plan
		}
	}
	if strings.TrimSpace(result.entry.Content) == "" {
		return nil
	}
	// Prefer the final message time. With truncated older logs the
	// completion event can be the first record to carry a timestamp.
	if result.entry.TS.IsZero() {
		result.entry.TS = ts
	}
	if ts.After(result.published) {
		result.published = ts
	}
	result.existing = result.existing && existing
	for path := range t.files {
		result.entry.FilesTouched = append(result.entry.FilesTouched, path)
	}
	slices.Sort(result.entry.FilesTouched)
	t.finalEmitted = true
	return []parsedEntry{result}
}

func messageText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var pieces []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &pieces) != nil {
		return ""
	}
	var allowed []string
	for _, piece := range pieces {
		switch strings.ToLower(piece.Type) {
		case "text", "input_text", "output_text":
			if piece.Text != "" {
				allowed = append(allowed, piece.Text)
			}
		}
	}
	return strings.Join(allowed, "\n")
}

func internalContext(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"# AGENTS.md instructions", "<environment_context", "<permissions instructions", "<user_instructions>"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func (t *turn) addFile(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if t.files == nil {
		t.files = make(map[string]struct{})
	}
	t.files[filepath.Clean(path)] = struct{}{}
}

func (t *turn) addChanges(raw json.RawMessage) {
	type change struct {
		Path     string `json:"path"`
		MovePath string `json:"move_path"`
	}
	var byPath map[string]change
	if json.Unmarshal(raw, &byPath) == nil {
		for path, value := range byPath {
			t.addFile(path)
			t.addFile(value.MovePath)
		}
		return
	}
	var list []change
	if json.Unmarshal(raw, &list) == nil {
		for _, value := range list {
			t.addFile(value.Path)
			t.addFile(value.MovePath)
		}
	}
}

func (t *turn) addToolCall(data payload) {
	var patch string
	if len(data.Input) > 0 {
		_ = json.Unmarshal(data.Input, &patch)
	}
	if patch == "" && json.Unmarshal(data.Arguments, &patch) == nil {
		var args struct {
			Patch string `json:"patch"`
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(patch), &args) == nil {
			patch = args.Patch
			if patch == "" {
				patch = args.Input
			}
		}
	}
	const begin = "*** Begin Patch"
	if !strings.HasPrefix(strings.TrimSpace(patch), begin) {
		return
	}
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if path, ok := strings.CutPrefix(line, prefix); ok {
				t.addFile(strings.TrimSuffix(path, "\r"))
			}
		}
	}
}
