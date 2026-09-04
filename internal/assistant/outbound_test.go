package assistant

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyTexts is every assistant-authored line the app would show.
func historyTexts(b *Bot) []string {
	var out []string
	for _, m := range b.history.List("", "", 200) {
		if m.Role == RoleAssistant {
			out = append(out, m.Text)
		}
	}
	return out
}

func containsText(texts []string, want string) bool {
	for _, t := range texts {
		if strings.Contains(t, want) {
			return true
		}
	}
	return false
}

// TestOwnerChatMessagesAlwaysReachTheAppHistory is the regression test for the
// bug the choke point exists to fix: only agent replies used to be recorded, so
// reminders, routine output, slash-command answers and system notices appeared
// in Telegram but left holes in the phone app's conversation.
func TestOwnerChatMessagesAlwaysReachTheAppHistory(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	ctx := context.Background()

	// 1. A scheduled reminder.
	if err := a.Sched.execute(ctx, Job{
		ID: "j1", Type: JobMessage, Name: "dentist", Payload: "call the dentist",
	}, false); err != nil {
		t.Fatal(err)
	}
	// 2. A finished background task.
	a.TaskMgr.announce("✅ Tarea 1 — done")
	// 3. A slash-command answer.
	b.say(ctx, ownerChat, "No hay nada en marcha ahora mismo.")
	// 4. A daemon error notice.
	b.emitError(ctx, ownerChat, false, "Error interno al procesar ese mensaje.")
	// 5. A proactive atlas-send.
	if err := a.Notify(ctx, a.OwnerID, "the backup finished"); err != nil {
		t.Fatal(err)
	}
	// 6. A message another agent sent through telegram_send.
	if _, err := a.SendTelegram(ctx, 0, "desde otro agente", ""); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"call the dentist",
		"Tarea 1",
		"No hay nada en marcha",
		"Error interno",
		"the backup finished",
		"desde otro agente",
	}
	texts := historyTexts(b)
	delivered := fake.TextsTo(ownerChat)
	for _, w := range want {
		if !containsText(texts, w) {
			t.Fatalf("%q never reached the app history; the app would show a hole.\nhistory: %v", w, texts)
		}
		if !containsText(delivered, w) {
			t.Fatalf("%q never reached Telegram.\nsent: %v", w, delivered)
		}
	}
}

// TestGroupMessagesStayOutOfTheAppHistory is the other half of the rule: the
// app mirrors the owner's own conversation, not every room the assistant is in.
func TestGroupMessagesStayOutOfTheAppHistory(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	defer func() {
		if got := fake.TextsTo(groupChat); len(got) != 1 || got[0] != "algo para el grupo" {
			t.Fatalf("the group still had to receive it: %v", got)
		}
	}()

	if _, err := b.Emit(context.Background(), Outbound{
		ChatID: groupChat, Text: "algo para el grupo", Source: SourceSend,
	}); err != nil {
		t.Fatal(err)
	}
	if texts := historyTexts(b); containsText(texts, "algo para el grupo") {
		t.Fatalf("group traffic must not leak into the phone history: %v", texts)
	}
}

func TestEmitBroadcastsAndPersistsOnce(t *testing.T) {
	b := newTestBot(t)
	events := b.events.Subscribe()
	defer b.events.Unsubscribe(events)

	if _, err := b.Emit(context.Background(), Outbound{Text: "hola", Source: SourceSend}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventMessage {
			t.Fatalf("an unsolicited message defaults to %q, got %q", EventMessage, ev.Type)
		}
		if ev.Message == nil || ev.Message.Text != "hola" {
			t.Fatalf("the event must carry the stored message, got %+v", ev.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("no event was broadcast")
	}

	if texts := historyTexts(b); len(texts) != 1 || texts[0] != "hola" {
		t.Fatalf("expected exactly one stored message, got %v", texts)
	}
}

func TestEmitErrorEventCarriesTheText(t *testing.T) {
	b := newTestBot(t)
	events := b.events.Subscribe()
	defer b.events.Unsubscribe(events)

	b.emitError(context.Background(), ownerChat, true, "no he podido leer eso")

	select {
	case ev := <-events:
		if ev.Type != EventError || ev.Error != "no he podido leer eso" {
			t.Fatalf("unexpected error event: %+v", ev)
		}
		if ev.Message == nil {
			t.Fatal("an error still belongs in the conversation the app shows")
		}
	case <-time.After(time.Second):
		t.Fatal("no event was broadcast")
	}
}

func TestEmitSkipTelegramStillRecords(t *testing.T) {
	// An app-only turn must not go to Telegram, but must still be recorded.
	b := newTestBot(t)

	if _, err := b.Emit(context.Background(), Outbound{
		Text: "sólo para la app", Source: SourceApp, SkipTelegram: true,
	}); err != nil {
		t.Fatalf("skipping Telegram must not fail: %v", err)
	}
	if texts := historyTexts(b); !containsText(texts, "sólo para la app") {
		t.Fatalf("an app-only message must still be stored: %v", texts)
	}
}

func TestEmitSkipTelegramSendsNothing(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	if _, err := a.Bot.Emit(context.Background(), Outbound{
		Text: "sólo para la app", Source: SourceApp, SkipTelegram: true,
	}); err != nil {
		t.Fatal(err)
	}
	if sent := fake.Sent(); len(sent) != 0 {
		t.Fatalf("SkipTelegram must not reach Telegram, got %+v", sent)
	}
}

// TestSendTelegramReportsAMessageID keeps telegram_send useful now that it goes
// through the choke point.
func TestSendTelegramReportsAMessageID(t *testing.T) {
	a, _ := newTestAssistantWithTelegram(t)
	id, err := a.SendTelegram(context.Background(), 0, "hola", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Fatalf("expected the message id from Telegram, got %d", id)
	}
}

// TestOnlyEmitTalksToTelegram is a source-level guard. The rule is easy to break
// by reflex — reach for b.tg.SendText and the app history silently drifts — so
// it is enforced mechanically rather than by review.
func TestOnlyEmitTalksToTelegram(t *testing.T) {
	// Sending is centralised in outbound.go. The typing indicator is not a
	// message and carries nothing to record, so it is not on this list.
	const allowedFile = "outbound.go"
	sendMethods := map[string]bool{
		"SendText": true, "SendLongText": true, "SendMessage": true, "SendFile": true,
		// The progress display rewrites and removes the message it sent, so
		// these are on the list for the same reason the sends are.
		"EditMessageText": true, "DeleteMessage": true,
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == allowedFile {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !sendMethods[sel.Sel.Name] {
				return true
			}
			// Only flag calls on the Telegram client itself (x.tg.SendFoo).
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "tg" {
				return true
			}
			t.Errorf("%s: %s bypasses Emit — the app history would miss this message",
				fset.Position(call.Pos()), sel.Sel.Name)
			return true
		})
	}
}
