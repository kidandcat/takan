package assistant

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

const (
	strangerID = int64(999)
	groupChat  = int64(-100)
)

// ownerMsg is a message the owner wrote.
func ownerMsg(chatID int64, chatType, text string, entities ...tg.Entity) *tg.Message {
	return &tg.Message{
		MessageID: 1,
		Chat:      tg.Chat{ID: chatID, Type: chatType, Title: "Casa"},
		From:      &tg.User{ID: ownerChat, FirstName: "Jairo", Username: "kidandcat"},
		Text:      text,
		Entities:  entities,
	}
}

// strangerMsg is a message somebody else wrote.
func strangerMsg(chatID int64, chatType, text string, entities ...tg.Entity) *tg.Message {
	m := ownerMsg(chatID, chatType, text, entities...)
	m.From = &tg.User{ID: strangerID, FirstName: "Olga", Username: "olga"}
	return m
}

// TestGateIsIdentityNotChat is the core authorization guarantee: the assistant
// serves one person. There is no whitelist, no approval, and no reply of any
// kind to anyone else — silence, so it does not confirm its own existence.
func TestGateIsIdentityNotChat(t *testing.T) {
	b := newTestBot(t)
	b.setMe(&tg.User{ID: 77, Username: "casa_bot"})

	if !b.accept(ownerMsg(ownerChat, "private", "hola")) {
		t.Fatal("the owner's own chat must be served")
	}
	if b.accept(strangerMsg(strangerID, "private", "hola")) {
		t.Fatal("a stranger's DM must be ignored")
	}
	if b.accept(strangerMsg(groupChat, "supergroup", "@casa_bot haz algo",
		tg.Entity{Type: "mention", Offset: 0, Length: 9})) {
		t.Fatal("a stranger must not be served even when they mention the bot")
	}

	// Channel posts and service messages have no sender.
	anon := ownerMsg(groupChat, "channel", "post")
	anon.From = nil
	if b.accept(anon) {
		t.Fatal("a message with no sender must be ignored")
	}
}

func TestGroupsNeedAMentionOrAReply(t *testing.T) {
	b := newTestBot(t)
	b.setMe(&tg.User{ID: 77, Username: "casa_bot"})

	if b.accept(ownerMsg(groupChat, "supergroup", "qué cena hay hoy")) {
		t.Fatal("unaddressed group chatter must be ignored")
	}
	if !b.accept(ownerMsg(groupChat, "supergroup", "@casa_bot pon la lista",
		tg.Entity{Type: "mention", Offset: 0, Length: 9})) {
		t.Fatal("an @mention from the owner must be answered")
	}
	// Edited and forwarded messages sometimes arrive without entities.
	if !b.accept(ownerMsg(groupChat, "supergroup", "oye @casa_bot mira esto")) {
		t.Fatal("a plain-text mention must be answered")
	}
	if b.accept(ownerMsg(groupChat, "supergroup", "@otro_bot haz algo")) {
		t.Fatal("a mention of another bot must be ignored")
	}

	reply := ownerMsg(groupChat, "supergroup", "y mañana?")
	reply.ReplyToMessage = &tg.Message{From: &tg.User{ID: 77, Username: "casa_bot"}}
	if !b.accept(reply) {
		t.Fatal("a reply to the assistant must be answered")
	}
	other := ownerMsg(groupChat, "supergroup", "ya voy")
	other.ReplyToMessage = &tg.Message{From: &tg.User{ID: strangerID}}
	if b.accept(other) {
		t.Fatal("a reply to somebody else must be ignored")
	}

	// respond_to_all removes the mention requirement, never the owner check.
	b.opts.RespondToAll = true
	if !b.accept(ownerMsg(groupChat, "supergroup", "cualquier cosa")) {
		t.Fatal("respond_to_all must answer every message the owner writes")
	}
	if b.accept(strangerMsg(groupChat, "supergroup", "cualquier cosa")) {
		t.Fatal("respond_to_all must not lift the owner check")
	}
}

func TestIgnoredMessagesCreateNoRunner(t *testing.T) {
	b := newTestBot(t)
	b.setMe(&tg.User{ID: 77, Username: "casa_bot"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.dispatch(ctx, &tg.Update{Message: strangerMsg(strangerID, "private", "hola")})

	b.runnersMu.Lock()
	n := len(b.runners)
	b.runnersMu.Unlock()
	if n != 0 {
		t.Fatalf("an ignored message must not create a runner, got %d", n)
	}
	// Nothing is recorded either: a stranger never appears in the panel.
	chats, err := b.state.st.ListAssistantChats(ctx, b.state.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 0 {
		t.Fatalf("an ignored chat must not be recorded, got %+v", chats)
	}
}

func TestPerChatSessionsAreIndependent(t *testing.T) {
	b := newTestBot(t)

	ownerSpec, _ := b.nextRunSpec(ownerChat)
	groupSpec, _ := b.nextRunSpec(groupChat)
	if ownerSpec.SessionID == groupSpec.SessionID {
		t.Fatal("each chat must get its own session id")
	}
	if err := b.state.MarkConversationStarted(ownerChat, ownerSpec.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := b.state.MarkConversationStarted(groupChat, groupSpec.SessionID); err != nil {
		t.Fatal(err)
	}
	if again, _ := b.nextRunSpec(ownerChat); again.Mode != RunResume || again.SessionID != ownerSpec.SessionID {
		t.Fatalf("the owner chat should resume its own session, got %#v", again)
	}
	if err := b.state.ResetConversation(ownerChat); err != nil {
		t.Fatal(err)
	}
	if spec, _ := b.nextRunSpec(ownerChat); spec.Mode != RunNew {
		t.Fatal("/new should rotate the owner session")
	}
	if spec, _ := b.nextRunSpec(groupChat); spec.SessionID != groupSpec.SessionID {
		t.Fatal("resetting one chat must not touch another")
	}
}

func TestPerChatInboxDirs(t *testing.T) {
	b := newTestBot(t)
	if a, c := b.inboxDirFor(ownerChat), b.inboxDirFor(groupChat); a == c {
		t.Fatal("each chat needs its own inbox directory")
	}
	if got := b.inboxDirFor(groupChat); !strings.HasSuffix(got, "-100") {
		t.Fatalf("unexpected inbox dir %q", got)
	}
}

func TestGroupPromptAttribution(t *testing.T) {
	b := newTestBot(t)
	prompt, err := b.buildQueuedPrompt(context.Background(), groupChat,
		&queuedMsg{tg: ownerMsg(groupChat, "supergroup", "saca la basura")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, "Jairo: ") {
		t.Fatalf("group messages must be attributed, got %q", prompt)
	}

	prompt, err = b.buildQueuedPrompt(context.Background(), ownerChat,
		&queuedMsg{tg: ownerMsg(ownerChat, "private", "hola")})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "hola" {
		t.Fatalf("private messages must not be attributed, got %q", prompt)
	}
}

func TestChatContextDescribesGroup(t *testing.T) {
	b := newTestBot(t)
	intro := b.chatContext(groupChat, []*queuedMsg{{tg: ownerMsg(groupChat, "supergroup", "hola")}})
	if !strings.Contains(intro, "Casa") || !strings.Contains(intro, "grupo") {
		t.Fatalf("group context should name the group, got %q", intro)
	}
	if got := b.chatContext(ownerChat, []*queuedMsg{{tg: ownerMsg(ownerChat, "private", "hola")}}); got != "" {
		t.Fatalf("the owner chat needs no context line, got %q", got)
	}
}

func TestWebhookReclaimIsRateLimited(t *testing.T) {
	b := newTestBot(t)

	// A second reclaim inside the window must be refused outright: two services
	// fighting over one bot must not become a delete/register loop.
	b.lastWebhookReclaim.Store(time.Now().Unix())
	if b.reclaimWebhook(context.Background()) {
		t.Fatal("a second reclaim inside the window must not proceed")
	}
	b.lastWebhookReclaim.Store(time.Now().Add(-2 * webhookReclaimWindow).Unix())
	if got := b.lastWebhookReclaim.Load(); got == 0 {
		t.Fatal("expected the reclaim timestamp to be set")
	}
}

func TestStrangerLoggingIsRateLimited(t *testing.T) {
	b := newTestBot(t)
	msg := strangerMsg(strangerID, "private", "hola")

	b.logStranger(msg)
	first, ok := b.strangers[strangerID]
	if !ok {
		t.Fatal("the first message from a stranger is logged")
	}
	b.logStranger(msg)
	if b.strangers[strangerID] != first {
		t.Fatal("a second message inside the window must not log again")
	}
}
