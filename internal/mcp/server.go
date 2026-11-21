package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
	"github.com/lainniay/AgentBridge/internal/hub"
)

const ProtocolVersion = "2025-11-25"
const requestIDPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

var requestID = regexp.MustCompile(requestIDPattern)

type Server struct {
	Hub       *hub.Hub
	Workspace string
	Manager   Manager
}

type Manager interface {
	Manage(context.Context, ManageRequest) (any, error)
}

type ManageRequest struct {
	Action         string `json:"action"`
	TargetProvider string `json:"target_provider"`
	TargetSession  string `json:"target_session_id"`
	BackfillTurns  int    `json:"backfill_turns"`
	Workspace      string `json:"workspace"`
	CallerSession  string `json:"caller_session_id"`
	RequestID      string `json:"request_id"`
	HostWorkspace  string `json:"_workspace"`
	ConversationID string `json:"_conversation_id"`
	SessionID      string `json:"_session_id"`
	ThreadID       string `json:"_thread_id"`
	QueryProvider  string `json:"-"`
	QuerySession   string `json:"-"`
	QueryWorkspace string `json:"-"`
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supported(version) {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{-32600, "Unsupported MCP protocol version"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil || !json.Valid(body) {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{-32700, "Invalid JSON"})
		return
	}
	var req request
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || !validID(req.ID) ||
		(req.Method == "" && (len(req.ID) == 0 || (len(req.Result) == 0) == (len(req.Error) == 0))) ||
		(req.Method != "" && (len(req.Result) != 0 || len(req.Error) != 0)) {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{-32600, "Invalid Request"})
		return
	}
	if len(req.ID) == 0 || req.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.dispatch(r, req)
	reply(w, http.StatusOK, req.ID, result, rpcErr)
}

func validID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	if len(id) == 0 {
		return true
	}
	return id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9'
}

func supported(version string) bool {
	switch version {
	case ProtocolVersion, "2025-06-18", "2025-03-26":
		return true
	}
	return false
}

func reply(w http.ResponseWriter, status int, id json.RawMessage, result any, err *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response{JSONRPC: "2.0", ID: id, Result: result, Error: err})
}

func (s Server) dispatch(r *http.Request, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if !objectParams(req.Params, &params) {
			return nil, &rpcError{-32602, "Invalid initialize parameters"}
		}
		if !supported(params.ProtocolVersion) {
			params.ProtocolVersion = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "AgentBridge", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		return s.call(r, req.Params)
	default:
		return nil, &rpcError{-32601, "Method not found"}
	}
}

func objectParams(raw json.RawMessage, target any) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || raw[0] == '{' && json.Unmarshal(raw, target) == nil
}

func (s Server) tools() []map[string]any {
	schema := func(properties map[string]any) map[string]any {
		// Hidden routing parameters may be added by the host after the tool call.
		return map[string]any{"type": "object", "properties": properties}
	}
	result := []map[string]any{
		{
			"name":        "bridge_status",
			"description": "Check room binding status, connected peers, and unread peer message counts.",
			"inputSchema": schema(map[string]any{}),
		},
		{
			"name":        "bridge_read",
			"description": "Read shared blackboard messages. Default mode='diff' returns unread peer messages and advances your read cursor. Use mode='full' with limit/after_seq/before_seq to browse history, or mode='path' for the Markdown file path.",
			"inputSchema": schema(map[string]any{
				"mode":       map[string]any{"type": "string", "enum": []string{"diff", "full", "path"}, "default": "diff", "description": "diff (default: unread peer messages), full (room history), or path (Markdown file path)"},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "description": "Maximum number of entries to return"},
				"peek":       map[string]any{"type": "boolean", "default": false, "description": "When true in diff mode, do not advance your read cursor"},
				"after_seq":  map[string]any{"type": "integer", "minimum": 1, "description": "In full mode, return entries with seq > after_seq"},
				"before_seq": map[string]any{"type": "integer", "minimum": 1, "description": "In full mode, return entries with seq < before_seq"},
			}),
		},
	}
	if s.Manager != nil {
		input := schema(map[string]any{
			"action":            map[string]any{"type": "string", "enum": []string{"list", "bind", "unbind"}, "description": "list: show peers; bind: connect to target_session_id; unbind: leave current room"},
			"target_provider":   map[string]any{"type": "string", "description": "Peer provider name, e.g. codex or antigravity"},
			"target_session_id": map[string]any{"type": "string", "description": "Peer session ID to bind, or 'latest' for the most recently active unbound peer"},
			"backfill_turns":    map[string]any{"type": "integer", "minimum": 0, "default": 0, "description": "Recent turns per session to backfill on bind (default 0)"},
			"workspace":         map[string]any{"type": "string", "description": "Optional workspace path override"},
			"caller_session_id": map[string]any{"type": "string", "description": "Calling session's own native ID from host env (never the peer ID)"},
			"request_id":        map[string]any{"type": "string", "description": "Fresh UUID for Codex caller matching when caller_session_id is unavailable", "pattern": requestIDPattern},
		})
		input["required"] = []string{"action"}
		result = append(result, map[string]any{
			"name":        "bridge_manage",
			"description": "Manage room bindings in the current workspace: list peers, bind this session to a peer, or unbind.",
			"inputSchema": input,
		})
	}
	return result
}

type readEntry struct {
	Seq          uint64              `json:"seq"`
	TS           time.Time           `json:"ts"`
	Provider     string              `json:"provider"`
	Role         string              `json:"role"`
	Content      string              `json:"content"`
	FilesTouched []string            `json:"files_touched,omitempty"`
	Handoff      *blackboard.Handoff `json:"handoff,omitempty"`
}

func compactEntries(entries []blackboard.Entry) []readEntry {
	out := make([]readEntry, len(entries))
	for i, e := range entries {
		out[i] = readEntry{
			Seq:          e.Seq,
			TS:           e.TS,
			Provider:     e.Provider,
			Role:         e.Role,
			Content:      e.Content,
			FilesTouched: e.FilesTouched,
			Handoff:      e.Handoff,
		}
	}
	return out
}

func (s Server) call(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if !objectParams(raw, &call) {
		return nil, &rpcError{-32602, "Invalid parameters"}
	}
	if call.Name == "bridge_manage" && s.Manager != nil {
		return s.manage(r, call.Arguments)
	}
	if call.Name != "bridge_status" && call.Name != "bridge_read" {
		return nil, &rpcError{-32602, "Unknown tool or invalid parameters"}
	}
	var args struct {
		ConversationID string `json:"_conversation_id"`
		SessionID      string `json:"_session_id"`
		ThreadID       string `json:"_thread_id"`
		Mode           string `json:"mode"`
		Limit          *int   `json:"limit"`
		Peek           bool   `json:"peek"`
		AfterSeq       *int64 `json:"after_seq"`
		BeforeSeq      *int64 `json:"before_seq"`
	}
	if !objectParams(call.Arguments, &args) {
		return nil, &rpcError{-32602, "Invalid tool arguments"}
	}
	opts := hub.ReadOptions{Mode: args.Mode, Peek: args.Peek}
	if call.Name == "bridge_read" {
		if args.Limit != nil {
			if *args.Limit < 1 {
				return nil, &rpcError{-32602, "limit must be positive"}
			}
			opts.Limit = *args.Limit
		}
		if args.AfterSeq != nil {
			if *args.AfterSeq < 1 {
				return nil, &rpcError{-32602, "after_seq must be positive"}
			}
			opts.AfterSeq = uint64(*args.AfterSeq)
		}
		if args.BeforeSeq != nil {
			if *args.BeforeSeq < 1 {
				return nil, &rpcError{-32602, "before_seq must be positive"}
			}
			opts.BeforeSeq = uint64(*args.BeforeSeq)
		}
		switch args.Mode {
		case "", "diff", "full", "path":
		default:
			return nil, &rpcError{-32602, "Invalid read mode"}
		}
	}
	query := r.URL.Query()
	provider, session, workspace := query.Get("provider"), query.Get("session_id"), query.Get("workspace")
	switch {
	case args.ConversationID != "":
		provider, session, workspace = "antigravity", args.ConversationID, ""
	case args.ThreadID != "":
		provider, session, workspace = "codex", args.ThreadID, ""
	case args.SessionID != "":
		provider, session, workspace = "", args.SessionID, ""
	case session == "" && workspace == "":
		workspace = s.Workspace
	}
	// Workspace-less fallback is allowed only for a concrete session identity.
	if session == "" && (workspace == "" || query.Get("require_session") == "true") {
		return toolResult(hub.Status{}, nil), nil
	}
	room, participant, ok := s.Hub.Resolve(workspace, provider, session)
	if !ok {
		return toolResult(hub.Status{}, nil), nil
	}
	if call.Name == "bridge_status" {
		status, err := s.Hub.Status(room.ID, participant.ID)
		return toolResult(status, err), nil
	}
	result, err := s.Hub.Read(room.ID, participant.ID, opts)
	if err != nil || !result.Bound {
		return toolResult(result, err), nil
	}
	if opts.Mode == "path" {
		return toolResult(map[string]any{
			"bound":      true,
			"room_id":    result.RoomID,
			"path":       result.Path,
			"latest_seq": result.LatestSeq,
		}, nil), nil
	}
	return toolResult(map[string]any{
		"bound":      true,
		"room_id":    result.RoomID,
		"cursor":     result.Cursor,
		"latest_seq": result.LatestSeq,
		"has_more":   result.HasMore,
		"entries":    compactEntries(result.Entries),
	}, nil), nil
}

func (s Server) manage(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var args ManageRequest
	if !objectParams(raw, &args) || args.BackfillTurns < 0 ||
		(args.RequestID != "" && !requestID.MatchString(args.RequestID)) {
		return nil, &rpcError{-32602, "Invalid management arguments"}
	}
	switch args.Action {
	case "list", "unbind":
	case "bind":
		if args.TargetSession == "" {
			return nil, &rpcError{-32602, "target_session_id is required for bind"}
		}
	default:
		return nil, &rpcError{-32602, "action must be list, bind, or unbind"}
	}
	q := r.URL.Query()
	args.QueryWorkspace, args.QueryProvider, args.QuerySession = q.Get("workspace"), q.Get("provider"), q.Get("session_id")
	value, err := s.Manager.Manage(r.Context(), args)
	if err != nil {
		return map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": err.Error()}}}, nil
	}
	return toolResult(value, nil), nil
}

func toolResult(value any, err error) map[string]any {
	if errors.Is(err, hub.ErrRoomNotFound) || errors.Is(err, hub.ErrParticipantNotFound) {
		value, err = hub.Status{}, nil
	}
	if err != nil {
		return map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Could not access the blackboard."}}}
	}
	payload, _ := json.Marshal(value)
	return map[string]any{
		"content":           []map[string]string{{"type": "text", "text": string(payload)}},
		"structuredContent": value,
	}
}
