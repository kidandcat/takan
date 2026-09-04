package store

import (
	"context"
	"testing"
)

func newChannelStore(t *testing.T) (*Store, *User, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "channels@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	return st, u, ctx
}

func TestChannelCRUDAndChats(t *testing.T) {
	st, u, ctx := newChannelStore(t)

	if _, err := st.CreateTelegramChannel(ctx, u.ID, "", "sealed", "bot", ""); err == nil {
		t.Fatal("name is required")
	}
	if _, err := st.CreateTelegramChannel(ctx, u.ID, "family", "", "bot", ""); err == nil {
		t.Fatal("a sealed token is required")
	}
	c, err := st.CreateTelegramChannel(ctx, u.ID, "family", "sealed-1", "@family_bot", "Family Bot")
	if err != nil {
		t.Fatal(err)
	}
	if c.BotUser != "family_bot" {
		t.Fatalf("@ should be stripped: %q", c.BotUser)
	}
	if !c.IsDefault {
		t.Fatal("the first channel becomes the default")
	}
	second, err := st.CreateTelegramChannel(ctx, u.ID, "ops", "sealed-2", "ops_bot", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.IsDefault {
		t.Fatal("only the first channel is default")
	}

	// A channel serves several chats; type is inferred from a negative id.
	if err := st.AddChannelChat(ctx, u.ID, c.ID, "282611642", "", "Operator"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddChannelChat(ctx, u.ID, c.ID, "-1002233445566", "supergroup", "Casa"); err != nil {
		t.Fatal(err)
	}
	chats, err := st.ListChannelChats(ctx, c.ID)
	if err != nil || len(chats) != 2 {
		t.Fatalf("chats: %v %v", err, chats)
	}
	if chats[0].Type != TelegramChatPrivate || chats[1].Type != TelegramChatGroup {
		t.Fatalf("chat types: %+v", chats)
	}
	// Re-adding refreshes rather than duplicating.
	if err := st.AddChannelChat(ctx, u.ID, c.ID, "282611642", "private", "Jairo"); err != nil {
		t.Fatal(err)
	}
	chats, _ = st.ListChannelChats(ctx, c.ID)
	if len(chats) != 2 {
		t.Fatalf("re-add duplicated: %+v", chats)
	}

	full, err := st.TelegramChannelByID(ctx, u.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := full.PrimaryChat(""); got != "282611642" {
		t.Fatalf("PrimaryChat default: %q", got)
	}
	if got := full.PrimaryChat("-1002233445566"); got != "-1002233445566" {
		t.Fatalf("PrimaryChat explicit: %q", got)
	}
	if got := full.PrimaryChat("999"); got != "282611642" {
		t.Fatalf("unknown chat should fall back to the first: %q", got)
	}

	if err := st.SetDefaultTelegramChannel(ctx, u.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	def, err := st.DefaultTelegramChannel(ctx, u.ID)
	if err != nil || def.ID != second.ID {
		t.Fatalf("default: %+v %v", def, err)
	}
}

func TestChannelAttachmentsAndSingleReceiver(t *testing.T) {
	st, u, ctx := newChannelStore(t)
	c, err := st.CreateTelegramChannel(ctx, u.ID, "family", "sealed", "family_bot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddChannelChat(ctx, u.ID, c.ID, "-100999", "group", "Casa"); err != nil {
		t.Fatal(err)
	}

	// Many senders on one channel.
	for _, consumer := range []string{ConsumerNotifier, ConsumerEmail} {
		if err := st.AttachChannel(ctx, u.ID, ChannelAttachment{
			ChannelID: c.ID, Consumer: consumer, Direction: DirectionSend,
		}); err != nil {
			t.Fatalf("attach %s: %v", consumer, err)
		}
	}
	// Re-attaching the same tuple is idempotent.
	if err := st.AttachChannel(ctx, u.ID, ChannelAttachment{
		ChannelID: c.ID, Consumer: ConsumerNotifier, Direction: DirectionSend,
	}); err != nil {
		t.Fatal(err)
	}

	// Exactly one receive consumer: the token has a single getUpdates slot.
	if err := st.AttachChannel(ctx, u.ID, ChannelAttachment{
		ChannelID: c.ID, Consumer: ConsumerBot, ConsumerID: "bot-1", Direction: DirectionReceive,
	}); err != nil {
		t.Fatal(err)
	}
	err = st.AttachChannel(ctx, u.ID, ChannelAttachment{
		ChannelID: c.ID, Consumer: ConsumerBot, ConsumerID: "bot-2", Direction: DirectionReceive,
	})
	if err == nil {
		t.Fatal("a second receive attachment must be refused")
	}

	atts, err := st.ListChannelAttachments(ctx, u.ID, c.ID)
	if err != nil || len(atts) != 3 {
		t.Fatalf("attachments: %v %d", err, len(atts))
	}
	recv, err := st.ReceiverOf(ctx, u.ID, c.ID)
	if err != nil || recv == nil || recv.ConsumerID != "bot-1" {
		t.Fatalf("receiver: %+v %v", recv, err)
	}

	// A channel in use cannot be deleted.
	if err := st.DeleteTelegramChannel(ctx, u.ID, c.ID); err == nil {
		t.Fatal("delete must refuse while attachments exist")
	}
	if err := st.DetachConsumer(ctx, u.ID, ConsumerBot, "bot-1"); err != nil {
		t.Fatal(err)
	}
	if recv, _ := st.ReceiverOf(ctx, u.ID, c.ID); recv != nil {
		t.Fatal("detached consumer still receiving")
	}
	// The slot is free again.
	if err := st.AttachChannel(ctx, u.ID, ChannelAttachment{
		ChannelID: c.ID, Consumer: ConsumerBot, ConsumerID: "bot-2", Direction: DirectionReceive,
	}); err != nil {
		t.Fatalf("slot should be reusable: %v", err)
	}
}

func TestChannelForConsumerFallsBackToDefault(t *testing.T) {
	st, u, ctx := newChannelStore(t)
	def, err := st.CreateTelegramChannel(ctx, u.ID, "default", "sealed", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddChannelChat(ctx, u.ID, def.ID, "111", "private", "Operator"); err != nil {
		t.Fatal(err)
	}
	// No attachment yet: the consumer still resolves, via the default channel.
	c, chat, err := st.ChannelForConsumer(ctx, u.ID, ConsumerNotifier, "", DirectionSend)
	if err != nil || c.ID != def.ID || chat != "111" {
		t.Fatalf("fallback: %+v %q %v", c, chat, err)
	}

	other, err := st.CreateTelegramChannel(ctx, u.ID, "alerts", "sealed2", "b2", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddChannelChat(ctx, u.ID, other.ID, "222", "private", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AttachChannel(ctx, u.ID, ChannelAttachment{
		ChannelID: other.ID, Consumer: ConsumerNotifier, Direction: DirectionSend, ChatID: "222",
	}); err != nil {
		t.Fatal(err)
	}
	c, chat, err = st.ChannelForConsumer(ctx, u.ID, ConsumerNotifier, "", DirectionSend)
	if err != nil || c.ID != other.ID || chat != "222" {
		t.Fatalf("attachment should win: %+v %q %v", c, chat, err)
	}
}

// The pre-channels credential must keep working untouched after the migration.
func TestSeedChannelFromLegacyTelegramSettings(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUserOpts(ctx, "legacy@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTelegramSettings(ctx, u.ID, "sealed-legacy", "mavis_es_bot", "282611642",
		[]TelegramChat{{ID: "282611642", Label: "Jairo"}, {ID: "-100777", Label: "Casa"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	channels, err := st.ListTelegramChannels(ctx, u.ID)
	if err != nil || len(channels) != 1 {
		t.Fatalf("seeded channels: %v %d", err, len(channels))
	}
	c := channels[0]
	if c.Name != "default" || c.TokenEnc != "sealed-legacy" || c.BotUser != "mavis_es_bot" || !c.IsDefault {
		t.Fatalf("seeded channel: %+v", c)
	}
	if len(c.Chats) != 2 || c.Chats[0].ChatID != "282611642" {
		t.Fatalf("seeded chats: %+v", c.Chats)
	}
	if c.Chats[1].Type != TelegramChatGroup {
		t.Fatalf("negative ids are groups: %+v", c.Chats[1])
	}
	// The notifier keeps delivering exactly where it did before.
	ch, chat, err := st.ChannelForConsumer(ctx, u.ID, ConsumerNotifier, "", DirectionSend)
	if err != nil || ch.ID != c.ID || chat != "282611642" {
		t.Fatalf("notifier attachment: %+v %q %v", ch, chat, err)
	}

	// Re-opening again must not seed a second time.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := st.ListTelegramChannels(ctx, u.ID); len(again) != 1 {
		t.Fatalf("re-seeded: %d channels", len(again))
	}
}
