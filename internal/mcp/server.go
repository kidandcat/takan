package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Protocol version we advertise (Streamable HTTP + listChanged).
const protocolVersion = "2025-03-26"

// UserResolver maps a bearer token to a user id.
type UserResolver func(ctx context.Context, bearer string) (userID string, err error)

// ToolProvider returns tools for a user (enabled modules only).
type ToolProvider func(ctx context.Context, userID string) []RegisteredTool

// ToolHandler executes a tool for a user.
type ToolHandler func(ctx context.Context, userID string, args map[string]any) (string, error)

// RegisteredTool is a tool + handler.
type RegisteredTool struct {
	Tool
	Handler ToolHandler
}

// Server is multi-tenant Streamable HTTP MCP.
type Server struct {
	Name      string
	PublicURL string // for WWW-Authenticate resource_metadata
	Resolve   UserResolver
	ToolsFor  ToolProvider
	Sessions  *SessionHub
}

func (s *Server) hub() *SessionHub {
	if s.Sessions == nil {
		s.Sessions = NewSessionHub()
	}
	return s.Sessions
}

// NotifyToolsChanged pushes notifications/tools/list_changed to open SSE
// streams for this user (panel module toggles, etc.).
func (s *Server) NotifyToolsChanged(userID string) {
	s.hub().NotifyToolsChanged(userID)
}

func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", "POST, GET, OPTIONS, DELETE")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodDelete:
		s.handleDelete(w, r)
		return
	case http.MethodGet:
		s.handleGET(w, r)
		return
	case http.MethodPost:
		s.handlePOST(w, r)
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
}

func (s *Server) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	bearer := bearerFrom(r)
	userID, err := s.Resolve(r.Context(), bearer)
	if err != nil || userID == "" {
		meta := strings.TrimRight(s.PublicURL, "/") + "/.well-known/oauth-protected-resource"
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="takan", resource_metadata="`+meta+`"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return userID, true
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authUser(w, r); !ok {
		return
	}
	if sid := r.Header.Get("MCP-Session-Id"); sid != "" {
		s.hub().Delete(sid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGET opens an SSE stream for server→client notifications (list_changed).
func (s *Server) handleGET(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authUser(w, r)
	if !ok {
		return
	}
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "text/event-stream") && accept != "" && !strings.Contains(accept, "*/*") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sid := r.Header.Get("MCP-Session-Id")
	var sess *Session
	if sid != "" {
		sess = s.hub().Get(sid)
		if sess == nil || sess.UserID != userID {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	} else {
		// Allow GET without session for clients that only want a listen channel;
		// bind a new session.
		sess = s.hub().Create(userID)
		w.Header().Set("MCP-Session-Id", sess.ID)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Prime stream (spec recommendation).
	_ = writeSSE(w, flusher, uuidEventID(), []byte{})

	ch := sess.addStream()
	defer sess.removeStream(ch)

	ctx := r.Context()
	// Keepalive comments so proxies don't kill idle streams.
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, flusher, uuidEventID(), msg); err != nil {
				return
			}
		}
	}
}

func (s *Server) handlePOST(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authUser(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeErr(w, nil, -32700, "read body")
		return
	}
	if strings.TrimSpace(string(raw)) == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, nil, -32700, "parse")
		return
	}
	if isNotif(req) {
		// e.g. notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Session binding
	sid := r.Header.Get("MCP-Session-Id")
	var sess *Session
	if req.Method == "initialize" {
		sess = s.hub().Create(userID)
		w.Header().Set("MCP-Session-Id", sess.ID)
	} else if sid != "" {
		sess = s.hub().Get(sid)
		if sess == nil || sess.UserID != userID {
			// Stale session — client should re-initialize
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		w.Header().Set("MCP-Session-Id", sess.ID)
	}

	writeJSON(w, s.handle(r.Context(), userID, req))
}

func (s *Server) handle(ctx context.Context, userID string, req Request) Response {
	switch req.Method {
	case "initialize":
		return Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": s.Name, "version": "0.2.0"},
		}}
	case "ping":
		return Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "notifications/initialized", "notifications/cancelled":
		return Response{JSONRPC: "2.0", ID: req.ID}
	case "tools/list":
		tools := s.ToolsFor(ctx, userID)
		out := make([]Tool, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Tool)
		}
		return Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": out}}
	case "tools/call":
		var p ToolCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, -32602, "invalid params")
		}
		var handler ToolHandler
		for _, t := range s.ToolsFor(ctx, userID) {
			if t.Name == p.Name {
				handler = t.Handler
				break
			}
		}
		if handler == nil {
			return errResp(req.ID, -32601, "unknown tool: "+p.Name)
		}
		text, err := handler(ctx, userID, p.Arguments)
		if err != nil {
			log.Printf("tool %s user=%s: %v", p.Name, userID, err)
			return Response{JSONRPC: "2.0", ID: req.ID, Result: CallToolResult{
				Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
				IsError: true,
			}}
		}
		return Response{JSONRPC: "2.0", ID: req.ID, Result: CallToolResult{
			Content: []Content{{Type: "text", Text: text}},
		}}
	default:
		return errResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func bearerFrom(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

func isNotif(req Request) bool {
	return len(req.ID) == 0 || string(req.ID) == "null"
}

func errResp(id json.RawMessage, code int, msg string) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		log.Printf("mcp write: %v", err)
	}
}

func writeErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, errResp(id, code, msg))
}

func uuidEventID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
