package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/blackboard"
)

type Adapter struct {
	root string

	mu           sync.Mutex
	metadata     map[string]sessionMetadata
	catalog      map[string][]sessionFile
	catalogError error
	discoveredAt time.Time
	indexInfo    fs.FileInfo
	indexNames   map[string]string
}

const (
	discoveryInterval = 5 * time.Second
	headerLineLimit   = 32
	titleSearchLimit  = 4 << 20
)

func New(root string) *Adapter {
	if root == "" {
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, _ := os.UserHomeDir()
			home = filepath.Join(userHome, ".codex")
		}
		root = filepath.Join(home, "sessions")
	}
	return &Adapter{root: root}
}

var _ adapter.LiveAdapter = (*Adapter)(nil)

func (*Adapter) Provider() string { return "codex" }

type sessionFile struct {
	session adapter.Session
	path    string
	info    fs.FileInfo
}

type sessionMetadata struct {
	info            fs.FileInfo
	session         adapter.Session
	internal        bool
	titleSearchDone bool
}

func (m sessionMetadata) matches(info fs.FileInfo) bool {
	if !os.SameFile(m.info, info) {
		return false
	}
	if m.internal && info.Size() >= m.info.Size() {
		return true
	}
	if m.session.ID != "" && (m.session.Title != "" || m.titleSearchDone) && info.Size() > m.info.Size() {
		// Codex appends to an existing rollout without changing its header.
		return true
	}
	// Re-read replaced or truncated files and partial headers that changed.
	return info.Size() == m.info.Size() && info.ModTime().Equal(m.info.ModTime())
}

func (a *Adapter) threadNamesLocked() map[string]string {
	indexPath := filepath.Join(filepath.Dir(a.root), "session_index.jsonl")
	info, err := os.Stat(indexPath)
	if os.IsNotExist(err) {
		a.indexInfo = nil
		a.indexNames = nil
		return nil
	}
	if err != nil {
		return a.indexNames
	}
	if a.indexInfo != nil && os.SameFile(a.indexInfo, info) &&
		info.Size() == a.indexInfo.Size() && info.ModTime().Equal(a.indexInfo.ModTime()) {
		return a.indexNames
	}
	names := make(map[string]string)
	err = adapter.ReadJSONL(indexPath, func(line []byte, _ int64) error {
		var row struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(line, &row) == nil && row.ID != "" {
			if name := strings.TrimSpace(row.ThreadName); name != "" {
				names[row.ID] = name
			}
		}
		return nil
	})
	if err != nil {
		return a.indexNames
	}
	a.indexInfo = info
	a.indexNames = names
	return names
}

func (a *Adapter) sessions(refresh bool) (map[string][]sessionFile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !refresh && time.Since(a.discoveredAt) < discoveryInterval {
		return a.catalog, a.catalogError
	}

	threadNames := a.threadNamesLocked()
	found := make(map[string][]sessionFile)
	metadata := make(map[string]sessionMetadata)
	err := filepath.WalkDir(a.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		cached, ok := a.metadata[path]
		if ok && cached.matches(info) {
			cached.info = info
		} else {
			cached, ok = readSessionMeta(path, info)
			if !ok {
				return nil
			}
		}
		metadata[path] = cached
		if cached.internal {
			return nil
		}
		session := cached.session
		if session.ID != "" {
			session.Provider = a.Provider()
			session.UpdatedAt = cached.info.ModTime()
			if name := threadNames[session.ID]; name != "" {
				session.Title = name
			}
			found[session.ID] = append(found[session.ID], sessionFile{session: session, path: path, info: cached.info})
		}
		return nil
	})
	for _, segment := range found {
		slices.SortFunc(segment, func(x, y sessionFile) int {
			if order := x.session.UpdatedAt.Compare(y.session.UpdatedAt); order != 0 {
				return order
			}
			return strings.Compare(x.path, y.path)
		})
	}
	if err == nil {
		a.metadata = metadata
	}
	// A refresh publishes a new immutable snapshot for all watchers. Retain
	// the previous metadata cache on a failed walk so it can be reused later.
	a.catalog, a.catalogError, a.discoveredAt = found, err, time.Now()
	return a.catalog, a.catalogError
}

func readSessionMeta(path string, expected fs.FileInfo) (sessionMetadata, bool) {
	f, err := os.Open(path)
	if err != nil {
		return sessionMetadata{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(expected, info) {
		return sessionMetadata{}, false
	}
	metadata := sessionMetadata{info: info}
	reader := bufio.NewReader(f)
	for range headerLineLimit {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return sessionMetadata{}, false
		}
		if session, internal, ok := parseSessionMeta(line); ok {
			// Child/fork logs can embed parent metadata after their own.
			metadata.session = session
			metadata.internal = internal
			if !internal && strings.TrimSpace(session.Title) == "" {
				metadata.session.Title, metadata.titleSearchDone = readTitle(path, reader)
			}
			break
		}
		if err == io.EOF {
			break
		}
	}
	return metadata, true
}

func readTitle(path string, input io.Reader) (string, bool) {
	budget := &io.LimitedReader{R: input, N: titleSearchLimit}
	reader := bufio.NewReader(budget)
	p := newParser(path)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return "", budget.N == 0
		}
		for _, next := range p.line(line, offset, true) {
			if next.entry.Role == "user" {
				title := []rune(strings.Join(strings.Fields(next.entry.Content), " "))
				return string(title[:min(len(title), 40)]), true
			}
		}
		offset += int64(len(line))
	}
}

func parseSessionMeta(line []byte) (adapter.Session, bool, bool) {
	var record envelope
	if json.Unmarshal(line, &record) != nil || record.Type != "session_meta" {
		return adapter.Session{}, false, false
	}
	var meta struct {
		ID             string          `json:"id"`
		SessionID      string          `json:"session_id"`
		CWD            string          `json:"cwd"`
		Title          string          `json:"title"`
		ThreadSource   string          `json:"thread_source"`
		ParentThreadID string          `json:"parent_thread_id"`
		Source         json.RawMessage `json:"source"`
	}
	if json.Unmarshal(record.Payload, &meta) != nil {
		return adapter.Session{}, false, false
	}
	if meta.ID == "" {
		meta.ID = meta.SessionID
	}
	if meta.ID == "" {
		return adapter.Session{}, false, false
	}
	internal := isInternalSession(meta.ThreadSource, meta.ParentThreadID, meta.Source)
	return adapter.Session{ID: meta.ID, Workspace: meta.CWD, Title: meta.Title}, internal, true
}

func isInternalSession(threadSource, parentThreadID string, source json.RawMessage) bool {
	if parentThreadID != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(threadSource)) {
	case "subagent", "guardian_review":
		return true
	}
	trimmed := bytes.TrimSpace(source)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var src map[string]json.RawMessage
		if json.Unmarshal(trimmed, &src) == nil {
			if _, ok := src["subagent"]; ok {
				return true
			}
		}
	}
	return false
}

func (a *Adapter) ListSessions(workspace string) ([]adapter.Session, error) {
	found, err := a.sessions(true)
	if err != nil {
		return nil, err
	}
	result := make([]adapter.Session, 0)
	for _, files := range found {
		for i := len(files) - 1; i >= 0; i-- {
			session := files[i].session
			if workspace == "" || adapter.SameWorkspace(session.Workspace, workspace) {
				result = append(result, session)
				break
			}
		}
	}
	slices.SortFunc(result, func(x, y adapter.Session) int {
		if c := y.UpdatedAt.Compare(x.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return result, nil
}

func (a *Adapter) sessionFiles(id string, refresh bool) ([]sessionFile, error) {
	found, err := a.sessions(refresh)
	if err != nil {
		return nil, err
	}
	files, ok := found[id]
	if !ok {
		return nil, fmt.Errorf("Codex session log: %w", os.ErrNotExist)
	}
	return files, nil
}

func (a *Adapter) ReadHistory(sessionID string, limit int) ([]blackboard.Entry, error) {
	files, err := a.sessionFiles(sessionID, true)
	if err != nil {
		return nil, err
	}
	var parsed []parsedEntry
	for _, file := range files {
		parser := newParser(file.path)
		err = adapter.ReadJSONL(file.path, func(line []byte, offset int64) error {
			parsed = append(parsed, parser.line(line, offset, true)...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) > 1 {
		orderByPublication(parsed)
	}
	entries := make([]blackboard.Entry, 0, len(parsed))
	seen := make(map[[2]string]struct{})
	for _, next := range parsed {
		key := [2]string{next.entry.NativeTurnID, next.entry.Role}
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			entries = append(entries, next.entry)
		}
	}
	return adapter.LastTurns(entries, limit), nil
}

func (a *Adapter) Watch(ctx context.Context, sessionID string, onEntry func(blackboard.Entry) error) error {
	return a.WatchSince(ctx, sessionID, time.Now(), onEntry)
}

func (a *Adapter) WatchSince(ctx context.Context, sessionID string, since time.Time, onEntry func(blackboard.Entry) error) error {
	type source struct {
		cursor       adapter.JSONLTail
		parser       *parser
		headerLines  int
		validSession bool
	}
	tails := make(map[string]*source)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		files, err := a.sessionFiles(sessionID, false)
		if err != nil {
			return err
		}
		var batch []parsedEntry
		for _, file := range files {
			path := file.path
			s := tails[path]
			if s == nil {
				s = &source{cursor: adapter.JSONLTail{Path: path}}
				tails[path] = s
			}
			err := s.cursor.Poll(ctx, func() {
				s.parser = newParser(path)
				s.headerLines = 0
				s.validSession = false
			}, func(line []byte, offset int64, existing bool) error {
				// A path can be replaced before background discovery catches up.
				// Validate the physical generation before publishing any text.
				if !s.validSession {
					if s.headerLines < headerLineLimit {
						s.headerLines++
						if metadata, _, ok := parseSessionMeta(line); ok {
							s.validSession = metadata.ID == sessionID
							s.headerLines = headerLineLimit
						}
					}
					return nil
				}
				for _, next := range s.parser.line(line, offset, existing) {
					if next.afterBinding(since) {
						batch = append(batch, next)
					}
				}
				return nil
			})
			if os.IsNotExist(err) {
				delete(tails, path)
				continue
			}
			if err != nil {
				return err
			}
		}
		if len(files) > 1 {
			orderByPublication(batch)
		}
		for _, next := range batch {
			if err := onEntry(next.entry); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p parsedEntry) afterBinding(since time.Time) bool {
	if since.IsZero() {
		return true
	}
	if p.published.IsZero() {
		return !p.existing
	}
	return !p.published.Before(since)
}

func orderByPublication(entries []parsedEntry) {
	slices.SortStableFunc(entries, func(x, y parsedEntry) int {
		return x.published.Compare(y.published)
	})
}
