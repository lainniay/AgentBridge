package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/adapter"
	"github.com/lainniay/AgentBridge/internal/hub"
	"github.com/lainniay/AgentBridge/internal/mcp"
)

type sessionRef struct {
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
}

type peerSession struct {
	sessionRef
	Title        string    `json:"title"`
	UpdatedAt    time.Time `json:"updated_at"`
	AlreadyBound bool      `json:"already_bound"`
	Pending      bool      `json:"pending,omitempty"`
}

type controlError struct {
	status  int
	message string
}

func (e *controlError) Error() string { return e.message }

type discovery struct {
	server    *Server
	workspace string
	sessions  map[string][]adapter.Session
}

func (d discovery) list(provider string) ([]adapter.Session, error) {
	if found, ok := d.sessions[provider]; ok {
		return found, nil
	}
	a := d.server.adapters[provider]
	if a == nil {
		return nil, &controlError{http.StatusBadRequest, "Unknown provider"}
	}
	found, err := a.ListSessions(d.workspace)
	if err != nil {
		return nil, &controlError{http.StatusBadGateway, "Could not list " + provider + " sessions"}
	}
	slices.SortFunc(found, func(a, b adapter.Session) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	d.sessions[provider] = found
	return found, nil
}

func (d discovery) selectSession(ref sessionRef, backfill int) (adapter.Session, error) {
	if strings.TrimSpace(ref.SessionID) == "" {
		return adapter.Session{}, &controlError{http.StatusBadRequest, "session_id is required"}
	}
	found, err := d.list(ref.Provider)
	if err != nil {
		return adapter.Session{}, err
	}
	for _, session := range found {
		if session.ID == ref.SessionID {
			if backfill > 0 && session.Pending {
				return adapter.Session{}, &controlError{http.StatusConflict, "Conversation transcript is not available for backfill yet"}
			}
			return session, nil
		}
	}
	if backfill == 0 && ref.Provider == d.server.agy.Provider() && d.server.agy.CanBindPending(ref.SessionID, d.workspace) {
		return adapter.Session{ID: ref.SessionID, Provider: ref.Provider, Pending: true}, nil
	}
	return adapter.Session{}, &controlError{http.StatusNotFound, "Session is unavailable in this workspace"}
}

func (s *Server) binding(ref sessionRef) (hub.Room, hub.Participant, bool) {
	return s.hub.Resolve("", ref.Provider, ref.SessionID)
}

func (d discovery) latest(provider string, backfill int) (sessionRef, error) {
	found, err := d.list(provider)
	if err != nil {
		return sessionRef{}, err
	}
	for _, session := range found {
		// Automatic selection requires a positive workspace association.
		if session.Workspace == "" || (backfill > 0 && session.Pending) {
			continue
		}
		ref := sessionRef{provider, session.ID}
		if _, _, bound := d.server.binding(ref); !bound {
			return ref, nil
		}
	}
	return sessionRef{}, &controlError{http.StatusNotFound, "No available " + provider + " session in this workspace"}
}

func (s *Server) bindSessions(ctx context.Context, workspace string, req bindRequest) (hub.Room, error) {
	d := discovery{s, workspace, make(map[string][]adapter.Session)}
	since := time.Now()
	if req.Latest {
		for _, provider := range []string{s.agy.Provider(), "codex"} {
			ref, err := d.latest(provider, req.BackfillTurns)
			if err != nil {
				return hub.Room{}, err
			}
			req.Participants = append(req.Participants, ref)
		}
	}
	return s.commitBinding(ctx, d, since, req.RoomID, req.Participants, req.BackfillTurns)
}

// Caller holds s.mu so discovery decisions, room mutations and watcher
// reconciliation stay ordered with other control-plane requests.
func (s *Server) commitBinding(ctx context.Context, d discovery, since time.Time, roomID string, refs []sessionRef, backfill int) (hub.Room, error) {
	bindings := make([]hub.Binding, 0, len(refs))
	for _, ref := range refs {
		if _, err := d.selectSession(ref, backfill); err != nil {
			return hub.Room{}, err
		}
		binding := hub.Binding{Provider: ref.Provider, SessionID: ref.SessionID, CaptureSince: since}
		if backfill > 0 {
			history, err := s.adapters[ref.Provider].ReadHistory(ref.SessionID, backfill)
			if err != nil {
				return hub.Room{}, &controlError{http.StatusBadGateway, "Could not read " + ref.Provider + " history"}
			}
			binding.Backfill = history
		}
		bindings = append(bindings, binding)
	}
	if err := ctx.Err(); err != nil {
		return hub.Room{}, err
	}
	var room hub.Room
	var err error
	if roomID == "" {
		room, err = s.hub.BindRoom(d.workspace, bindings)
	} else {
		if !slices.ContainsFunc(s.hub.Rooms(d.workspace), func(room hub.Room) bool { return room.ID == roomID }) {
			return hub.Room{}, hub.ErrRoomNotFound
		}
		room, err = s.hub.JoinRoom(roomID, bindings)
	}
	if err == nil {
		s.syncWatches()
	}
	return room, err
}

func (s *Server) Manage(ctx context.Context, req mcp.ManageRequest) (any, error) {
	workspace, err := s.requestWorkspace(firstNonempty(req.Workspace, req.HostWorkspace, req.QueryWorkspace))
	if err != nil {
		return nil, &controlError{http.StatusBadRequest, "Invalid workspace"}
	}
	d := discovery{s, workspace, make(map[string][]adapter.Session)}
	self, identityErr := s.identifyCaller(d, req)
	if req.Action != "list" && identityErr != nil {
		return nil, identityErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var room hub.Room
	var participant hub.Participant
	bound := false
	if self != nil {
		room, participant, bound = s.binding(*self)
		if bound && room.Workspace != workspace {
			return nil, errors.New("Current session is bound in a different workspace")
		}
	}
	if req.Action == "unbind" {
		if !bound {
			return map[string]bool{"unbound": true}, nil
		}
		if err := s.hub.UnbindParticipant(room.ID, participant.ID); err != nil {
			return nil, err
		}
		delete(s.notices, participant.ID)
		if len(room.Participants) == 2 {
			for _, member := range room.Participants {
				delete(s.notices, member.ID)
			}
		}
		s.syncWatches()
		return map[string]bool{"unbound": true}, nil
	}

	provider := callerProvider(req)
	if self != nil {
		provider = self.Provider
	}
	peers, err := s.peerProviders(provider, req.TargetProvider)
	if err != nil {
		return nil, err
	}
	if req.Action == "list" {
		candidates, err := s.listPeers(d, peers)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"self": self, "bound": bound, "candidates": candidates}
		if bound {
			result["room"] = room
		}
		if identityErr != nil {
			result["bound"] = nil
			result["identity_error"] = identityErr.Error()
		}
		return result, nil
	}
	if len(peers) != 1 {
		return nil, errors.New("Specify target_provider to select a peer")
	}
	since := time.Now()
	target := sessionRef{peers[0], req.TargetSession}
	if target.SessionID == "latest" {
		target, err = d.latest(target.Provider, req.BackfillTurns)
		if err != nil {
			return nil, err
		}
	}
	targetRoom, _, targetBound := s.binding(target)
	if targetBound && targetRoom.Workspace != workspace {
		return nil, errors.New("Peer is bound in a different workspace")
	}
	if bound && targetBound {
		if room.ID != targetRoom.ID {
			return nil, errors.New("Current session and peer belong to different rooms")
		}
		return map[string]any{"bound": true, "room": room}, nil
	}
	var refs []sessionRef
	if !bound {
		refs = append(refs, *self)
	}
	if !targetBound {
		refs = append(refs, target)
	}
	room, err = s.commitBinding(ctx, d, since, firstNonempty(room.ID, targetRoom.ID), refs, req.BackfillTurns)
	if err != nil {
		return nil, err
	}
	return map[string]any{"bound": true, "room": room}, nil
}

func callerProvider(req mcp.ManageRequest) string {
	switch {
	case req.ConversationID != "":
		return "antigravity"
	case req.ThreadID != "":
		return "codex"
	default:
		return req.QueryProvider
	}
}

func (s *Server) identifyCaller(d discovery, req mcp.ManageRequest) (*sessionRef, error) {
	provider := callerProvider(req)
	id := firstNonempty(req.ConversationID, req.ThreadID, req.SessionID, req.QuerySession, req.CallerSession)
	if id == "" && (provider == "" || provider == "codex") {
		// A workspace-unique active room suffices for passive reads, but
		// cannot identify which native session authorized a room mutation.
		if req.RequestID == "" {
			return nil, errors.New("Current Codex session identity is unknown; supply caller_session_id from the host or a fresh request_id")
		}
		type callerIdentifier interface {
			IdentifyToolCaller(workspace, toolName, requestID string) (string, error)
		}
		if cx, ok := s.adapters["codex"].(callerIdentifier); ok {
			found, err := cx.IdentifyToolCaller(d.workspace, "bridge_manage", req.RequestID)
			if err != nil {
				return nil, err
			}
			provider, id = "codex", found
		}
	}
	if id == "" {
		return nil, errors.New("Unable to identify the current session; configure the native session ID")
	}
	if provider != "" {
		ref := sessionRef{provider, id}
		if _, _, bound := s.binding(ref); bound {
			return &ref, nil
		}
		if _, err := d.selectSession(ref, 0); err != nil {
			return nil, err
		}
		return &ref, nil
	}
	var match *sessionRef
	for candidate := range s.adapters {
		ref := sessionRef{candidate, id}
		// Pending Antigravity IDs are only trustworthy with a provider hint.
		sessions, err := d.list(candidate)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(sessions, func(x adapter.Session) bool { return x.ID == id }) {
			continue
		}
		if match != nil {
			return nil, errors.New("Session ID matches more than one provider; specify the calling provider")
		}
		match = &ref
	}
	if match == nil {
		return nil, errors.New("Current session is unavailable in this workspace")
	}
	return match, nil
}

func (s *Server) peerProviders(caller, target string) ([]string, error) {
	if target != "" {
		if s.adapters[target] == nil || caller == target {
			return nil, errors.New("target_provider must identify a different, supported provider")
		}
		return []string{target}, nil
	}
	var peers []string
	for provider := range s.adapters {
		if provider != caller {
			peers = append(peers, provider)
		}
	}
	slices.Sort(peers)
	return peers, nil
}

func (s *Server) listPeers(d discovery, providers []string) ([]peerSession, error) {
	result := make([]peerSession, 0)
	for _, provider := range providers {
		found, err := d.list(provider)
		if err != nil {
			return nil, err
		}
		for _, session := range found {
			ref := sessionRef{provider, session.ID}
			_, _, bound := s.binding(ref)
			result = append(result, peerSession{ref, session.Title, session.UpdatedAt, bound, session.Pending})
		}
	}
	slices.SortStableFunc(result, func(a, b peerSession) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return result, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
