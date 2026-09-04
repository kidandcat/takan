package tg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
)

// editServer stands in for the Bot API, recording what it was asked and
// answering with whatever the test queued.
type editServer struct {
	mu       sync.Mutex
	calls    []map[string]any
	response string
}

func newEditServer(t *testing.T, response string) (*editServer, *Client) {
	t.Helper()
	s := &editServer{response: response}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["_method"] = path.Base(r.URL.Path)
		s.mu.Lock()
		s.calls = append(s.calls, body)
		reply := s.response
		s.mu.Unlock()
		if reply == "" {
			reply = `{"ok":true,"result":{"message_id":7}}`
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return s, NewWithBase("test-token", srv.URL)
}

func (s *editServer) recorded() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.calls...)
}

func (s *editServer) answerWith(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.response = reply
}

func TestEditMessageTextSendsTheRightCall(t *testing.T) {
	srv, client := newEditServer(t, "")
	if err := client.EditMessageText(context.Background(), 42, 7, "hola", "plain"); err != nil {
		t.Fatal(err)
	}
	calls := srv.recorded()
	if len(calls) != 1 || calls[0]["_method"] != "editMessageText" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
	if calls[0]["message_id"] != float64(7) || calls[0]["text"] != "hola" {
		t.Fatalf("unexpected payload: %+v", calls[0])
	}
	if _, ok := calls[0]["parse_mode"]; ok {
		t.Fatalf("a plain edit must not ask for markup: %+v", calls[0])
	}
}

// TestEditMessageTextIgnoresNotModified: the progress loop asks for a state the
// chat may already be in. Telegram calls that an error; the caller should not.
func TestEditMessageTextIgnoresNotModified(t *testing.T) {
	_, client := newEditServer(t,
		`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
	if err := client.EditMessageText(context.Background(), 42, 7, "same text", "plain"); err != nil {
		t.Fatalf("a no-op edit is not a failure: %v", err)
	}
}

// TestEditMessageTextSurfacesRetryAfter is what lets the caller pace itself: a
// 429 has to arrive as a typed error carrying the wait Telegram asked for.
func TestEditMessageTextSurfacesRetryAfter(t *testing.T) {
	srv, client := newEditServer(t,
		`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 12","parameters":{"retry_after":12}}`)
	err := client.EditMessageText(context.Background(), 42, 7, "hola", "Markdown")

	var apiErr *APIError
	if !AsAPIError(err, &apiErr) {
		t.Fatalf("expected a typed API error, got %v", err)
	}
	if apiErr.Code != 429 || apiErr.RetryDelay().Seconds() != 12 {
		t.Fatalf("retry_after was lost: %+v (%s)", apiErr, apiErr.RetryDelay())
	}
	// A rate limit must not be retried here as though it were a markup problem.
	if calls := srv.recorded(); len(calls) != 1 {
		t.Fatalf("a 429 must not be retried by the client: %+v", calls)
	}
}

// TestEditMessageTextRetriesWithoutMarkup keeps a task result deliverable when
// the agent's Markdown is not quite Telegram's.
func TestEditMessageTextRetriesWithoutMarkup(t *testing.T) {
	srv, client := newEditServer(t,
		`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
	_ = client.EditMessageText(context.Background(), 42, 7, "**bold*", "Markdown")

	calls := srv.recorded()
	if len(calls) != 2 {
		t.Fatalf("expected a retry without parse_mode, got %d call(s): %+v", len(calls), calls)
	}
	if _, ok := calls[0]["parse_mode"]; !ok {
		t.Fatalf("the first attempt should have carried markup: %+v", calls[0])
	}
	if _, ok := calls[1]["parse_mode"]; ok {
		t.Fatalf("the retry must be unformatted: %+v", calls[1])
	}
}

func TestEditMessageTextRefusesAnOversizedBody(t *testing.T) {
	srv, client := newEditServer(t, "")
	err := client.EditMessageText(context.Background(), 42, 7, strings.Repeat("x", MaxMessageRunes+1), "plain")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected a clear size error, got %v", err)
	}
	if calls := srv.recorded(); len(calls) != 0 {
		t.Fatalf("nothing should have been sent: %+v", calls)
	}
}

func TestDeleteMessage(t *testing.T) {
	srv, client := newEditServer(t, `{"ok":true,"result":true}`)
	if err := client.DeleteMessage(context.Background(), 42, 7); err != nil {
		t.Fatal(err)
	}
	calls := srv.recorded()
	if len(calls) != 1 || calls[0]["_method"] != "deleteMessage" || calls[0]["message_id"] != float64(7) {
		t.Fatalf("unexpected call: %+v", calls)
	}

	// A message that is already gone is the state the caller wanted.
	srv.answerWith(`{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`)
	if err := client.DeleteMessage(context.Background(), 42, 7); err != nil {
		t.Fatalf("deleting an absent message is not a failure: %v", err)
	}
}
