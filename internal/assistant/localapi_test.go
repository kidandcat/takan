package assistant

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func localMux(a *Assistant) *http.ServeMux {
	mux := http.NewServeMux()
	a.LocalRoutes(mux)
	return mux
}

func getHealth(t *testing.T, a *Assistant) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	localMux(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health body: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, payload
}

func TestLocalHealthReportsAStoppedPoller(t *testing.T) {
	a := newTestAssistant(t)

	code, payload := getHealth(t, a)
	if code != http.StatusOK || payload["status"] != "ok" {
		t.Fatalf("a fresh assistant is healthy: %d %v", code, payload)
	}

	// A rejected bot token ends the poll loop for good. Waiting out the stall
	// threshold before saying so is five minutes of a silently dead assistant.
	a.Bot.markStopped(errors.New("getMe: telegram getMe failed (401): Unauthorized"))

	code, payload = getHealth(t, a)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a stopped poller must page immediately, got %d", code)
	}
	if payload["status"] != "stopped" {
		t.Fatalf("status should say stopped, got %v", payload["status"])
	}
	if msg, _ := payload["error"].(string); msg == "" {
		t.Fatal("health must carry the reason so the journal is not the only clue")
	}
}

func TestLocalHealthReportsDisabled(t *testing.T) {
	a := newTestAssistant(t)
	a.Opts.Enabled = false

	// Deliberately off is not a failure: it must not page.
	code, payload := getHealth(t, a)
	if code != http.StatusOK || payload["status"] != "disabled" {
		t.Fatalf("a disabled assistant is healthy but says so: %d %v", code, payload)
	}
}

func TestLocalSendGoesToTelegramAndTheApp(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	mux := localMux(a)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/send",
		jsonBody(`{"text":"the backup finished"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if got := fake.TextsTo(ownerChat); len(got) != 1 || got[0] != "the backup finished" {
		t.Fatalf("Telegram did not receive it: %v", got)
	}
	if texts := historyTexts(a.Bot); !containsText(texts, "the backup finished") {
		t.Fatalf("the app history did not receive it: %v", texts)
	}

	// An empty request is a caller bug, not a blank message.
	empty := httptest.NewRecorder()
	mux.ServeHTTP(empty, httptest.NewRequest(http.MethodPost, "/internal/send", jsonBody(`{}`)))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty send, got %d", empty.Code)
	}
}
