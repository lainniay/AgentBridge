package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/adapter/antigravity"
	"github.com/lainniay/AgentBridge/internal/blackboard"
	"github.com/lainniay/AgentBridge/internal/hub"
	"github.com/lainniay/AgentBridge/internal/mcp"
)

type WatcherStatus struct {
	RoomID    string `json:"room_id"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
	LastError string `json:"last_error,omitempty"`
}

type Server struct {
	hub       *hub.Hub
	adapters  map[string]adapter.AgentAdapter
	agy       *antigravity.Adapter
	workspace string
	ctx       context.Context

	mu          sync.Mutex
	watches     map[string]context.CancelFunc
	watchers    sync.WaitGroup
	notices     map[string]invocationNotice
	watchMu     sync.Mutex
	watchStates map[string]WatcherStatus
}

func New(ctx context.Context, h *hub.Hub, workspace string, agy *antigravity.Adapter, adapters ...adapter.AgentAdapter) *Server {
	s := &Server{hub: h, workspace: workspace, ctx: ctx, agy: agy,
		adapters:    make(map[string]adapter.AgentAdapter),
		watches:     make(map[string]context.CancelFunc),
		watchStates: make(map[string]WatcherStatus),
		notices:     make(map[string]invocationNotice)}
	s.adapters[agy.Provider()] = agy
	for _, a := range adapters {
		s.adapters[a.Provider()] = a
	}
	s.syncWatches()
	return s
}

func (s *Server) WatcherStatuses() []WatcherStatus {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	out := make([]WatcherStatus, 0, len(s.watchStates))
	for _, st := range s.watchStates {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RoomID != out[j].RoomID {
			return out[i].RoomID < out[j].RoomID
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// Wait blocks until canceled live watchers have stopped.
func (s *Server) Wait() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchers.Wait()
}

func (s *Server) Handler(listenHost string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "ok",
			"home":     s.hub.Home(),
			"rooms":    len(s.hub.Rooms("")),
			"watchers": s.WatcherStatuses(),
		})
	})
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("GET /api/rooms", s.rooms)
	mux.HandleFunc("POST /api/rooms/bind", s.bind)
	mux.HandleFunc("POST /api/rooms/detach", s.detach)
	mux.HandleFunc("GET /api/rooms/{id}/blackboard", s.blackboard)
	mux.HandleFunc("POST /api/hooks/antigravity", s.hook)
	mux.Handle("/mcp", mcp.Server{Hub: s.hub, Workspace: s.workspace, Manager: s})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, listenHost) {
			fail(w, http.StatusForbidden, "Invalid Host or Origin")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func localRequest(r *http.Request, listenHost string) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.Trim(r.Host, "[]")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && !(ip != nil && ip.IsLoopback()) &&
		(host == "" || !strings.EqualFold(host, listenHost) || (ip != nil && ip.IsUnspecified())) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && strings.EqualFold(u.Host, r.Host) &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if contentType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]); contentType != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "Expected application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest, "Invalid JSON")
		return false
	}
	return true
}

func (s *Server) requestWorkspace(value string) (string, error) {
	if value == "" {
		value = s.workspace
	}
	if value == "" {
		return "", errors.New("workspace is required")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace must be a directory")
	}
	return abs, nil
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	workspace, err := s.requestWorkspace(r.URL.Query().Get("workspace"))
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid workspace")
		return
	}
	all := make([]adapter.Session, 0)
	var unavailable []string
	for provider, a := range s.adapters {
		sessions, err := a.ListSessions(workspace)
		if err != nil {
			unavailable = append(unavailable, provider)
		}
		all = append(all, sessions...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	sort.Strings(unavailable)
	writeJSON(w, http.StatusOK, struct {
		Sessions    []adapter.Session `json:"sessions"`
		Unavailable []string          `json:"unavailable,omitempty"`
	}{all, unavailable})
}

func (s *Server) rooms(w http.ResponseWriter, r *http.Request) {
	workspace, err := s.requestWorkspace(r.URL.Query().Get("workspace"))
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid workspace")
		return
	}
	rooms := s.hub.Rooms(workspace)
	writeJSON(w, http.StatusOK, map[string]any{"rooms": append([]hub.Room{}, rooms...)})
}

type bindRequest struct {
	Workspace     string       `json:"workspace"`
	RoomID        string       `json:"room_id"`
	Latest        bool         `json:"latest"`
	BackfillTurns int          `json:"backfill_turns"`
	Participants  []sessionRef `json:"participants"`
}

func (s *Server) bind(w http.ResponseWriter, r *http.Request) {
	var req bindRequest
	if !readJSON(w, r, &req) {
		return
	}
	workspace, err := s.requestWorkspace(req.Workspace)
	if err != nil || req.BackfillTurns < 0 ||
		(req.Latest && (req.RoomID != "" || len(req.Participants) != 0)) ||
		(!req.Latest && len(req.Participants) == 0) {
		fail(w, http.StatusBadRequest, "Invalid workspace, participants, latest, or backfill_turns")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, err := s.bindSessions(r.Context(), workspace, req)
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) detach(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID        string `json:"room_id"`
		ParticipantID string `json:"participant_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.hub.DetachParticipant(req.RoomID, req.ParticipantID); err != nil {
		stateError(w, err)
		return
	}
	delete(s.notices, req.ParticipantID)
	s.syncWatches()
	writeJSON(w, http.StatusOK, map[string]bool{"detached": true})
}

func (s *Server) blackboard(w http.ResponseWriter, r *http.Request) {
	entries, err := s.hub.Blackboard(r.PathValue("id"))
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": append([]blackboard.Entry{}, entries...)})
}

func stateError(w http.ResponseWriter, err error) {
	var requestErr *controlError
	switch {
	case errors.As(err, &requestErr):
		fail(w, requestErr.status, requestErr.message)
	case errors.Is(err, hub.ErrRoomNotFound), errors.Is(err, hub.ErrParticipantNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, hub.ErrAlreadyBound):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, hub.ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("AgentBridge state: %v", err)
		fail(w, http.StatusInternalServerError, "Could not update or read room state")
	}
}

// Caller serializes room mutations and watcher reconciliation.
func (s *Server) syncWatches() {
	if s.ctx.Err() != nil {
		return
	}
	active := make(map[string]bool)
	for _, room := range s.hub.Rooms("") {
		for _, p := range room.Participants {
			a, ok := s.adapters[p.Provider].(adapter.LiveAdapter)
			if !ok {
				continue
			}
			active[p.ID] = true
			if _, ok := s.watches[p.ID]; ok {
				continue
			}
			ctx, cancel := context.WithCancel(s.ctx)
			s.watches[p.ID] = cancel
			s.watchMu.Lock()
			s.watchStates[p.ID] = WatcherStatus{RoomID: room.ID, Provider: p.Provider, SessionID: p.SessionID}
			s.watchMu.Unlock()
			s.watchers.Add(1)
			go s.watch(ctx, a, room.ID, p)
		}
	}
	for id, cancel := range s.watches {
		if !active[id] {
			cancel()
			delete(s.watches, id)
			s.watchMu.Lock()
			delete(s.watchStates, id)
			s.watchMu.Unlock()
		}
	}
}

func (s *Server) setWatcherError(participantID, msg string) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	st, ok := s.watchStates[participantID]
	if !ok {
		return
	}
	st.LastError = msg
	s.watchStates[participantID] = st
}

func (s *Server) watch(ctx context.Context, a adapter.LiveAdapter, roomID string, p hub.Participant) {
	defer s.watchers.Done()
	lastLogged := ""
	ingest := func(entry blackboard.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lastLogged != "" {
			lastLogged = ""
			s.setWatcherError(p.ID, "")
		}
		return s.hub.IngestEntry(roomID, p.ID, entry)
	}
	for ctx.Err() == nil {
		err := a.WatchSince(ctx, p.SessionID, p.CaptureSince, ingest)
		if ctx.Err() != nil || errors.Is(err, hub.ErrParticipantNotFound) || errors.Is(err, hub.ErrRoomNotFound) {
			return
		}
		if err != nil {
			msg := err.Error()
			s.setWatcherError(p.ID, msg)
			if msg != lastLogged {
				lastLogged = msg
				log.Printf("AgentBridge %s watcher (%s): %v", a.Provider(), p.SessionID, err)
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	empty := map[string]any{}
	var req struct {
		antigravity.HookInput
		EventName string `json:"hookEventName"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	input := req.HookInput
	event := r.URL.Query().Get("event")
	if event == "" {
		switch req.EventName {
		case "PreToolUse":
			event = "pre-tool-use"
		case "PreInvocation":
			event = "pre-invocation"
		case "Stop":
			event = "stop"
		}
	}
	if input.ConversationID == "" || (event == "pre-invocation" && !input.IsInitialInvocation()) {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	room, p, bound := s.hub.Resolve("", s.agy.Provider(), input.ConversationID)
	if bound && !input.MatchesWorkspace(room.Workspace) {
		writeJSON(w, http.StatusOK, empty)
		return
	}

	if event == "pre-tool-use" {
		workspace := input.CWD
		if len(input.WorkspacePaths) > 0 {
			workspace = input.WorkspacePaths[0]
		}
		if bound {
			workspace = room.Workspace
		}
		overwrite, toolName := preToolUseOverwrite(input.ToolCall.Name, input.ToolCall.Args, input.ConversationID, workspace)
		if toolName != "" && (bound || isManageTool(toolName)) {
			writeJSON(w, http.StatusOK, map[string]any{"decision": "allow", "overwrite": overwrite})
			return
		}
	}
	if !bound {
		writeJSON(w, http.StatusOK, empty)
		return
	}

	switch event {
	case "pre-invocation":
		if s.preInvocation(r.Context(), room.ID, p, input) {
			writeJSON(w, http.StatusOK, map[string]any{"injectSteps": []any{
				map[string]any{"systemMessage": map[string]string{
					"systemMessage": "AgentBridge 有新消息，可调用 bridge_read 查看。",
				}},
			}})
			return
		}
	case "stop":
		if input.IsCollectibleStop() {
			entries, err := s.agy.HookEntries(input)
			if err == nil && len(entries) != 0 && r.Context().Err() == nil {
				_ = s.hub.IngestEntries(room.ID, p.ID, entries)
			}
		}
	}
	writeJSON(w, http.StatusOK, empty)
}

type invocationNotice struct {
	turnID string
	steps  int
}

func (s *Server) preInvocation(ctx context.Context, roomID string, p hub.Participant, input antigravity.HookInput) bool {
	entry, err := s.agy.HookUser(input)
	if err != nil || ctx.Err() != nil {
		return false
	}
	if entry.NativeTurnID != "" && s.hub.IngestEntry(roomID, p.ID, entry) != nil {
		return false
	}
	status, err := s.hub.Status(roomID, p.ID)
	if err != nil {
		return false
	}
	for _, unread := range status.Unread {
		if unread > 0 {
			notice := invocationNotice{turnID: entry.NativeTurnID}
			if input.InitialNumSteps != nil {
				notice.steps = *input.InitialNumSteps
			}
			return s.recordNotice(p, notice)
		}
	}
	return false
}

func (s *Server) recordNotice(p hub.Participant, notice invocationNotice) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, current, bound := s.hub.Resolve("", p.Provider, p.SessionID); !bound || current.ID != p.ID {
		return false
	}
	last := s.notices[p.ID]
	if last.turnID != "" && notice.turnID != "" {
		if last.turnID == notice.turnID {
			return false
		}
	} else if last.steps > 0 && last.steps == notice.steps {
		return false
	}
	if notice.turnID != "" || notice.steps > 0 {
		s.notices[p.ID] = notice
	}
	return true
}

var bridgeTool = regexp.MustCompile(`(^|[._:/-])bridge_[a-z0-9_]+$`)

// IsBridgeTool recognizes bridge tools with or without an MCP namespace.
func IsBridgeTool(name string) bool {
	return bridgeTool.MatchString(name)
}

// IsBridgeToolCall matches both direct bridge_* tools and lazy-loaded MCP
// invocations routed through call_mcp_tool.
func IsBridgeToolCall(name string, rawArgs json.RawMessage) bool {
	_, toolName := preToolUseOverwrite(name, rawArgs, "check", "")
	return toolName != ""
}

func isManageTool(name string) bool {
	return name == "bridge_manage" || strings.HasSuffix(name, "bridge_manage") &&
		len(name) > len("bridge_manage") &&
		strings.ContainsRune("._:/-", rune(name[len(name)-len("bridge_manage")-1]))
}

// preToolUseOverwrite builds the shallow overwrite payload and identifies the
// underlying tool, including calls through the host's lazy MCP dispatcher.
func preToolUseOverwrite(name string, rawArgs json.RawMessage, conversationID, workspace string) (map[string]any, string) {
	inject := func(args map[string]any, tool string) {
		args["_conversation_id"] = conversationID
		if isManageTool(tool) && workspace != "" {
			args["_workspace"] = workspace
		}
	}

	if IsBridgeTool(name) {
		overwrite := make(map[string]any)
		inject(overwrite, name)
		return overwrite, name
	}
	if name != "call_mcp_tool" {
		return nil, ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(rawArgs, &fields) != nil {
		return nil, ""
	}
	var serverName, toolName string
	argsKey := "Arguments"
	var argsRaw json.RawMessage
	for key, val := range fields {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "servername":
			_ = json.Unmarshal(val, &serverName)
		case "toolname":
			_ = json.Unmarshal(val, &toolName)
		case "arguments":
			argsKey = key
			argsRaw = val
		}
	}
	serverName = strings.Trim(serverName, `"`)
	toolName = strings.Trim(toolName, `"`)
	if !IsBridgeTool(toolName) {
		return nil, ""
	}
	if serverName != "" && !strings.Contains(strings.ToLower(serverName), "agentbridge") {
		return nil, ""
	}
	merged := make(map[string]any)
	encodedAsString := false
	trimmed := strings.TrimSpace(string(argsRaw))
	if trimmed != "" && trimmed != "null" {
		if trimmed[0] == '"' {
			var inner string
			if json.Unmarshal(argsRaw, &inner) != nil {
				return nil, ""
			}
			encodedAsString = true
			if strings.TrimSpace(inner) != "" && json.Unmarshal([]byte(inner), &merged) != nil {
				return nil, ""
			}
		} else if json.Unmarshal(argsRaw, &merged) != nil {
			return nil, ""
		}
	}
	if merged == nil {
		merged = make(map[string]any)
	}
	inject(merged, toolName)
	if encodedAsString {
		encoded, err := json.Marshal(merged)
		if err != nil {
			return nil, ""
		}
		return map[string]any{argsKey: string(encoded)}, toolName
	}
	return map[string]any{argsKey: merged}, toolName
}

func Address() string {
	if addr := os.Getenv("AGENTBRIDGE_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:4319"
}

func Endpoint(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return fmt.Sprintf("http://%s", addr)
}
