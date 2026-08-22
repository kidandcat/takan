package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testMCPServer() *Server {
	return &Server{
		Name:      "takan-test",
		PublicURL: "http://example.test",
		Resolve: func(_ context.Context, bearer string) (string, error) {
			switch bearer {
			case "alice":
				return "user-alice", nil
			case "bob":
				return "user-bob", nil
			default:
				return "", errors.New("unauthorized")
			}
		},
		ToolsFor: func(_ context.Context, _ string) []RegisteredTool {
			return []RegisteredTool{{
				Tool: Tool{
					Name:        "ping_tool",
					Description: "ping",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(_ context.Context, _ string, _ map[string]any) (string, error) {
					return "pong", nil
				},
			}}
		},
		Sessions: NewSessionHub(),
	}
}

func postMCP(t *testing.T, s *Server, bearer, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	s.HandleHTTP(rec, req)
	return rec
}

func rpcResult(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "unknown session") {
		t.Fatalf("stale session 404 leaked: %s", rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v body=%s", err, rec.Body.String())
	}
	if errObj, ok := out["error"]; ok && errObj != nil {
		t.Fatalf("rpc error: %v", errObj)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("missing result: %s", rec.Body.String())
	}
	return result
}

func TestStaleSessionToolsListRecreates(t *testing.T) {
	s := testMCPServer()
	stale := "00000000-0000-0000-0000-000000000000"
	rec := postMCP(t, s, "alice", stale, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result := rpcResult(t, rec)
	newSID := rec.Header().Get("MCP-Session-Id")
	if newSID == "" {
		t.Fatal("expected MCP-Session-Id response header")
	}
	if newSID == stale {
		t.Fatal("replacement session id must differ from the stale id")
	}
	sess := s.hub().Get(newSID)
	if sess == nil || sess.UserID != "user-alice" {
		t.Fatalf("hub session=%+v", sess)
	}
	if s.hub().Get(stale) != nil {
		t.Fatal("stale id must not be inserted into the hub")
	}
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", result["tools"])
	}
	name, _ := tools[0].(map[string]any)["name"].(string)
	if name != "ping_tool" {
		t.Fatalf("tool name=%q", name)
	}
}

func TestStaleSessionToolsCallRecreates(t *testing.T) {
	s := testMCPServer()
	stale := "dead-session-after-restart"
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping_tool","arguments":{}}}`
	rec := postMCP(t, s, "alice", stale, body)
	result := rpcResult(t, rec)
	newSID := rec.Header().Get("MCP-Session-Id")
	if newSID == "" || newSID == stale {
		t.Fatalf("MCP-Session-Id=%q", newSID)
	}
	if s.hub().Get(newSID) == nil {
		t.Fatal("replacement session missing from hub")
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("content=%v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if text != "pong" {
		t.Fatalf("text=%q", text)
	}
}

func TestStaleSessionPingRecreates(t *testing.T) {
	s := testMCPServer()
	stale := "stale-ping"
	rec := postMCP(t, s, "alice", stale, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	_ = rpcResult(t, rec)
	newSID := rec.Header().Get("MCP-Session-Id")
	if newSID == "" || newSID == stale {
		t.Fatalf("MCP-Session-Id=%q", newSID)
	}
}

func TestForeignSessionDoesNotSteal(t *testing.T) {
	s := testMCPServer()
	bobSess := s.hub().Create("user-bob")
	rec := postMCP(t, s, "alice", bobSess.ID, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_ = rpcResult(t, rec)
	newSID := rec.Header().Get("MCP-Session-Id")
	if newSID == "" || newSID == bobSess.ID {
		t.Fatalf("must mint a session for alice, got %q", newSID)
	}
	if got := s.hub().Get(bobSess.ID); got == nil || got.UserID != "user-bob" {
		t.Fatal("bob session must remain")
	}
	if got := s.hub().Get(newSID); got == nil || got.UserID != "user-alice" {
		t.Fatalf("alice session=%+v", got)
	}
}

func TestKnownSessionPreserved(t *testing.T) {
	s := testMCPServer()
	sess := s.hub().Create("user-alice")
	rec := postMCP(t, s, "alice", sess.ID, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	_ = rpcResult(t, rec)
	if got := rec.Header().Get("MCP-Session-Id"); got != sess.ID {
		t.Fatalf("MCP-Session-Id=%q want %q", got, sess.ID)
	}
}

func TestInitializeAlwaysCreatesNewSession(t *testing.T) {
	s := testMCPServer()
	old := s.hub().Create("user-alice")
	rec := postMCP(t, s, "alice", old.ID, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_ = rpcResult(t, rec)
	newSID := rec.Header().Get("MCP-Session-Id")
	if newSID == "" || newSID == old.ID {
		t.Fatalf("initialize must mint a new session, got %q", newSID)
	}
}

func TestStaleSessionStillRequiresAuth(t *testing.T) {
	s := testMCPServer()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("MCP-Session-Id", "stale-id")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("MCP-Session-Id") != "" {
		t.Fatal("must not mint a session for an unauthenticated request")
	}
}

func TestGETStaleSessionRecreates(t *testing.T) {
	s := testMCPServer()
	hs := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stale := "sse-stale-after-restart"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("MCP-Session-Id", stale)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	newSID := resp.Header.Get("MCP-Session-Id")
	if newSID == "" || newSID == stale {
		t.Fatalf("MCP-Session-Id=%q", newSID)
	}
	if sess := s.hub().Get(newSID); sess == nil || sess.UserID != "user-alice" {
		t.Fatalf("hub session missing for %s", newSID)
	}
	cancel()
}
