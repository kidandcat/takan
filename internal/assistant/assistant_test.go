package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/tg"
)

// ownerChat is the owner's Telegram user id in tests. In a private chat it is
// also the chat id, which is the whole point of the identity gate.
const ownerChat = int64(1)

// fakeTelegram is a stand-in Bot API server. Tests must never reach the real
// api.telegram.org, and asserting on what was sent is how the outbound rules
// are checked.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []sentMessage
	// nextID is the message id handed out by sendMessage, so a test can tell
	// which message an edit targets.
	nextID int64
	// failEdits, when set, makes every editMessageText answer with it.
	failEdits *fakeAPIError
}

// fakeAPIError is a Bot API refusal a test asks the server to produce.
type fakeAPIError struct {
	Code        int
	Description string
	RetryAfter  int
	// times is how many calls still fail; zero means "for ever".
	times int
}

type sentMessage struct {
	Method    string
	ChatID    int64
	Text      string
	MessageID int64
}

func (f *fakeTelegram) record(m sentMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
}

// calls counts the requests made to one Bot API method.
func (f *fakeTelegram) calls(method string) int {
	n := 0
	for _, m := range f.Sent() {
		if m.Method == method {
			n++
		}
	}
	return n
}

// lastText is the body of the most recent call to a method, "" when there was
// none.
func (f *fakeTelegram) lastText(method string) string {
	sent := f.Sent()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].Method == method {
			return sent[i].Text
		}
	}
	return ""
}

// failEditsWith makes the next n edits fail (n <= 0 means all of them).
func (f *fakeTelegram) failEditsWith(code, retryAfter, n int, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failEdits = &fakeAPIError{Code: code, Description: description, RetryAfter: retryAfter, times: n}
}

// takeEditFailure returns the refusal to answer this edit with, if any.
func (f *fakeTelegram) takeEditFailure() *fakeAPIError {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := f.failEdits
	if fail == nil {
		return nil
	}
	if fail.times > 0 {
		fail.times--
		if fail.times == 0 {
			f.failEdits = nil
		}
	}
	return fail
}

// Sent returns a copy of everything delivered so far.
func (f *fakeTelegram) Sent() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// TextsTo returns the message bodies delivered to one chat.
func (f *fakeTelegram) TextsTo(chatID int64) []string {
	var out []string
	for _, m := range f.Sent() {
		if m.ChatID == chatID {
			out = append(out, m.Text)
		}
	}
	return out
}

func newFakeTelegram(t *testing.T) (*fakeTelegram, string) {
	t.Helper()
	f := &fakeTelegram{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := path.Base(r.URL.Path)
		var body struct {
			ChatID    int64  `json:"chat_id"`
			Text      string `json:"text"`
			MessageID int64  `json:"message_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch method {
		case "sendMessage":
			f.mu.Lock()
			f.nextID++
			id := 41 + f.nextID
			f.mu.Unlock()
			f.record(sentMessage{Method: method, ChatID: body.ChatID, Text: body.Text, MessageID: id})
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
		case "editMessageText":
			f.record(sentMessage{Method: method, ChatID: body.ChatID, Text: body.Text, MessageID: body.MessageID})
			if fail := f.takeEditFailure(); fail != nil {
				_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q,"parameters":{"retry_after":%d}}`,
					fail.Code, fail.Description, fail.RetryAfter)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		case "deleteMessage":
			f.record(sentMessage{Method: method, ChatID: body.ChatID, MessageID: body.MessageID})
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":77,"is_bot":true,"username":"casa_bot"}}`)
		case "sendChatAction":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			f.record(sentMessage{Method: method, ChatID: body.ChatID, Text: body.Text})
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// newTestAssistant builds a real assistant over a temporary database, wired to
// a fake Bot API server so nothing reaches the network.
func newTestAssistant(t *testing.T) *Assistant {
	t.Helper()
	a, _ := newTestAssistantWithTelegram(t)
	return a
}

func newTestAssistantWithLegacyDir(t *testing.T, legacyDir string) *Assistant {
	t.Helper()
	a, _ := newTestAssistantOpts(t, legacyDir)
	return a
}

func newTestAssistantWithTelegram(t *testing.T) (*Assistant, *fakeTelegram) {
	t.Helper()
	return newTestAssistantOpts(t, "")
}

func newTestAssistantOpts(t *testing.T, legacyDir string) (*Assistant, *fakeTelegram) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := cryptox.NewBox("test-session-key")
	if err != nil {
		t.Fatal(err)
	}
	fake, base := newFakeTelegram(t)
	a, err := New(ctx, st, box, nil, Config{
		OwnerID:          owner.ID,
		OwnerTelegram:    ownerChat,
		DataDir:          dir,
		AgentHome:        dir,
		TelegramBotToken: "test-bot-token",
		AppToken:         "test-token",
		TelegramAPIBase:  base,
		LegacyDir:        legacyDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, fake
}

func newTestBot(t *testing.T) *Bot { return newTestAssistant(t).Bot }

// stubStart builds a startHook that fabricates a RunHandle around fn, so tests
// can drive the conversation pipeline without spawning a real agent. fn is
// given a channel that closes when the run is cancelled.
func stubStart(fn func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error)) func(context.Context, RunSpec) (*RunHandle, error) {
	return func(_ context.Context, spec RunSpec) (*RunHandle, error) {
		cancelled := make(chan struct{})
		var once sync.Once
		h := &RunHandle{
			spec:      spec,
			startedAt: time.Now(),
			done:      make(chan struct{}),
			cancel:    func() { once.Do(func() { close(cancelled) }) },
		}
		go func() {
			defer close(h.done)
			res, err := fn(spec, cancelled)
			h.mu.Lock()
			h.res, h.err = res, err
			h.mu.Unlock()
		}()
		return h, nil
	}
}

func queuedTG(texts ...string) []*queuedMsg {
	out := make([]*queuedMsg, len(texts))
	for i, text := range texts {
		out[i] = &queuedMsg{tg: &tg.Message{
			MessageID: int64(i + 1),
			Chat:      tg.Chat{ID: ownerChat, Type: "private"},
			From:      &tg.User{ID: ownerChat},
			Text:      text,
		}}
	}
	return out
}

func TestNewRefusesWithoutAnOwnerTelegramID(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, _ := cryptox.NewBox("k")

	_, err = New(ctx, st, box, nil, Config{
		OwnerID: owner.ID, DataDir: dir, TelegramBotToken: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "OWNER_TELEGRAM_ID") {
		t.Fatalf("an assistant with no owner id must fail closed, got %v", err)
	}
}

func TestBotTokenIsSealedAndReused(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, _ := st.BootstrapOwner(ctx, "owner@example.com")
	box, _ := cryptox.NewBox("test-session-key")

	base := Config{OwnerID: owner.ID, OwnerTelegram: ownerChat, DataDir: dir, AgentHome: dir}

	// First boot with the environment token seals it.
	withToken := base
	withToken.TelegramBotToken = "123456:AA-secret"
	if _, err := New(ctx, st, box, nil, withToken); err != nil {
		t.Fatal(err)
	}
	sealed, err := st.AssistantMeta(ctx, owner.ID, store.MetaBotTokenEnc)
	if err != nil || sealed == "" {
		t.Fatalf("the token should have been sealed: %q err=%v", sealed, err)
	}
	if strings.Contains(sealed, "secret") {
		t.Fatal("the stored token must be ciphertext")
	}

	// A later boot without it reads the sealed copy.
	if _, err := New(ctx, st, box, nil, base); err != nil {
		t.Fatalf("the sealed token should be reusable: %v", err)
	}

	// A wrong session key must fail loudly rather than start credential-less.
	other, _ := cryptox.NewBox("a-different-key")
	if _, err := New(ctx, st, other, nil, base); err == nil ||
		!strings.Contains(err.Error(), "could not be decrypted") {
		t.Fatalf("a wrong session key must be reported, got %v", err)
	}
}

func TestNoCredentialFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, _ := st.BootstrapOwner(ctx, "owner@example.com")
	box, _ := cryptox.NewBox("k")

	_, err = New(ctx, st, box, nil, Config{
		OwnerID: owner.ID, OwnerTelegram: ownerChat, DataDir: dir,
	})
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("no credential must be a clear startup error, got %v", err)
	}
}

func TestAgentsGuideIsWritten(t *testing.T) {
	a := newTestAssistant(t)
	raw, err := os.ReadFile(filepath.Join(a.Workdir(), "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(raw)
	for _, want := range []string{InstanceName, "atlas-task", "atlas-sched", "atlas-send", "/usage"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("AGENTS.md is missing %q", want)
		}
	}
	// The multi-chat approval prose belonged to the retired whitelist.
	for _, gone := range []string{"approved for", "pending approval"} {
		if strings.Contains(guide, gone) {
			t.Fatalf("AGENTS.md still mentions %q", gone)
		}
	}
}
