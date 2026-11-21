package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type Adapter struct {
	root string
}

func New(root string) *Adapter {
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".gemini", "antigravity", "brain")
	}
	return &Adapter{root: root}
}

var _ adapter.AgentAdapter = (*Adapter)(nil)

func (*Adapter) Provider() string { return "antigravity" }

func (a *Adapter) path(sessionID string) (string, error) {
	if !filepath.IsLocal(sessionID) || filepath.Base(sessionID) != sessionID {
		return "", fmt.Errorf("invalid Antigravity conversation ID")
	}
	return filepath.Join(a.root, sessionID, ".system_generated", "logs", "transcript.jsonl"), nil
}

func (a *Adapter) ListSessions(workspace string) ([]adapter.Session, error) {
	sessions := make([]adapter.Session, 0)
	children, err := os.ReadDir(a.root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	summaries := a.summaries()
	for _, child := range children {
		if !child.IsDir() || strings.HasPrefix(child.Name(), ".") {
			continue
		}
		path, err := a.path(child.Name())
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		row := summaries[child.Name()]
		delete(summaries, child.Name())
		if matched, ok := row.matchWorkspace(workspace); ok {
			title := strings.TrimSpace(row.Title)
			if title == "" {
				title = a.fallbackTitle(path)
			}
			sessions = append(sessions, adapter.Session{
				ID: child.Name(), Provider: a.Provider(), Workspace: matched,
				Title: title, UpdatedAt: info.ModTime(),
			})
		}
	}
	for _, row := range summaries {
		matched, ok := row.matchWorkspace(workspace)
		path, err := a.path(row.ID)
		if !ok || matched == "" || err != nil {
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			sessions = append(sessions, adapter.Session{
				ID: row.ID, Provider: a.Provider(), Workspace: matched, Title: row.Title, Pending: true,
			})
		}
	}
	slices.SortFunc(sessions, func(x, y adapter.Session) int {
		if c := y.UpdatedAt.Compare(x.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return sessions, nil
}

var nativeConversationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CanBindPending allows explicitly selected native conversations to bind
// before discovery, provided no transcript exists and workspace metadata agrees.
func (a *Adapter) CanBindPending(sessionID, workspace string) bool {
	if !nativeConversationID.MatchString(sessionID) {
		return false
	}
	path, err := a.path(sessionID)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return false
	}
	_, ok := a.summaries()[sessionID].matchWorkspace(workspace)
	return ok
}

type summary struct {
	ID         string `json:"conversation_id"`
	Title      string `json:"title"`
	Workspaces string `json:"workspace_uris"`
}

func (s summary) workspaces() []string {
	var uris []string
	if json.Unmarshal([]byte(s.Workspaces), &uris) != nil {
		return nil
	}
	var roots []string
	for _, value := range uris {
		u, err := url.Parse(value)
		if err == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") && u.Path != "" {
			roots = append(roots, filepath.FromSlash(u.Path))
		}
	}
	return roots
}

func (s summary) matchWorkspace(workspace string) (string, bool) {
	roots := s.workspaces()
	if len(roots) == 0 {
		return "", true
	}
	for _, root := range roots {
		if workspace == "" || adapter.SameWorkspace(root, workspace) {
			return root, true
		}
	}
	return "", false
}

func (a *Adapter) summaries() map[string]summary {
	db, err := filepath.Abs(filepath.Join(a.root, "..", "conversation_summaries.db"))
	if err != nil {
		return nil
	}
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	for _, rawQuery := range []string{"mode=ro", "mode=ro&immutable=1"} {
		if index := querySummaries(db, rawQuery); index != nil {
			return index
		}
	}
	return nil
}

func querySummaries(db, rawQuery string) map[string]summary {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Try mode=ro first so committed WAL frames are visible; fall back to
	// immutable=1 when the host's live WAL/SHM cannot be opened read-only.
	uri := url.URL{Scheme: "file", Path: db, RawQuery: rawQuery}
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", uri.String(),
		"SELECT conversation_id, title, workspace_uris FROM conversation_summaries").Output()
	if err != nil {
		return nil
	}
	var rows []summary
	if json.Unmarshal(out, &rows) != nil {
		return nil
	}
	index := make(map[string]summary, len(rows))
	for _, row := range rows {
		index[row.ID] = row
	}
	return index
}

func (a *Adapter) fallbackTitle(path string) string {
	var title string
	_ = adapter.ReadJSONL(path, func(line []byte, _ int64) error {
		step, ok := decodeStep(line)
		if !ok || !step.user() {
			return nil
		}
		text := strings.Join(strings.Fields(unwrapUserContent(step.Content)), " ")
		if text == "" {
			return nil
		}
		runes := []rune(text)
		title = string(runes[:min(len(runes), 40)])
		return errStopScan
	})
	return title
}

var errStopScan = fmt.Errorf("stop")

func (a *Adapter) ReadHistory(sessionID string, limit int) ([]blackboard.Entry, error) {
	path, err := a.path(sessionID)
	if err != nil {
		return nil, err
	}
	return a.ReadTranscript(path, limit)
}

// ReadTranscript supports native Hook paths outside the default brain layout.
func (*Adapter) ReadTranscript(path string, limit int) ([]blackboard.Entry, error) {
	var current turn
	entries := make([]blackboard.Entry, 0)
	err := adapter.ReadJSONL(path, func(line []byte, offset int64) error {
		step, ok := decodeStep(line)
		if !ok {
			return nil
		}
		if step.user() {
			entries = appendFinal(entries, current.final())
			current = newTurn(step, offset)
			if strings.TrimSpace(current.user.Content) != "" {
				entries = append(entries, current.user)
			}
		} else {
			current.accept(step)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adapter.LastTurns(appendFinal(entries, current.final()), limit), nil
}

func appendFinal(entries []blackboard.Entry, final blackboard.Entry) []blackboard.Entry {
	if final.NativeTurnID != "" && final.Content != "" {
		return append(entries, final)
	}
	return entries
}

// Watch follows human inputs. An Antigravity transcript has no termination
// reason, so hook-based collection is authoritative for live assistant replies.
func (a *Adapter) Watch(ctx context.Context, sessionID string, onEntry func(blackboard.Entry) error) error {
	path, err := a.path(sessionID)
	if err != nil {
		return err
	}
	return adapter.TailJSONL(ctx, path, func() {}, func(line []byte, offset int64, existing bool) error {
		if existing {
			return nil
		}
		step, ok := decodeStep(line)
		if !ok || !step.user() || strings.TrimSpace(step.Content) == "" {
			return nil
		}
		return onEntry(newTurn(step, offset).user)
	})
}
