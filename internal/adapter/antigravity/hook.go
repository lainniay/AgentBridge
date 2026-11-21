package antigravity

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type HookInput struct {
	ConversationID    string   `json:"conversationId"`
	TranscriptPath    string   `json:"transcriptPath"`
	LastUserInput     string   `json:"lastUserInput"`
	CWD               string   `json:"cwd"`
	WorkspacePaths    []string `json:"workspacePaths"`
	InitialNumSteps   *int     `json:"initialNumSteps"`
	InvocationNum     *int     `json:"invocationNum"`
	TerminationReason string   `json:"terminationReason"`
	ToolCall          struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// IsInitialInvocation allows the first call for hosts that count from either
// zero or one, and accepts older hook payloads without an invocation number.
func (input HookInput) IsInitialInvocation() bool {
	return input.InvocationNum == nil || *input.InvocationNum <= 1
}

// MatchesWorkspace checks workspace metadata when the host supplies it. A CWD
// beneath the bound root also identifies the workspace when roots are absent.
func (input HookInput) MatchesWorkspace(workspace string) bool {
	if len(input.WorkspacePaths) > 0 {
		for _, root := range input.WorkspacePaths {
			if adapter.SameWorkspace(root, workspace) {
				return true
			}
		}
		return false
	}
	if input.CWD == "" {
		return true
	}
	cwd, err := filepath.EvalSymlinks(input.CWD)
	if err != nil {
		cwd = input.CWD
	}
	rel, err := filepath.Rel(workspace, cwd)
	return err == nil && filepath.IsLocal(rel)
}

func (a *Adapter) hookPath(input HookInput) (string, error) {
	if input.TranscriptPath != "" {
		return input.TranscriptPath, nil
	}
	return a.path(input.ConversationID)
}

// HookUser returns the current human input using the same identity as backfill.
func (a *Adapter) HookUser(input HookInput) (blackboard.Entry, error) {
	if strings.TrimSpace(input.LastUserInput) == "" {
		return blackboard.Entry{}, nil
	}
	path, err := a.hookPath(input)
	if err != nil {
		return blackboard.Entry{}, err
	}
	current, err := readCurrent(path, true)
	if err != nil && !os.IsNotExist(err) {
		return blackboard.Entry{}, err
	}
	if index, ok := queuedUserIndex(input, current.lastIndex); ok {
		return newTurn(step{Index: &index, Content: input.LastUserInput}, 0).user, nil
	}
	if current.user.NativeTurnID != "" && matchesInput(current.user.Content, input.LastUserInput) {
		current.user.Content = unwrapUserContent(input.LastUserInput)
		return current.user, nil
	}
	return blackboard.Entry{}, nil
}

func queuedUserIndex(input HookInput, lastIndex *int) (int, bool) {
	if input.InitialNumSteps == nil || *input.InitialNumSteps < 1 || !input.IsInitialInvocation() {
		return 0, false
	}
	index := *input.InitialNumSteps - 1
	// The initial count can identify a not-yet-flushed user input. Only
	// infer its index when it follows the transcript, or this is step zero.
	if lastIndex == nil {
		return index, index == 0
	}
	return index, index == *lastIndex+1
}

// HookFinal returns only the latest turn's final answer after a normal Stop.
func (a *Adapter) HookFinal(input HookInput) (blackboard.Entry, error) {
	entries, err := a.HookEntries(input)
	if len(entries) > 0 && entries[len(entries)-1].Role == "assistant" {
		return entries[len(entries)-1], err
	}
	return blackboard.Entry{}, err
}

// HookEntries recovers a missing user record together with the final reply.
// Both records share the transcript's native turn identity.
func (a *Adapter) HookEntries(input HookInput) ([]blackboard.Entry, error) {
	if input.TerminationReason != "NO_TOOL_CALL" {
		return nil, nil
	}
	path, err := a.hookPath(input)
	if err != nil {
		return nil, err
	}
	current, err := readCurrent(path, false)
	if err != nil || current.user.NativeTurnID == "" {
		return nil, err
	}
	if input.LastUserInput != "" && !matchesInput(current.user.Content, input.LastUserInput) {
		// The transcript can briefly lag the hook. Never collect the
		// previous turn merely because it is still at the end of the file.
		return nil, nil
	}
	var entries []blackboard.Entry
	if strings.TrimSpace(current.user.Content) != "" {
		entries = append(entries, current.user)
	}
	return appendFinal(entries, current.final()), nil
}

func matchesInput(transcript, hook string) bool {
	return strings.TrimSpace(unwrapUserContent(transcript)) == strings.TrimSpace(unwrapUserContent(hook))
}

// readCurrent walks the file backwards and stops at the last native user step.
// Hooks can run after every tool call, so avoid rescanning previous turns.
func readCurrent(path string, userOnly bool) (turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return turn{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return turn{}, err
	}
	if !info.Mode().IsRegular() {
		return turn{}, os.ErrInvalid
	}

	var reversed []step
	var prefix []byte
	var lastIndex *int
	visit := func(line []byte, offset int64) (turn, bool) {
		s, ok := decodeStep(line)
		if !ok {
			return turn{}, false
		}
		if lastIndex == nil {
			lastIndex = s.Index
		}
		if s.user() {
			result := newTurn(s, offset)
			result.lastIndex = lastIndex
			slices.Reverse(reversed)
			for _, later := range reversed {
				result.accept(later)
			}
			return result, true
		}
		if !userOnly {
			reversed = append(reversed, s)
		}
		return turn{}, false
	}

	const block = 32 * 1024
	for end := info.Size(); end > 0; {
		start := max(0, end-block)
		chunk := make([]byte, end-start, end-start+int64(len(prefix)))
		if _, err := f.ReadAt(chunk, start); err != nil {
			if err == io.EOF {
				return turn{}, nil
			}
			return turn{}, err
		}
		chunk = append(chunk, prefix...)
		for {
			index := bytes.LastIndexByte(chunk, '\n')
			if index < 0 {
				break
			}
			if result, found := visit(chunk[index+1:], start+int64(index)+1); found {
				return result, nil
			}
			chunk = chunk[:index]
		}
		prefix = chunk
		end = start
	}
	result, _ := visit(prefix, 0)
	return result, nil
}
