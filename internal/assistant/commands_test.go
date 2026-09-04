package assistant

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// TestCommandsDoNotBlockThePollLoop is the regression test for a stalled
// assistant: dispatch runs inline in the getUpdates loop, so answering a
// command there — /usage walks the whole session tree on disk — stops the bot
// receiving anything at all until it finishes.
func TestCommandsDoNotBlockThePollLoop(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.setMe(&tg.User{ID: 77, Username: "casa_bot"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.SetRunContext(ctx)

	// A reporter that hangs stands in for a slow session tree.
	release := make(chan struct{})
	b.SetUsage(blockingUsage{release: release})

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.dispatch(ctx, &tg.Update{Message: ownerMsg(ownerChat, "private", "/usage")})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch blocked on a slow command; the poll loop would stall")
	}

	close(release)
	waitFor(t, func() bool { return len(fake.TextsTo(ownerChat)) > 0 }, "the command answer")
}

type blockingUsage struct{ release chan struct{} }

func (blockingUsage) Runner() string { return "grok" }
func (u blockingUsage) Report() (UsageReport, error) {
	<-u.release
	return UsageReport{Runner: "grok"}, nil
}

// TestTaskListingIsPerChat: task titles and prompts are the operator's own
// words, so a group must only ever see what was started in that group.
func TestTaskListingIsPerChat(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	if err := b.state.See(groupChat, "group", "Casa"); err != nil {
		t.Fatal(err)
	}

	a.TaskMgr.mu.Lock()
	a.TaskMgr.tasks["dm"] = &Task{
		ID: "dm", Title: "renovar el seguro del coche", State: TaskRunning,
		StartedAt: time.Now(), ChatID: 0,
	}
	a.TaskMgr.tasks["grp"] = &Task{
		ID: "grp", Title: "lista de la compra", State: TaskRunning,
		StartedAt: time.Now(), ChatID: groupChat,
	}
	a.TaskMgr.mu.Unlock()

	groupView := b.tasksSummary(groupChat)
	if strings.Contains(groupView, "seguro del coche") {
		t.Fatalf("a private task leaked into a group listing:\n%s", groupView)
	}
	if !strings.Contains(groupView, "lista de la compra") {
		t.Fatalf("the group's own task is missing:\n%s", groupView)
	}

	dmView := b.tasksSummary(ownerChat)
	if !strings.Contains(dmView, "seguro del coche") {
		t.Fatalf("the owner should see his own task:\n%s", dmView)
	}
	if strings.Contains(dmView, "lista de la compra") {
		t.Fatalf("a group task should not appear in the DM listing:\n%s", dmView)
	}
}

// TestCancelIsPerChat: /cancel in a group must not stop work started privately.
func TestCancelIsPerChat(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot

	a.TaskMgr.mu.Lock()
	a.TaskMgr.tasks["dm"] = &Task{
		ID: "dm", Title: "private", State: TaskRunning, Promoted: true,
		StartedAt: time.Now(), ChatID: 0,
	}
	a.TaskMgr.cancels["dm"] = func() {}
	a.TaskMgr.mu.Unlock()

	if _, ok := b.cancelLatestPromoted(groupChat); ok {
		t.Fatal("/cancel in a group must not reach a task started in the DM")
	}
	if _, ok := b.cancelLatestPromoted(ownerChat); !ok {
		t.Fatal("/cancel in the owner's chat must reach his own task")
	}
}

// TestUsageIsOwnerChatOnly: consumption is account-wide and names what the
// operator has been working on.
func TestUsageIsOwnerChatOnly(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.setMe(&tg.User{ID: 77, Username: "casa_bot"})

	b.handleCommand(context.Background(), groupChat, ownerMsg(groupChat, "supergroup", "/usage"))
	got := fake.TextsTo(groupChat)
	if len(got) != 1 || !strings.Contains(got[0], "chat privado") {
		t.Fatalf("a group should be told where to ask, got %v", got)
	}
	if strings.Contains(got[0], "Consumo de") {
		t.Fatalf("usage figures leaked into a group: %v", got)
	}
}

// TestInterruptedRunIsAnnounced: a run killed by a restart looks the same as
// one killed by /cancel, but only the second is something the operator already
// knows about. Without a word his message simply vanishes.
func TestInterruptedRunIsAnnounced(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot

	r := &chatRunner{wake: make(chan struct{}, 1)}
	handle := cancelledHandle()
	b.finishConversationalRun(context.Background(), ownerChat, true, RunSpec{}, handle, r)

	got := fake.TextsTo(ownerChat)
	if len(got) != 1 || !strings.Contains(got[0], "reiniciado a mitad") {
		t.Fatalf("expected a restart notice, got %v", got)
	}
	if texts := historyTexts(b); !containsText(texts, "reiniciado a mitad") {
		t.Fatalf("the notice must also reach the app history: %v", texts)
	}
}

func TestUserCancelledRunIsNotAnnounced(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot

	r := &chatRunner{wake: make(chan struct{}, 1)}
	r.mu.Lock()
	r.running = true
	r.cancel = func() {}
	r.mu.Unlock()
	if !r.stop() {
		t.Fatal("expected the runner to report a cancellation")
	}

	b.finishConversationalRun(context.Background(), ownerChat, true, RunSpec{}, cancelledHandle(), r)
	if got := fake.TextsTo(ownerChat); len(got) != 0 {
		t.Fatalf("/cancel was already acknowledged; a second notice is noise: %v", got)
	}
}

// cancelledHandle is a finished run that reports itself cancelled.
func cancelledHandle() *RunHandle {
	h := &RunHandle{startedAt: time.Now(), done: make(chan struct{}), cancel: func() {}}
	h.res = &AgentResult{Cancelled: true, Duration: time.Second}
	h.err = context.Canceled
	close(h.done)
	return h
}

// TestBotIdentityIsRaceFree pins a real concurrency bug: the poll loop writes
// the bot's identity after getMe, while the panel, /health and takan_status read
// it from HTTP handlers. It was a plain pointer field.
func TestBotIdentityIsRaceFree(t *testing.T) {
	a, _ := newTestAssistantWithTelegram(t)
	b := a.Bot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(ctx) // writes b.me after getMe
	}()

	// Concurrent readers, as the panel and the health endpoint would be.
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = b.Username()
				_ = b.addressedToUs(ownerMsg(groupChat, "supergroup", "@casa_bot hola"))
			}
		}()
	}

	waitFor(t, func() bool { return b.Username() == "casa_bot" }, "the bot to identify itself")
	close(stop)
	readers.Wait()
	cancel()
	<-done
}
