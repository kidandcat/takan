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
)

const protocolVersion = "2024-11-05"

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
	Name         string
	PublicURL    string // for WWW-Authenticate resource_metadata
	Resolve      UserResolver
	ToolsFor     ToolProvider
}

func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", "POST, OPTIONS, DELETE")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bearer := bearerFrom(r)
	userID, err := s.Resolve(r.Context(), bearer)
	if err != nil || userID == "" {
		meta := strings.TrimRight(s.PublicURL, "/") + "/.well-known/oauth-protected-resource"
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="takan", resource_metadata="`+meta+`"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, s.handle(r.Context(), userID, req))
}

func (s *Server) handle(ctx context.Context, userID string, req Request) Response {
	switch req.Method {
	case "initialize":
		return Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": s.Name, "version": "0.1.0"},
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
