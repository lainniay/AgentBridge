package blackboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Handoff struct {
	Target        string `json:"target"`
	Mode          string `json:"mode"`
	ReturnControl bool   `json:"return_control"`
}

type Entry struct {
	Seq           uint64    `json:"seq"`
	TS            time.Time `json:"ts"`
	RoomID        string    `json:"room_id"`
	ParticipantID string    `json:"participant_id"`
	Provider      string    `json:"provider"`
	Role          string    `json:"role"`
	NativeTurnID  string    `json:"native_turn_id"`
	Content       string    `json:"content"`
	FilesTouched  []string  `json:"files_touched,omitempty"`
	Handoff       *Handoff  `json:"handoff,omitempty"`
}

type entryKey struct {
	participant, turn, role string
}

func (e Entry) key() entryKey {
	return entryKey{e.ParticipantID, e.NativeTurnID, e.Role}
}

func (e Entry) clone() Entry {
	e.FilesTouched = slices.Clone(e.FilesTouched)
	if e.Handoff != nil {
		handoff := *e.Handoff
		e.Handoff = &handoff
	}
	return e
}

type Store struct {
	mu          sync.Mutex
	roomID      string
	jsonl       string
	path        string
	size        int64
	entries     []Entry
	seen        map[entryKey]bool
	createdDirs []string
}

var providerPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

func ValidProvider(provider string) bool {
	return providerPattern.MatchString(provider)
}

func paths(roomsDir, roomID string) (string, string, error) {
	if roomID == "" || roomID == "." || roomID == ".." || strings.ContainsAny(roomID, "/\\") {
		return "", "", errors.New("invalid room ID")
	}
	if strings.TrimSpace(roomsDir) == "" {
		return "", "", errors.New("rooms directory is required")
	}
	dir, err := filepath.Abs(roomsDir)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, roomID+".jsonl"), filepath.Join(dir, roomID+".md"), nil
}

// Open initializes a bound room inside roomsDir and repairs its derived Markdown view.
func Open(roomsDir, roomID string) (_ *Store, resultErr error) {
	jsonl, path, err := paths(roomsDir, roomID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(jsonl)
	var createdDirs []string
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		createdDirs = append(createdDirs, dir)
	}
	var createdFiles []string
	defer func() {
		if resultErr != nil {
			for _, path := range createdFiles {
				_ = os.Remove(path)
			}
			for _, dir := range createdDirs {
				_ = os.Remove(dir)
			}
		}
	}()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(jsonl, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		createdFiles = append(createdFiles, jsonl)
	} else if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(jsonl, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	entries, size, err := decode(data, roomID)
	if err != nil {
		return nil, err
	}
	if size < int64(len(data)) {
		if err := file.Truncate(size); err != nil {
			return nil, err
		}
		if err := file.Sync(); err != nil {
			return nil, err
		}
	}
	view := render(entries)
	existing, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(existing, view) {
		if errors.Is(err, os.ErrNotExist) {
			createdFiles = append(createdFiles, path)
		}
		if err := replaceView(path, view); err != nil {
			return nil, err
		}
	}
	store := &Store{roomID: roomID, jsonl: jsonl, path: path, size: size, entries: entries, seen: make(map[entryKey]bool, len(entries)), createdDirs: createdDirs}
	for _, entry := range entries {
		store.seen[entry.key()] = true
	}
	return store, nil
}

// Load reads existing history without initializing or repairing an unbound room.
func Load(roomsDir, roomID string) ([]Entry, error) {
	jsonl, _, err := paths(roomsDir, roomID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(jsonl)
	if err != nil {
		return nil, err
	}
	entries, _, err := decode(data, roomID)
	return entries, err
}

func decode(data []byte, roomID string) ([]Entry, int64, error) {
	// A partial final line can remain after a process is interrupted mid-write.
	size := bytes.LastIndexByte(data, '\n') + 1
	entries := make([]Entry, 0)
	seen := make(map[entryKey]bool)
	for _, line := range bytes.Split(data[:size], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, 0, fmt.Errorf("decode blackboard record %d: %w", len(entries)+1, err)
		}
		if entry.RoomID != roomID || entry.Seq != uint64(len(entries))+1 || seen[entry.key()] {
			return nil, 0, fmt.Errorf("invalid blackboard record %d", len(entries)+1)
		}
		seen[entry.key()] = true
		entries = append(entries, entry)
	}
	return entries, int64(size), nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) LatestSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.entries))
}

func (s *Store) Entries() []Entry {
	return s.EntriesAfter(0)
}

func (s *Store) EntriesAfter(seq uint64) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset := min(seq, uint64(len(s.entries)))
	entries := make([]Entry, len(s.entries)-int(offset))
	for index, entry := range s.entries[offset:] {
		entries[index] = entry.clone()
	}
	return entries
}

// AppendBatch appends complete JSONL records with one write. An optional commit
// persists the binding that owns the batch before it is exposed to the Hub.
func (s *Store) AppendBatch(entries []Entry, commit func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var records bytes.Buffer
	encoder := json.NewEncoder(&records)
	added := make([]Entry, 0, len(entries))
	batchKeys := make(map[entryKey]bool, len(entries))
	for _, entry := range entries {
		if entry.ParticipantID == "" || entry.NativeTurnID == "" || !ValidProvider(entry.Provider) ||
			(entry.Role != "user" && entry.Role != "assistant") || strings.TrimSpace(entry.Content) == "" {
			return errors.New("invalid blackboard entry")
		}
		key := entry.key()
		if s.seen[key] || batchKeys[key] {
			continue
		}
		batchKeys[key] = true
		entry.RoomID = s.roomID
		entry.Seq = uint64(len(s.entries)+len(added)) + 1
		if entry.TS.IsZero() {
			entry.TS = time.Now().UTC()
		}
		if err := encoder.Encode(entry); err != nil {
			return err
		}
		added = append(added, entry.clone())
	}
	if len(added) == 0 {
		if commit != nil {
			return commit()
		}
		return nil
	}

	all := append(s.entries, added...)
	view, err := stageView(s.path, render(all))
	if err != nil {
		return err
	}
	defer os.Remove(view)

	file, err := os.OpenFile(s.jsonl, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != s.size {
		return errors.New("blackboard changed outside the daemon")
	}

	rollback := func(cause error) error {
		if err := file.Truncate(s.size); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(cause, file.Sync())
	}

	written, err := file.Write(records.Bytes())
	if err != nil {
		return rollback(err)
	}
	if written != records.Len() {
		return rollback(io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return rollback(err)
	}
	if err := os.Rename(view, s.path); err != nil {
		return rollback(err)
	}
	if commit != nil {
		if err := commit(); err != nil {
			return errors.Join(rollback(err), replaceView(s.path, render(s.entries)))
		}
	}

	for _, entry := range added {
		s.seen[entry.key()] = true
	}
	s.entries = all
	s.size += int64(records.Len())
	return nil
}

// DiscardEmpty removes the files of a newly initialized room if its binding
// could not be committed.
func (s *Store) DiscardEmpty() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) != 0 {
		return errors.New("cannot discard a room with records")
	}
	err := errors.Join(os.Remove(s.jsonl), os.Remove(s.path))
	for _, dir := range s.createdDirs {
		_ = os.Remove(dir)
	}
	return err
}

func render(entries []Entry) []byte {
	var out strings.Builder
	for index, entry := range entries {
		if index > 0 {
			out.WriteByte('\n')
		}
		tag := entry.Provider
		if entry.Role == "user" {
			tag = "user"
		}
		fmt.Fprintf(&out, `<%s seq="%d"`, tag, entry.Seq)
		if entry.Role == "user" {
			fmt.Fprintf(&out, ` channel="%s"`, html.EscapeString(entry.Provider))
		}
		if entry.Handoff != nil {
			fmt.Fprintf(&out, ` handoff="%s" return_control="%t"`, html.EscapeString(entry.Handoff.Target), entry.Handoff.ReturnControl)
		}
		if len(entry.FilesTouched) > 0 {
			fmt.Fprintf(&out, ` files_touched="%s"`, html.EscapeString(strings.Join(entry.FilesTouched, ",")))
		}
		fmt.Fprintf(&out, ">\n%s\n</%s>\n", entry.Content, tag)
	}
	return []byte(out.String())
}

func replaceView(path string, data []byte) error {
	tmp, err := stageView(path, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

func stageView(path string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".view-*")
	if err != nil {
		return "", err
	}
	_, writeErr := tmp.Write(data)
	err = errors.Join(writeErr, tmp.Sync(), tmp.Close())
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
