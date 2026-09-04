package bots

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

type fixture struct {
	srv   *Server
	mux   *http.ServeMux
	st    *store.Store
	user  *store.User
	bot   *store.Bot
	token string
	notes *noteSink
}

type noteSink struct {
	mu   sync.Mutex
	sent []string
}

func (n *noteSink) fn(ctx context.Context, userID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, text)
	return nil
}

func (n *noteSink) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "bots-api@example.com", "password1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetModuleEnabled(ctx, u.ID, "bots", true); err != nil {
		t.Fatal(err)
	}
	b, token, err := st.CreateBot(ctx, u.ID, "test-bot", "")
	if err != nil {
		t.Fatal(err)
	}
	sink := &noteSink{}
	srv := &Server{Store: st, Watch: NewWatcher(), Notify: sink.fn}
	mux := http.NewServeMux()
	srv.Routes(mux)
	return &fixture{srv: srv, mux: mux, st: st, user: u, bot: b, token: token, notes: sink}
}

func (f *fixture) ctx() context.Context { return context.Background() }

func (f *fixture) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestBotAPIRequiresToken(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/bots/register"},
		{"POST", "/api/bots/heartbeat"},
		{"GET", "/api/bots/chats"},
		{"POST", "/api/bots/chats/pending"},
	} {
		if w, _ := f.do(t, tc.method, tc.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: %d", tc.method, tc.path, w.Code)
		}
		if w, _ := f.do(t, tc.method, tc.path, "bogus", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with bad token: %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestBotAPIModuleDisabled(t *testing.T) {
	f := newFixture(t)
	if err := f.st.SetModuleEnabled(context.Background(), f.user.ID, "bots", false); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.do(t, "POST", "/api/bots/heartbeat", f.token, ""); w.Code != http.StatusForbidden {
		t.Fatalf("disabled module should 403, got %d", w.Code)
	}
}

func TestBotRegisterIsIdempotent(t *testing.T) {
	f := newFixture(t)
	body := `{"name":"test-bot","bot_username":"@atlas_bot","machine":"vps2","version":"0.1.0"}`
	w, out := f.do(t, "POST", "/api/bots/register", f.token, body)
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	bot, _ := out["bot"].(map[string]any)
	if bot["name"] != "test-bot" || bot["bot_username"] != "atlas_bot" || bot["machine"] != "vps2" {
		t.Fatalf("register payload: %v", out)
	}
	if _, ok := out["chats"]; !ok {
		t.Fatalf("register must return the whitelist: %v", out)
	}

	// Second call: same bot, no duplicate.
	if w, _ := f.do(t, "POST", "/api/bots/register", f.token, body); w.Code != http.StatusOK {
		t.Fatalf("second register: %d", w.Code)
	}
	list, err := f.st.ListBots(context.Background(), f.user.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 bot, err=%v n=%d", err, len(list))
	}

	// A mismatched name is reported back, not silently applied.
	_, out = f.do(t, "POST", "/api/bots/register", f.token, `{"name":"Renamed"}`)
	if note, _ := out["note"].(string); !strings.Contains(note, "test-bot") {
		t.Fatalf("expected a name mismatch note, got %v", out)
	}
}

func TestReportPendingNotifiesOnceAndApprovalFlows(t *testing.T) {
	f := newFixture(t)

	body := `{"chat_id":"282611642","type":"private","first_name":"Jairo","username":"kidandcat","first_message":"hola"}`
	w, out := f.do(t, "POST", "/api/bots/chats/pending", f.token, body)
	if w.Code != http.StatusCreated || out["created"] != true {
		t.Fatalf("first report: %d %v", w.Code, out)
	}
	chat, _ := out["chat"].(map[string]any)
	if chat["status"] != store.BotChatPending || chat["title"] != "Jairo" {
		t.Fatalf("chat payload: %v", chat)
	}
	if f.notes.count() != 1 {
		t.Fatalf("expected one telegram notification, got %d", f.notes.count())
	}
	if !strings.Contains(f.notes.sent[0], "test-bot") || !strings.Contains(f.notes.sent[0], "hola") {
		t.Fatalf("notification text: %q", f.notes.sent[0])
	}

	// Repeat report: idempotent, no second notification.
	w, out = f.do(t, "POST", "/api/bots/chats/pending", f.token, body)
	if w.Code != http.StatusOK || out["created"] != false {
		t.Fatalf("repeat report: %d %v", w.Code, out)
	}
	if f.notes.count() != 1 {
		t.Fatalf("repeat must not re-notify, got %d", f.notes.count())
	}

	// chat_id is mandatory.
	if w, _ := f.do(t, "POST", "/api/bots/chats/pending", f.token, `{"type":"private"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing chat_id should 400, got %d", w.Code)
	}

	// Whitelist shows it as pending.
	_, out = f.do(t, "GET", "/api/bots/chats?status=pending", f.token, "")
	chats, _ := out["chats"].([]any)
	if len(chats) != 1 {
		t.Fatalf("pending list: %v", out)
	}
	cursor, _ := out["cursor"].(string)
	if cursor == "" {
		t.Fatal("cursor missing")
	}

	// Approve through the MCP tool path, then the daemon sees it as approved.
	if _, err := f.st.DecideBotChat(context.Background(), f.user.ID, f.bot.ID, "282611642", store.BotChatApproved, "mcp"); err != nil {
		t.Fatal(err)
	}
	_, out = f.do(t, "GET", "/api/bots/chats?status=approved", f.token, "")
	chats, _ = out["chats"].([]any)
	if len(chats) != 1 {
		t.Fatalf("approved list: %v", out)
	}
	got, _ := chats[0].(map[string]any)
	if got["chat_id"] != "282611642" || got["decided_by"] != "mcp" {
		t.Fatalf("approved chat: %v", got)
	}
}

func TestChatsQueryValidation(t *testing.T) {
	f := newFixture(t)
	if w, _ := f.do(t, "GET", "/api/bots/chats?status=weird", f.token, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status should 400, got %d", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/bots/chats?updated_since=yesterday", f.token, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad updated_since should 400, got %d", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/bots/chats?wait=-1", f.token, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad wait should 400, got %d", w.Code)
	}
}

func TestChatsLongPollWakesOnDecision(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.st.ReportBotChat(ctx, f.bot.ID, f.user.ID, store.BotChat{ChatID: "7", Type: "private"}); err != nil {
		t.Fatal(err)
	}
	cursor, err := f.st.BotChatsUpdatedAt(ctx, f.bot.ID)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		w, out := f.do(t, "GET", "/api/bots/chats?wait=5&updated_since="+cursor, f.token, "")
		chats, _ := out["chats"].([]any)
		if w.Code != http.StatusOK {
			done <- -1
			return
		}
		done <- len(chats)
	}()

	// Give the poller a moment to park, then decide.
	time.Sleep(1100 * time.Millisecond)
	if _, err := f.st.DecideBotChat(ctx, f.user.ID, f.bot.ID, "7", store.BotChatApproved, "panel"); err != nil {
		t.Fatal(err)
	}
	f.srv.Watch.NotifyChats(f.bot.ID)

	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("long poll should return the decided chat, got %d", n)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("long poll did not return")
	}
}

func TestHeartbeatTouchesLastSeen(t *testing.T) {
	f := newFixture(t)
	w, out := f.do(t, "POST", "/api/bots/heartbeat", f.token, "")
	if w.Code != http.StatusOK || out["ok"] != true || out["bot"] != "test-bot" {
		t.Fatalf("heartbeat: %d %v", w.Code, out)
	}
	b, err := f.st.BotByID(context.Background(), f.user.ID, f.bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.LastSeen == nil || !Online(*b) {
		t.Fatalf("bot should be online after a heartbeat: %+v", b)
	}
}
