package adapter

import (
	"context"
	"path/filepath"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type Session struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Workspace string    `json:"workspace,omitempty"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Pending   bool      `json:"pending,omitempty"`
}

type AgentAdapter interface {
	Provider() string
	ListSessions(workspace string) ([]Session, error)
	// ReadHistory returns entries in native order for the last limit turns.
	// A zero limit selects all turns.
	ReadHistory(sessionID string, limit int) ([]blackboard.Entry, error)
	Watch(ctx context.Context, sessionID string, onEntry func(blackboard.Entry) error) error
}

// LiveAdapter can recover entries written since a durable binding time. The
// caller must accept duplicate notifications when restarting a failed watcher.
type LiveAdapter interface {
	AgentAdapter
	WatchSince(ctx context.Context, sessionID string, since time.Time, onEntry func(blackboard.Entry) error) error
}

// LastTurns keeps both roles belonging to the most recent native turns.
func LastTurns(entries []blackboard.Entry, limit int) []blackboard.Entry {
	if limit <= 0 {
		return entries
	}
	turns := make(map[string]struct{})
	for i := len(entries) - 1; i >= 0 && len(turns) < limit; i-- {
		turns[entries[i].NativeTurnID] = struct{}{}
	}
	selected := make([]blackboard.Entry, 0)
	for _, entry := range entries {
		if _, ok := turns[entry.NativeTurnID]; ok {
			selected = append(selected, entry)
		}
	}
	return selected
}

func SameWorkspace(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return normalizedPath(a) == normalizedPath(b)
}

func normalizedPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}
