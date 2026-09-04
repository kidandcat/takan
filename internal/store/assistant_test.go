package store

import (
	"testing"
	"time"
)

func TestAssistantChatSessionStateMachine(t *testing.T) {
	st, owner, ctx := newOwner(t)
	const chat = "282611642"

	c, err := st.AssistantChatState(ctx, owner.ID, chat)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConversationStarted || c.SessionID != "" {
		t.Fatalf("an unknown chat starts empty, got %+v", c)
	}

	if err := st.MarkConversationStarted(ctx, owner.ID, chat, "s1"); err != nil {
		t.Fatal(err)
	}
	c, _ = st.AssistantChatState(ctx, owner.ID, chat)
	if !c.ConversationStarted || c.SessionID != "s1" || c.ForkFrom != "" || c.Runs != 1 {
		t.Fatalf("after a completed turn: %+v", c)
	}
	if c.LastRunAt == nil {
		t.Fatal("a completed turn stamps last_run_at")
	}

	if err := st.MarkConversationPromoted(ctx, owner.ID, chat, "s1"); err != nil {
		t.Fatal(err)
	}
	c, _ = st.AssistantChatState(ctx, owner.ID, chat)
	if c.ForkFrom != "s1" || c.Runs != 2 {
		t.Fatalf("after promotion: %+v", c)
	}

	if err := st.ResetAssistantConversation(ctx, owner.ID, chat); err != nil {
		t.Fatal(err)
	}
	c, _ = st.AssistantChatState(ctx, owner.ID, chat)
	if c.ConversationStarted || c.SessionID != "" || c.ForkFrom != "" {
		t.Fatalf("/new must rotate the session: %+v", c)
	}
	if c.Runs != 2 {
		t.Fatalf("/new must not count as a run: %+v", c)
	}
}

func TestAssistantChatsAreIndependent(t *testing.T) {
	st, owner, ctx := newOwner(t)
	if err := st.MarkConversationStarted(ctx, owner.ID, "1", "owner-session"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkConversationStarted(ctx, owner.ID, "-100", "group-session"); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetAssistantConversation(ctx, owner.ID, "1"); err != nil {
		t.Fatal(err)
	}
	group, _ := st.AssistantChatState(ctx, owner.ID, "-100")
	if group.SessionID != "group-session" {
		t.Fatalf("resetting one chat must not touch another: %+v", group)
	}
	list, err := st.ListAssistantChats(ctx, owner.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 known chats, got %d err=%v", len(list), err)
	}
}

func TestAssistantMessagesPagingAndCap(t *testing.T) {
	st, owner, ctx := newOwner(t)
	var ids []string
	for i := 0; i < 5; i++ {
		id := newMessageIDForTest(i)
		if err := st.AppendAssistantMessage(ctx, owner.ID, AssistantMessage{
			ID: id, Role: "user", Text: "m", Source: "app",
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got, err := st.ListAssistantMessages(ctx, owner.ID, ids[1], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages after id[1], got %d", len(got))
	}
	if got[0].ID != ids[2] {
		t.Fatalf("after must be exclusive, got %s want %s", got[0].ID, ids[2])
	}
	tail, err := st.ListAssistantMessages(ctx, owner.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[1].ID != ids[4] {
		t.Fatalf("an empty cursor returns the newest window, got %+v", tail)
	}
}

func newMessageIDForTest(i int) string {
	return string(rune('a'+i)) + "0000000000000000"
}

func TestJobChatRoutingAndSweeper(t *testing.T) {
	st, owner, ctx := newOwner(t)

	if err := st.RecordJobChat(ctx, "job-1", owner.ID, "-100", "vps3"); err != nil {
		t.Fatal(err)
	}
	jc, err := st.JobChatByID(ctx, "job-1")
	if err != nil || jc == nil {
		t.Fatalf("job chat: %+v err=%v", jc, err)
	}
	if jc.ChatID != "-100" || jc.Machine != "vps3" {
		t.Fatalf("unexpected routing row: %+v", jc)
	}

	pending, err := st.PendingJobChats(ctx, owner.ID, time.Now().Add(-time.Hour))
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending delivery, got %d err=%v", len(pending), err)
	}

	if err := st.MarkJobChatDelivered(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingJobChats(ctx, owner.ID, time.Now().Add(-time.Hour))
	if len(pending) != 0 {
		t.Fatalf("a delivered job must leave the retry queue, got %d", len(pending))
	}

	// An unknown job is not an error: the caller falls back to the owner DM.
	missing, err := st.JobChatByID(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("unknown job: %+v err=%v", missing, err)
	}
}

func TestPushDevicesDedupe(t *testing.T) {
	st, owner, ctx := newOwner(t)
	for _, p := range []string{"android", "ios"} {
		if err := st.RegisterPushDevice(ctx, owner.ID, "device-a", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RegisterPushDevice(ctx, owner.ID, "device-b", "ios"); err != nil {
		t.Fatal(err)
	}
	devices, err := st.ListPushDevices(ctx, owner.ID)
	if err != nil || len(devices) != 2 {
		t.Fatalf("re-registering a device must refresh it, got %d err=%v", len(devices), err)
	}
	if err := st.DeletePushDevice(ctx, owner.ID, "device-a"); err != nil {
		t.Fatal(err)
	}
	devices, _ = st.ListPushDevices(ctx, owner.ID)
	if len(devices) != 1 || devices[0].Token != "device-b" {
		t.Fatalf("expected only device-b to remain, got %+v", devices)
	}
}

func TestAssistantMetaRoundTrip(t *testing.T) {
	st, owner, ctx := newOwner(t)
	if got, err := st.AssistantMeta(ctx, owner.ID, MetaTelegramOffset); err != nil || got != "" {
		t.Fatalf("missing key must read empty, got %q err=%v", got, err)
	}
	if err := st.SetAssistantMeta(ctx, owner.ID, MetaTelegramOffset, "42"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAssistantMeta(ctx, owner.ID, MetaTelegramOffset, "43"); err != nil {
		t.Fatal(err)
	}
	got, err := st.AssistantMeta(ctx, owner.ID, MetaTelegramOffset)
	if err != nil || got != "43" {
		t.Fatalf("meta round trip: %q err=%v", got, err)
	}
}
