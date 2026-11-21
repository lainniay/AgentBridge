package hub

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

var (
	ErrRoomNotFound        = errors.New("room not found")
	ErrParticipantNotFound = errors.New("participant not found")
	ErrAlreadyBound        = errors.New("session already bound")
	ErrInvalid             = errors.New("invalid room or participant parameters")
)

type Participant struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	SessionID    string    `json:"session_id"`
	ReadCursor   uint64    `json:"read_cursor"`
	CaptureSince time.Time `json:"capture_since"`
}

type Room struct {
	ID           string        `json:"id"`
	Workspace    string        `json:"workspace"`
	Participants []Participant `json:"participants"`
}

func (r Room) clone() Room {
	r.Participants = slices.Clone(r.Participants)
	return r
}

func (r Room) participantIndex(id string) int {
	return slices.IndexFunc(r.Participants, func(p Participant) bool {
		return p.ID == id
	})
}

type Binding struct {
	Provider     string
	SessionID    string
	Backfill     []blackboard.Entry
	CaptureSince time.Time
}

type Status struct {
	Bound       bool           `json:"bound"`
	Room        *Room          `json:"room,omitempty"`
	Participant *Participant   `json:"participant,omitempty"`
	Unread      map[string]int `json:"unread,omitempty"`
	Handoff     string         `json:"handoff,omitempty"`
}

type ReadOptions struct {
	Mode      string `json:"mode"`
	Limit     int    `json:"limit"`
	Peek      bool   `json:"peek"`
	AfterSeq  uint64 `json:"after_seq"`
	BeforeSeq uint64 `json:"before_seq"`
}

type ReadResult struct {
	Bound     bool               `json:"bound"`
	RoomID    string             `json:"room_id,omitempty"`
	Entries   []blackboard.Entry `json:"entries,omitempty"`
	Path      string             `json:"path,omitempty"`
	Cursor    uint64             `json:"cursor,omitempty"`
	LatestSeq uint64             `json:"latest_seq,omitempty"`
	HasMore   bool               `json:"has_more,omitempty"`
}

type Hub struct {
	mu       sync.Mutex
	home     string
	roomsDir string
	state    string
	rooms    map[string]Room
	boards   map[string]*blackboard.Store
}

type storedState struct {
	Rooms []Room `json:"rooms"`
}

// HomeDir resolves the AgentBridge data directory, preferring an explicit
// override, then AGENTBRIDGE_HOME, then legacy AGENTBRIDGE_STATE_DIR, then ~/.agentbridge.
func HomeDir(override string) (string, error) {
	dir := strings.TrimSpace(override)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("AGENTBRIDGE_HOME"))
	}
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("AGENTBRIDGE_STATE_DIR"))
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".agentbridge")
	}
	return filepath.Abs(dir)
}

func Open(homeDir string) (*Hub, error) {
	dir, err := HomeDir(homeDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.json")
	h := &Hub{
		home:     dir,
		roomsDir: filepath.Join(dir, "rooms"),
		state:    path,
		rooms:    make(map[string]Room),
		boards:   make(map[string]*blackboard.Store),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	var state storedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	for _, room := range state.Rooms {
		if len(room.Participants) == 0 {
			continue
		}
		room.Workspace, err = workspacePath(room.Workspace)
		if err != nil {
			return nil, fmt.Errorf("decode room %q workspace: %w", room.ID, err)
		}
		h.rooms[room.ID] = room
	}
	return h, nil
}

func (h *Hub) Home() string     { return h.home }
func (h *Hub) RoomsDir() string { return h.roomsDir }

func (h *Hub) BindRoom(workspace string, bindings []Binding) (Room, error) {
	workspace, err := existingWorkspace(workspace)
	if err != nil {
		return Room{}, err
	}
	id, err := newID("rm_")
	if err != nil {
		return Room{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.join(Room{ID: id, Workspace: workspace}, bindings, true)
}

func (h *Hub) JoinRoom(roomID string, bindings []Binding) (Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	if !ok {
		return Room{}, ErrRoomNotFound
	}
	return h.join(room.clone(), bindings, false)
}

func (h *Hub) join(room Room, bindings []Binding, newRoom bool) (Room, error) {
	if len(bindings) == 0 {
		return Room{}, fmt.Errorf("%w: at least one binding is required", ErrInvalid)
	}
	var history []blackboard.Entry
	type identity struct{ provider, sessionID string }
	batch := make(map[identity]bool, len(bindings))
	now := time.Now().UTC()
	for _, binding := range bindings {
		if !blackboard.ValidProvider(binding.Provider) || strings.TrimSpace(binding.SessionID) == "" {
			return Room{}, fmt.Errorf("%w: provider and session ID are required", ErrInvalid)
		}
		key := identity{binding.Provider, binding.SessionID}
		_, _, bound := h.resolve("", binding.Provider, binding.SessionID)
		if bound || batch[key] {
			return Room{}, ErrAlreadyBound
		}
		batch[key] = true
		id, err := newID("p_")
		if err != nil {
			return Room{}, err
		}
		p := Participant{ID: id, Provider: binding.Provider, SessionID: binding.SessionID, CaptureSince: binding.CaptureSince}
		if p.CaptureSince.IsZero() {
			p.CaptureSince = now
		}
		room.Participants = append(room.Participants, p)
		for _, entry := range binding.Backfill {
			entry.Provider = p.Provider
			entry.ParticipantID = p.ID
			if entry.TS.IsZero() {
				entry.TS = now
			}
			history = append(history, entry)
		}
	}
	slices.SortStableFunc(history, func(a, b blackboard.Entry) int { return a.TS.Compare(b.TS) })
	board, err := h.board(room)
	if err != nil {
		return Room{}, err
	}
	err = board.AppendBatch(history, func() error { return h.save(room) })
	if err != nil {
		if newRoom {
			delete(h.boards, room.ID)
			err = errors.Join(err, board.DiscardEmpty())
		}
		return Room{}, err
	}
	h.rooms[room.ID] = room
	return room.clone(), nil
}

func (h *Hub) DetachParticipant(roomID, participantID string) error {
	return h.detachParticipant(roomID, participantID, false)
}

// UnbindParticipant also detaches the other member of a one-to-one room.
func (h *Hub) UnbindParticipant(roomID, participantID string) error {
	return h.detachParticipant(roomID, participantID, true)
}

func (h *Hub) detachParticipant(roomID, participantID string, detachCompanion bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	room, exists := h.rooms[roomID]
	if !exists {
		return ErrRoomNotFound
	}
	index := room.participantIndex(participantID)
	if index < 0 {
		return ErrParticipantNotFound
	}
	if detachCompanion && len(room.Participants) <= 2 {
		room.Participants = []Participant{}
	} else {
		room = room.clone()
		room.Participants = slices.Delete(room.Participants, index, index+1)
	}
	if len(room.Participants) == 0 {
		if err := h.saveWithout(roomID); err != nil {
			return err
		}
		delete(h.rooms, roomID)
		delete(h.boards, roomID)
		return nil
	}
	if err := h.save(room); err != nil {
		return err
	}
	h.rooms[roomID] = room
	return nil
}

func (h *Hub) IngestEntry(roomID, participantID string, entry blackboard.Entry) error {
	return h.IngestEntries(roomID, participantID, []blackboard.Entry{entry})
}

// IngestEntries commits the records collected together at a native turn boundary.
func (h *Hub) IngestEntries(roomID, participantID string, entries []blackboard.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	room, exists := h.rooms[roomID]
	if !exists {
		return ErrRoomNotFound
	}
	index := room.participantIndex(participantID)
	if index < 0 {
		return ErrParticipantNotFound
	}
	board, err := h.board(room)
	if err != nil {
		return err
	}
	batch := slices.Clone(entries)
	for i := range batch {
		batch[i].ParticipantID = participantID
		batch[i].Provider = room.Participants[index].Provider
	}
	return board.AppendBatch(batch, nil)
}

func (h *Hub) Rooms(workspace string) []Room {
	if workspace != "" {
		var err error
		workspace, err = workspacePath(workspace)
		if err != nil {
			return []Room{}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rooms := h.roomsWith(Room{}, "")
	if workspace == "" {
		return rooms
	}
	return slices.DeleteFunc(rooms, func(room Room) bool { return room.Workspace != workspace })
}

func (h *Hub) Resolve(workspace, provider, sessionID string) (Room, Participant, bool) {
	if workspace == "" && sessionID == "" {
		return Room{}, Participant{}, false
	}
	if workspace != "" {
		var err error
		workspace, err = workspacePath(workspace)
		if err != nil {
			return Room{}, Participant{}, false
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	room, p, ok := h.resolve(workspace, provider, sessionID)
	return room.clone(), p, ok
}

func (h *Hub) resolve(workspace, provider, sessionID string) (Room, Participant, bool) {
	var foundRoom Room
	var found Participant
	for _, room := range h.rooms {
		if workspace != "" && room.Workspace != workspace {
			continue
		}
		for _, p := range room.Participants {
			if (provider != "" && p.Provider != provider) || (sessionID != "" && p.SessionID != sessionID) {
				continue
			}
			if found.ID != "" {
				return Room{}, Participant{}, false
			}
			foundRoom, found = room, p
		}
	}
	return foundRoom, found, found.ID != ""
}

func (h *Hub) Status(roomID, participantID string) (Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	room := h.rooms[roomID]
	index := room.participantIndex(participantID)
	if index < 0 {
		return Status{}, nil
	}
	board, err := h.board(room)
	if err != nil {
		return Status{}, err
	}
	p := room.Participants[index]
	unread := make(map[string]int)
	for _, peer := range room.Participants {
		if peer.ID != p.ID {
			unread[peer.ID] = 0
		}
	}
	for _, entry := range board.EntriesAfter(p.ReadCursor) {
		if entry.ParticipantID != p.ID {
			unread[entry.ParticipantID]++
		}
	}
	room = room.clone()
	return Status{Bound: true, Room: &room, Participant: &p, Unread: unread, Handoff: "idle"}, nil
}

func (h *Hub) Read(roomID, participantID string, opts ReadOptions) (ReadResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	room := h.rooms[roomID]
	index := room.participantIndex(participantID)
	if index < 0 {
		return ReadResult{}, nil
	}
	if opts.Limit < 0 {
		return ReadResult{}, fmt.Errorf("%w: negative limit", ErrInvalid)
	}
	board, err := h.board(room)
	if err != nil {
		return ReadResult{}, err
	}
	latest := board.LatestSeq()
	p := room.Participants[index]
	switch opts.Mode {
	case "path":
		return ReadResult{Bound: true, RoomID: room.ID, Path: board.Path(), LatestSeq: latest}, nil
	case "full":
		entries := board.Entries()
		filtered := make([]blackboard.Entry, 0, len(entries))
		for _, entry := range entries {
			if opts.AfterSeq > 0 && entry.Seq <= opts.AfterSeq {
				continue
			}
			if opts.BeforeSeq > 0 && entry.Seq >= opts.BeforeSeq {
				continue
			}
			filtered = append(filtered, entry)
		}
		hasMore := false
		if opts.Limit > 0 && len(filtered) > opts.Limit {
			hasMore = true
			if opts.AfterSeq > 0 && opts.BeforeSeq == 0 {
				filtered = filtered[:opts.Limit]
			} else {
				filtered = filtered[len(filtered)-opts.Limit:]
			}
		}
		return ReadResult{
			Bound:     true,
			RoomID:    room.ID,
			Entries:   filtered,
			Cursor:    p.ReadCursor,
			LatestSeq: latest,
			HasMore:   hasMore,
		}, nil
	case "", "diff":
		return h.diff(room, index, board.EntriesAfter(p.ReadCursor), latest, opts)
	default:
		return ReadResult{}, fmt.Errorf("%w: unknown read mode %q", ErrInvalid, opts.Mode)
	}
}

func (h *Hub) diff(room Room, index int, entries []blackboard.Entry, latest uint64, opts ReadOptions) (ReadResult, error) {
	p := room.Participants[index]
	cursor := p.ReadCursor
	diff := make([]blackboard.Entry, 0)
	hasMore := false
	for _, entry := range entries {
		if entry.ParticipantID != p.ID {
			if opts.Limit > 0 && len(diff) == opts.Limit {
				hasMore = true
				break
			}
			diff = append(diff, entry)
		}
		cursor = entry.Seq
	}
	effectiveCursor := p.ReadCursor
	if !opts.Peek && cursor > p.ReadCursor {
		room = room.clone()
		room.Participants[index].ReadCursor = cursor
		if err := h.save(room); err != nil {
			return ReadResult{}, err
		}
		h.rooms[room.ID] = room
		effectiveCursor = cursor
	}
	return ReadResult{
		Bound:     true,
		RoomID:    room.ID,
		Entries:   diff,
		Cursor:    effectiveCursor,
		LatestSeq: latest,
		HasMore:   hasMore,
	}, nil
}

func (h *Hub) Blackboard(roomID string) ([]blackboard.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room, ok := h.rooms[roomID]; ok && len(room.Participants) > 0 {
		board, err := h.board(room)
		if err != nil {
			return nil, err
		}
		return board.Entries(), nil
	}
	entries, err := blackboard.Load(h.roomsDir, roomID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrRoomNotFound
	}
	return entries, err
}

func (h *Hub) board(room Room) (*blackboard.Store, error) {
	if board := h.boards[room.ID]; board != nil {
		return board, nil
	}
	h.migrateLegacyRoom(room)
	board, err := blackboard.Open(h.roomsDir, room.ID)
	if err != nil {
		return nil, err
	}
	h.boards[room.ID] = board
	return board, nil
}

// migrateLegacyRoom copies a pre-AGENTBRIDGE_HOME workspace blackboard into
// roomsDir once when the target file does not yet exist.
func (h *Hub) migrateLegacyRoom(room Room) {
	if room.Workspace == "" || room.ID == "" {
		return
	}
	target := filepath.Join(h.roomsDir, room.ID+".jsonl")
	if _, err := os.Stat(target); err == nil {
		return
	}
	legacy := filepath.Join(room.Workspace, ".agent", "blackboard", room.ID+".jsonl")
	data, err := os.ReadFile(legacy)
	if err != nil || len(data) == 0 {
		return
	}
	if err := os.MkdirAll(h.roomsDir, 0700); err != nil {
		return
	}
	_ = os.WriteFile(target, data, 0600)
}

func (h *Hub) roomsWith(replacement Room, removeID string) []Room {
	rooms := make([]Room, 0, len(h.rooms)+1)
	for id, room := range h.rooms {
		if id != replacement.ID && id != removeID {
			rooms = append(rooms, room.clone())
		}
	}
	if replacement.ID != "" && replacement.ID != removeID {
		rooms = append(rooms, replacement.clone())
	}
	slices.SortFunc(rooms, func(a, b Room) int { return cmp.Compare(a.ID, b.ID) })
	return rooms
}

func (h *Hub) save(room Room) error {
	return h.writeState(h.roomsWith(room, ""))
}

func (h *Hub) saveWithout(roomID string) error {
	return h.writeState(h.roomsWith(Room{}, roomID))
}

func (h *Hub) writeState(rooms []Room) error {
	dir := filepath.Dir(h.state)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	err = errors.Join(enc.Encode(storedState{Rooms: rooms}), tmp.Sync(), tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.state)
}

func newID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func workspacePath(workspace string) (string, error) {
	path, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real, nil
	}
	return path, nil
}

func existingWorkspace(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("%w: workspace is required", ErrInvalid)
	}
	path, err := workspacePath(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: workspace must be a directory", ErrInvalid)
	}
	return path, nil
}
