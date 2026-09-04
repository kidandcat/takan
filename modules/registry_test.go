package modules

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/store"
	assistantmod "github.com/kidandcat/takan/modules/assistant"
)

func TestCatalogShape(t *testing.T) {
	ids := map[string]string{}
	for _, c := range Catalog {
		if c.Name == "" || c.Description == "" {
			t.Fatalf("catalog entry %q needs a name and a description", c.ID)
		}
		if _, dup := ids[c.ID]; dup {
			t.Fatalf("duplicate catalog id %q", c.ID)
		}
		ids[c.ID] = c.Name
	}
	for _, want := range []string{"assistant", "machine", "display", "tv", "mercadona", "email", "people", "health", "vault"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("module %q missing from the catalog", want)
		}
	}
	// The bot fleet, the Telegram channel layer and SIP were retired together.
	for _, gone := range []string{"bots", "telegram", "sip"} {
		if _, ok := ids[gone]; ok {
			t.Fatalf("module %q should be gone from the catalog", gone)
		}
	}
	if ids["assistant"] != "Assistant" {
		t.Fatalf("assistant name: %q", ids["assistant"])
	}
}

// stubSender records what telegram_send would have delivered.
type stubSender struct {
	chatID    int64
	text      string
	parseMode string
}

func (s *stubSender) SendTelegram(_ context.Context, chatID int64, text, parseMode string) (int64, error) {
	s.chatID, s.text, s.parseMode = chatID, text, parseMode
	return 42, nil
}

func newProviderOwner(t *testing.T) (*store.Store, string, context.Context) {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return st, owner.ID, ctx
}

func TestAssistantToolsAreGatedByTheModule(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	sender := &stubSender{}
	p := &Provider{Store: st, Assistant: assistantmod.Factory(sender)}

	for _, name := range namesOf(p.ToolsFor(ctx, userID)) {
		if name == "telegram_send" {
			t.Fatal("telegram_send must not appear while the module is off")
		}
	}
	if err := st.SetModuleEnabled(ctx, userID, "assistant", true); err != nil {
		t.Fatal(err)
	}
	on := namesOf(p.ToolsFor(ctx, userID))
	if !contains(on, "telegram_send") {
		t.Fatalf("telegram_send missing from %v", on)
	}
}

// TestRetiredToolsAreGone pins the surface other agents see: the bot registry
// and the channel picker no longer exist.
func TestRetiredToolsAreGone(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	p := &Provider{Store: st, Assistant: assistantmod.Factory(&stubSender{})}
	for _, mod := range []string{"assistant", "machine", "vault", "people", "health"} {
		if err := st.SetModuleEnabled(ctx, userID, mod, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range namesOf(p.ToolsFor(ctx, userID)) {
		if strings.HasPrefix(name, "bots_") || name == "telegram_chats" {
			t.Fatalf("retired tool %q is still exposed", name)
		}
	}
}

func TestTelegramSendDefaultsToTheOperatorChat(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	if err := st.SetModuleEnabled(ctx, userID, "assistant", true); err != nil {
		t.Fatal(err)
	}
	sender := &stubSender{}
	tools := (&Provider{Store: st, Assistant: assistantmod.Factory(sender)}).ToolsFor(ctx, userID)

	var send func(context.Context, string, map[string]any) (string, error)
	for _, tl := range tools {
		if tl.Name == "telegram_send" {
			send = tl.Handler
		}
	}
	if send == nil {
		t.Fatal("telegram_send is not registered")
	}

	out, err := send(ctx, userID, map[string]any{"text": "hola"})
	if err != nil {
		t.Fatal(err)
	}
	if sender.chatID != 0 {
		t.Fatalf("an omitted chat_id means the operator's own chat, got %d", sender.chatID)
	}
	if !strings.Contains(out, `"message_id": 42`) {
		t.Fatalf("the tool should report the message id: %s", out)
	}

	if _, err := send(ctx, userID, map[string]any{"text": "hola", "chat_id": "-1002233445566"}); err != nil {
		t.Fatal(err)
	}
	if sender.chatID != -1002233445566 {
		t.Fatalf("an explicit chat_id must be honoured, got %d", sender.chatID)
	}

	// A non-numeric chat id is a caller mistake, not a silent fallback to the
	// operator's chat — that would send private text to the wrong place.
	if _, err := send(ctx, userID, map[string]any{"text": "hola", "chat_id": "not-a-number"}); err == nil {
		t.Fatal("a malformed chat_id must be rejected")
	}
}

// TestAssistantReadinessIsReported keeps takan_status useful: it is how another
// agent finds out the assistant is not receiving anything.
func TestAssistantReadinessIsReported(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	if err := st.SetModuleEnabled(ctx, userID, "assistant", true); err != nil {
		t.Fatal(err)
	}

	// With no assistant wired, the status must say why rather than claim ready.
	p := &Provider{Store: st}
	status, err := p.StatusJSON(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "assistant not running") {
		t.Fatalf("status should explain a missing assistant:\n%s", status)
	}

	p.AssistantStatus = func(context.Context) (bool, string) { return true, "@casa_bot · 2 chat(s)" }
	status, err = p.StatusJSON(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "@casa_bot") {
		t.Fatalf("status should carry the assistant detail:\n%s", status)
	}
}

func contains(have []string, want string) bool {
	for _, h := range have {
		if h == want {
			return true
		}
	}
	return false
}
