package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveChatRejectsUnknownTargets is the regression test for arbitrary
// chat targeting. The callers that supply a chat id — the loopback API and the
// MCP tool — are reachable by the CLI agent, so an invented id would deliver
// the operator's private conversation to a stranger.
func TestResolveChatRejectsUnknownTargets(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()

	got, err := a.ResolveChat(ctx, 0)
	if err != nil || got != ownerChat {
		t.Fatalf("zero means the owner's own chat: %d %v", got, err)
	}
	if got, err := a.ResolveChat(ctx, ownerChat); err != nil || got != ownerChat {
		t.Fatalf("the owner's own id is always valid: %d %v", got, err)
	}

	if _, err := a.ResolveChat(ctx, -1002233445566); err == nil {
		t.Fatal("an unserved chat must be rejected")
	} else if !strings.Contains(err.Error(), "unknown chat") {
		t.Fatalf("the error should say why: %v", err)
	}

	// Once the assistant has actually served a group, it becomes a valid target.
	if err := a.Bot.state.See(-1002233445566, "group", "Casa"); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ResolveChat(ctx, -1002233445566); err != nil || got != -1002233445566 {
		t.Fatalf("a known group must be accepted: %d %v", got, err)
	}
}

// TestResolveOutboundFileConfinesUploads is the regression test for file
// exfiltration: atlas-send takes a path from the CLI agent, so without this the
// agent could send /etc/takan/takan.env or the SQLite database to Telegram.
func TestResolveOutboundFileConfinesUploads(t *testing.T) {
	a := newTestAssistant(t)

	inside := filepath.Join(a.Workdir(), "chart.png")
	if err := os.WriteFile(inside, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ResolveOutboundFile(inside); err != nil || got == "" {
		t.Fatalf("a workspace file must be allowed: %q %v", got, err)
	}

	outside := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(outside, []byte("ATLAS_SESSION_KEY=..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResolveOutboundFile(outside); err == nil {
		t.Fatal("a file outside the data directory must be refused")
	}

	// Traversal must not get back out.
	traversal := filepath.Join(a.Workdir(), "..", "..", filepath.Base(outside))
	if _, err := a.ResolveOutboundFile(traversal); err == nil {
		t.Fatal("path traversal must be refused")
	}

	// Nor must a symlink planted inside the workspace.
	link := filepath.Join(a.Workdir(), "innocent.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := a.ResolveOutboundFile(link); err == nil {
		t.Fatal("a symlink pointing outside the data directory must be refused")
	}

	if got, err := a.ResolveOutboundFile(""); err != nil || got != "" {
		t.Fatalf("no file is not an error: %q %v", got, err)
	}
	if _, err := a.ResolveOutboundFile(filepath.Join(a.Workdir(), "missing.png")); err == nil {
		t.Fatal("a missing file must be refused")
	}
	if _, err := a.ResolveOutboundFile(a.Workdir()); err == nil {
		t.Fatal("a directory must be refused")
	}
}

// TestWithinDoesNotMatchSiblingPrefixes: a plain strings.HasPrefix would let
// /opt/atlas-other through for root /opt/atlas.
func TestWithinDoesNotMatchSiblingPrefixes(t *testing.T) {
	if within("/opt/atlas", "/opt/atlas-other/file") {
		t.Fatal("a sibling directory sharing a prefix must not count as inside")
	}
	if !within("/opt/atlas", "/opt/atlas/data/file") {
		t.Fatal("a real descendant must count as inside")
	}
	if !within("/opt/atlas", "/opt/atlas") {
		t.Fatal("the root itself counts as inside")
	}
}

func TestInternalSendValidatesChatAndFile(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	mux := localMux(a)

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/send", strings.NewReader(body)))
		return rec
	}

	if rec := post(`{"text":"hola","chat_id":-1002233445566}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unserved chat must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	outside := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "look", "file": outside})
	if rec := post(string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a file outside the data dir must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	if sent := fake.Sent(); len(sent) != 0 {
		t.Fatalf("nothing should have been delivered, got %+v", sent)
	}

	if rec := post(`{"text":"hola"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("the owner's own chat must still work, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTaskAndJobAPIsValidateTheChat(t *testing.T) {
	a := newTestAssistant(t)
	mux := localMux(a)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tasks",
		strings.NewReader(`{"prompt":"do it","chat_id":-1002233445566}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a task targeting an unserved chat must be refused, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jobs",
		strings.NewReader(`{"type":"message","payload":"ping","cron":"@daily","chat_id":-1002233445566}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a job targeting an unserved chat must be refused, got %d", rec.Code)
	}
}

// TestSendTelegramValidatesTheChat covers the MCP tool path.
func TestSendTelegramValidatesTheChat(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	ctx := context.Background()

	if _, err := a.SendTelegram(ctx, -1002233445566, "hola", ""); err == nil {
		t.Fatal("telegram_send must refuse a chat the assistant has never served")
	}
	if sent := fake.Sent(); len(sent) != 0 {
		t.Fatalf("nothing should have been delivered, got %+v", sent)
	}
	if _, err := a.SendTelegram(ctx, 0, "hola", ""); err != nil {
		t.Fatalf("the owner's own chat must work: %v", err)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"Bearer abc":     {"abc", true},
		"bearer abc":     {"abc", true},
		"BEARER abc":     {"abc", true},
		"  Bearer abc  ": {"abc", true},
		// A bare token means any header value is treated as a credential.
		"abc":       {"", false},
		"":          {"", false},
		"Bearer":    {"", false},
		"Bearer ":   {"", false},
		"Basic abc": {"", false},
		"Bearerabc": {"", false},
	}
	for header, want := range cases {
		got, ok := bearerToken(header)
		if ok != want.ok || got != want.want {
			t.Fatalf("bearerToken(%q) = %q,%v want %q,%v", header, got, ok, want.want, want.ok)
		}
	}
}

func TestAppAuthRejectsABareToken(t *testing.T) {
	a := newTestAssistant(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("Authorization", "test-token") // no scheme
	rec := httptest.NewRecorder()
	appMux(a).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a credential with no scheme must be rejected, got %d", rec.Code)
	}
}

// TestUnknownCursorIs404: a cursor that has aged out of the log used to fall
// back to the newest or oldest window, so a resuming client silently jumped
// somewhere else in the conversation.
func TestUnknownCursorIs404(t *testing.T) {
	a := newTestAssistant(t)
	if _, err := a.Bot.history.Append(HistoryMessage{Role: RoleUser, Text: "x", Source: SourceApp}); err != nil {
		t.Fatal(err)
	}
	mux := appMux(a)

	for _, query := range []string{"?after=nope", "?before=nope"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/messages"+query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d %s", query, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "unknown cursor") {
			t.Fatalf("%s: expected an explicit reason, got %s", query, rec.Body.String())
		}
	}
}
