package assistant

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

func TestBatchPromptCoalescesMessages(t *testing.T) {
	b := newTestBot(t)
	prompt := b.buildBatchPrompt(context.Background(), ownerChat,
		queuedTG("first thing", "second thing", "third thing"))

	for _, want := range []string{"first thing", "second thing", "third thing"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "Jairo sent 3 messages") {
		t.Fatalf("expected a header explaining the batch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "--- message 2 of 3 ---") {
		t.Fatalf("expected messages to be clearly separated:\n%s", prompt)
	}
}

func TestBatchPromptSingleMessageIsNotDecorated(t *testing.T) {
	b := newTestBot(t)
	if prompt := b.buildBatchPrompt(context.Background(), ownerChat, queuedTG("just one")); prompt != "just one" {
		t.Fatalf("a lone message should pass through untouched, got %q", prompt)
	}
}

func TestChatRunnerBusyAndStop(t *testing.T) {
	r := &chatRunner{wake: make(chan struct{}, 1)}

	if busy, _ := r.busy(); busy {
		t.Fatal("a fresh runner should be idle")
	}
	if r.stop() {
		t.Fatal("stopping an idle runner should report nothing to cancel")
	}

	cancelled := false
	r.mu.Lock()
	r.running = true
	r.startedAt = time.Now().Add(-30 * time.Second)
	r.cancel = func() { cancelled = true }
	r.mu.Unlock()

	busy, elapsed := r.busy()
	if !busy {
		t.Fatal("expected the runner to report busy")
	}
	if elapsed < 29*time.Second {
		t.Fatalf("expected ~30s elapsed, got %s", elapsed)
	}
	if !r.stop() || !cancelled {
		t.Fatal("expected stop to cancel the in-flight run")
	}
}

// TestBatchPromptMarksSupersededTurns pins the shape of a coalesced prompt: the
// interrupted turn is carried, in order, labelled, and the agent is told the
// last message is the live one.
func TestBatchPromptMarksSupersededTurns(t *testing.T) {
	b := newTestBot(t)
	batch := queuedTG("esto está solucionado?", "es Astra en app store")
	batch[0].superseded.Store(true)

	prompt := b.buildBatchPrompt(context.Background(), ownerChat, batch)

	if !strings.Contains(prompt, supersededMarker) {
		t.Fatalf("the interrupted turn must be labelled:\n%s", prompt)
	}
	first := strings.Index(prompt, "esto está solucionado?")
	second := strings.Index(prompt, "es Astra en app store")
	if first < 0 || second < 0 {
		t.Fatalf("both turns must survive the interrupt:\n%s", prompt)
	}
	if first > second {
		t.Fatalf("the interrupted turn must come first:\n%s", prompt)
	}
	if strings.Index(prompt, supersededMarker) > first {
		t.Fatalf("the marker belongs on the interrupted turn, not after it:\n%s", prompt)
	}
	if !strings.Contains(prompt, "interrupted you") {
		t.Fatalf("the prompt must say the previous run was cut short:\n%s", prompt)
	}
	// The header for an ordinary pile-up would be wrong here: nothing queued.
	if strings.Contains(prompt, "while you were busy") {
		t.Fatalf("an interrupt is not a backlog:\n%s", prompt)
	}
}

// blockingRun builds a startHook whose first run blocks until it is cancelled
// (like a real grok being killed) and whose later runs answer immediately. It
// returns the prompts each run was given, in order.
type runRecorder struct {
	mu      sync.Mutex
	prompts []string

	started chan struct{}
	release chan struct{}
}

func (rec *runRecorder) record(prompt string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.prompts = append(rec.prompts, prompt)
	return len(rec.prompts)
}

func (rec *runRecorder) all() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.prompts...)
}

// ownerText is an inbound Telegram message from the owner's own chat.
func ownerText(id int64, text string) *tg.Message {
	return &tg.Message{
		MessageID: id, Chat: tg.Chat{ID: ownerChat, Type: "private"},
		From: &tg.User{ID: ownerChat}, Text: text,
	}
}

// TestNewMessageInterruptsTheRunInFlight is the core promise: writing again
// kills the run rather than queueing behind it, its answer is thrown away, and
// exactly one replacement run is started carrying both turns.
func TestNewMessageInterruptsTheRunInFlight(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &runRecorder{started: make(chan struct{}, 4)}
	b.startHook = stubStart(func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		n := rec.record(spec.Prompt)
		rec.started <- struct{}{}
		if n > 1 {
			return &AgentResult{Stdout: "answer to both", Duration: time.Millisecond}, nil
		}
		<-cancelled
		// The nasty case: the run actually produced an answer just as it was
		// killed. It must still never reach the chat.
		return &AgentResult{Stdout: "half answer", Duration: time.Second}, nil
	})

	b.enqueue(ctx, ownerText(1, "esto está solucionado?"))
	waitStarted(t, rec, "the first run never started")

	b.enqueue(ctx, ownerText(2, "es Astra en app store"))
	waitStarted(t, rec, "the replacement run never started")

	waitFor(t, func() bool { return containsText(historyTexts(b), "answer to both") },
		"the replacement run's answer never reached the conversation")

	prompts := rec.all()
	if len(prompts) != 2 {
		t.Fatalf("expected exactly two runs (the killed one and its replacement), got %d: %v", len(prompts), prompts)
	}
	replacement := prompts[1]
	for _, want := range []string{supersededMarker, "esto está solucionado?", "es Astra en app store"} {
		if !strings.Contains(replacement, want) {
			t.Fatalf("the replacement prompt is missing %q:\n%s", want, replacement)
		}
	}

	for _, texts := range [][]string{historyTexts(b), fake.TextsTo(ownerChat)} {
		if containsText(texts, "half answer") {
			t.Fatalf("the killed run's reply must never be delivered: %v", texts)
		}
		if containsText(texts, "encolado") || containsText(texts, "Interrumpido") {
			t.Fatalf("an interrupt is silent; the typing indicator is the whole story: %v", texts)
		}
	}

	if b.InterruptedRuns() != 1 {
		t.Fatalf("expected one interrupted run to be counted, got %d", b.InterruptedRuns())
	}
	waitFor(t, func() bool { return b.RunningConversations() == 0 },
		"the chat's slot was never released, so /health and deploy.sh would both hang")
}

// TestBurstInterruptsOnceAndKeepsOrder covers the real habit: three corrections
// in a row must produce ONE replacement run, not three.
func TestBurstInterruptsOnceAndKeepsOrder(t *testing.T) {
	a := newTestAssistantWithLegacyDir(t, "")
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &runRecorder{started: make(chan struct{}, 8), release: make(chan struct{})}
	b.startHook = stubStart(func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		n := rec.record(spec.Prompt)
		rec.started <- struct{}{}
		if n > 1 {
			return &AgentResult{Stdout: "one answer", Duration: time.Millisecond}, nil
		}
		<-cancelled
		// Hold the killed run open until the whole burst has landed, so the
		// worker cannot start a replacement halfway through it.
		<-rec.release
		return &AgentResult{Cancelled: true, Duration: time.Second}, context.Canceled
	})

	b.enqueue(ctx, ownerText(1, "uno"))
	waitStarted(t, rec, "the first run never started")

	b.enqueue(ctx, ownerText(2, "dos"))
	b.enqueue(ctx, ownerText(3, "tres"))
	b.enqueue(ctx, ownerText(4, "cuatro"))
	close(rec.release)

	waitStarted(t, rec, "the replacement run never started")
	waitFor(t, func() bool { return containsText(historyTexts(b), "one answer") },
		"the replacement run never answered")

	prompts := rec.all()
	if len(prompts) != 2 {
		t.Fatalf("a burst must interrupt once and coalesce, got %d runs: %v", len(prompts), prompts)
	}
	replacement := prompts[1]
	last := -1
	for _, want := range []string{"uno", "dos", "tres", "cuatro"} {
		at := strings.Index(replacement, want)
		if at < 0 {
			t.Fatalf("the coalesced prompt lost %q:\n%s", want, replacement)
		}
		if at < last {
			t.Fatalf("the coalesced prompt reordered %q:\n%s", want, replacement)
		}
		last = at
	}
	if b.InterruptedRuns() != 1 {
		t.Fatalf("a burst must interrupt exactly once, got %d", b.InterruptedRuns())
	}
}

// TestBackgroundTaskSurvivesANewMessage is the boundary the interrupt must not
// cross. A run that outlived the soft budget belongs to the task manager now,
// and a new message starts a fresh conversation instead of killing it.
func TestBackgroundTaskSurvivesANewMessage(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "80ms"
	a.TaskMgr.Start(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var killed atomic.Bool
	rec := &runRecorder{started: make(chan struct{}, 4), release: make(chan struct{})}
	b.startHook = stubStart(func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		n := rec.record(spec.Prompt)
		rec.started <- struct{}{}
		if n > 1 {
			return &AgentResult{Stdout: "fresh answer", Duration: time.Millisecond}, nil
		}
		select {
		case <-rec.release:
		case <-cancelled:
			killed.Store(true)
		}
		return &AgentResult{Stdout: "slow answer", Duration: time.Second}, nil
	})

	b.enqueue(ctx, ownerText(1, "algo lento"))
	waitStarted(t, rec, "the slow run never started")

	// Wait for the promotion to actually happen before writing again.
	waitFor(t, func() bool {
		for _, task := range a.TaskMgr.List() {
			if task.Promoted && task.Running() {
				return true
			}
		}
		return false
	}, "the slow run was never promoted to a background task")

	b.enqueue(ctx, ownerText(2, "otra cosa"))
	waitStarted(t, rec, "the new message did not start its own run")
	waitFor(t, func() bool { return containsText(historyTexts(b), "fresh answer") },
		"the new message was never answered")

	if killed.Load() {
		t.Fatal("a new message must never cancel background work")
	}
	if b.InterruptedRuns() != 0 {
		t.Fatalf("nothing conversational was in flight, so nothing should have been interrupted: %d", b.InterruptedRuns())
	}
	for _, task := range a.TaskMgr.List() {
		if task.Promoted && !task.Running() {
			t.Fatalf("the promoted task must still be running, got %s", task.State)
		}
	}

	// The replacement prompt is a fresh turn, not a coalesced one: the promoted
	// turn is still being answered in the background and is not superseded.
	if prompts := rec.all(); len(prompts) < 2 || strings.Contains(prompts[1], supersededMarker) {
		t.Fatalf("a message after a promotion starts a clean turn, got %v", prompts)
	}
	close(rec.release)
}

// TestNewCommandInterruptsTheRunInFlight: /new resets the conversation, so
// keeping the run alive makes no sense — but unlike an ordinary interrupt the
// discarded turn is NOT carried into a replacement run.
func TestNewCommandInterruptsTheRunInFlight(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var killed atomic.Bool
	rec := &runRecorder{started: make(chan struct{}, 4)}
	b.startHook = stubStart(func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		rec.record(spec.Prompt)
		rec.started <- struct{}{}
		<-cancelled
		killed.Store(true)
		return &AgentResult{Cancelled: true, Duration: time.Second}, context.Canceled
	})

	b.enqueue(ctx, ownerText(1, "una pregunta larga"))
	waitStarted(t, rec, "the run never started")

	b.handleCommand(ctx, ownerChat, ownerText(2, "/new"))

	waitFor(t, killed.Load, "/new left the run alive; its answer belongs to a conversation that no longer exists")
	waitFor(t, func() bool { return b.RunningConversations() == 0 }, "the chat's slot was never released")

	if chat := b.state.Chat(ownerChat); chat.ConversationStarted {
		t.Fatalf("/new must rotate the session, got %#v", chat)
	}
	if prompts := rec.all(); len(prompts) != 1 {
		t.Fatalf("/new discards the turn rather than replaying it, got %d runs: %v", len(prompts), prompts)
	}
	if texts := fake.TextsTo(ownerChat); !containsText(texts, "Empezamos conversación nueva") {
		t.Fatalf("expected the reset confirmation, got %v", texts)
	}
}

// waitStarted blocks until one more run has begun.
func waitStarted(t *testing.T, rec *runRecorder, msg string) {
	t.Helper()
	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s (prompts so far: %v)", msg, rec.all())
	}
}

func TestNextRunSpecSessionStateMachine(t *testing.T) {
	b := newTestBot(t)

	// A fresh chat starts a new session with a generated id.
	first, _ := b.nextRunSpec(ownerChat)
	if first.Mode != RunNew || first.SessionID == "" {
		t.Fatalf("expected a new session, got %#v", first)
	}

	// After a completed turn the same session is resumed.
	if err := b.state.MarkConversationStarted(ownerChat, first.SessionID); err != nil {
		t.Fatal(err)
	}
	second, _ := b.nextRunSpec(ownerChat)
	if second.Mode != RunResume || second.SessionID != first.SessionID {
		t.Fatalf("expected to resume %s, got %#v", first.SessionID, second)
	}

	// After a promotion the next turn forks off the promoted session.
	if err := b.state.MarkPromoted(ownerChat, second.SessionID); err != nil {
		t.Fatal(err)
	}
	third, _ := b.nextRunSpec(ownerChat)
	if third.Mode != RunFork {
		t.Fatalf("expected a fork after promotion, got %#v", third)
	}
	if third.ParentSession != second.SessionID {
		t.Fatalf("expected to fork from %s, got %s", second.SessionID, third.ParentSession)
	}
	if third.SessionID == "" || third.SessionID == second.SessionID {
		t.Fatalf("a fork needs a distinct new session id, got %q", third.SessionID)
	}

	// Completing the forked turn clears the pending fork.
	if err := b.state.MarkConversationStarted(ownerChat, third.SessionID); err != nil {
		t.Fatal(err)
	}
	fourth, _ := b.nextRunSpec(ownerChat)
	if fourth.Mode != RunResume || fourth.SessionID != third.SessionID {
		t.Fatalf("expected to resume the forked session, got %#v", fourth)
	}

	// /new rotates: back to a brand new session.
	if err := b.state.ResetConversation(ownerChat); err != nil {
		t.Fatal(err)
	}
	fifth, _ := b.nextRunSpec(ownerChat)
	if fifth.Mode != RunNew || fifth.SessionID == fourth.SessionID {
		t.Fatalf("expected a rotated new session, got %#v", fifth)
	}
}

// TestPromotionFreesTheChat is the core guarantee: a run that overruns the soft
// budget is handed to the task manager, the chat is released, and the process is
// NOT killed.
func TestPromotionFreesTheChat(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "100ms"
	a.TaskMgr.Start(context.Background())

	release := make(chan struct{})
	var cancelledDuringRun bool
	b.startHook = stubStart(func(_ RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		select {
		case <-release:
		case <-cancelled:
			cancelledDuringRun = true
		}
		return &AgentResult{Stdout: "slow answer", Duration: time.Second}, nil
	})

	notices := make(chan string, 4)
	b.noticeHook = func(text string) { notices <- text }

	r := &chatRunner{wake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleBatch(context.Background(), ownerChat, r,
			[]*queuedMsg{{app: &appInbound{Text: "do something slow"}}})
	}()

	// The turn must return well before the run finishes.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the conversational turn did not return after the soft budget")
	}
	if cancelledDuringRun {
		t.Fatal("promotion must not kill the running process")
	}

	select {
	case note := <-notices:
		if !strings.Contains(note, "background") || !strings.Contains(note, "tarea") {
			t.Fatalf("expected a Spanish promotion notice, got %q", note)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no promotion notice was sent")
	}

	// The task manager now owns a running, promoted task.
	var promoted Task
	for _, task := range a.TaskMgr.List() {
		if task.Promoted {
			promoted = task
		}
	}
	if promoted.ID == "" || !promoted.Running() {
		t.Fatalf("expected a running promoted task, got %#v", a.TaskMgr.List())
	}

	// The chat is set up to fork from the promoted session on the next turn.
	if chat := b.state.Chat(ownerChat); chat.ForkFrom == "" {
		t.Fatal("expected the chat to be marked for forking after promotion")
	}

	// Letting it finish transitions the task to done.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := a.TaskMgr.Get(promoted.ID); ok && !task.Running() {
			if task.State != TaskDone {
				t.Fatalf("expected the promoted task to complete, got %s", task.State)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the promoted task never completed")
}

// TestFastRunIsNotPromoted guards the other side: a quick reply stays inline.
func TestFastRunIsNotPromoted(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "5s"

	b.startHook = stubStart(func(_ RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		return &AgentResult{Stdout: "quick answer", Duration: time.Millisecond}, nil
	})

	r := &chatRunner{wake: make(chan struct{}, 1)}
	b.handleBatch(context.Background(), ownerChat, r,
		[]*queuedMsg{{app: &appInbound{Text: "quick question"}}})

	if len(a.TaskMgr.List()) != 0 {
		t.Fatalf("a fast run must not create a task, got %#v", a.TaskMgr.List())
	}
	chat := b.state.Chat(ownerChat)
	if chat.ForkFrom != "" {
		t.Fatal("a fast run must not mark the chat for forking")
	}
	if !chat.ConversationStarted || chat.SessionID == "" {
		t.Fatalf("a completed run should record its session, got %#v", chat)
	}
}

// TestForkOnlyWhenPromotedRunFinished pins the measured behaviour: forking a
// session that is still being written hangs, so the conversation may only branch
// off a promoted session once its run is done.
func TestForkOnlyWhenPromotedRunFinished(t *testing.T) {
	a := newTestAssistant(t)
	b, tasks := a.Bot, a.TaskMgr

	const session = "11111111-2222-4333-8444-555555555555"
	if err := b.state.MarkPromoted(ownerChat, session); err != nil {
		t.Fatal(err)
	}

	// While the promoted task still owns the session: fresh session + a note.
	tasks.mu.Lock()
	tasks.tasks["t1"] = &Task{
		ID: "t1", State: TaskRunning, SessionID: session, Promoted: true, StartedAt: time.Now(),
	}
	tasks.mu.Unlock()

	spec, note := b.nextRunSpec(ownerChat)
	if spec.Mode != RunNew {
		t.Fatalf("must not fork a live session, got mode %s", spec.Mode)
	}
	if !strings.Contains(note, "t1") {
		t.Fatalf("expected a note naming the background task, got %q", note)
	}

	// A run that was killed mid-write leaves the session unforkable too.
	tasks.mu.Lock()
	tasks.tasks["t1"].State = TaskKilled
	tasks.tasks["t1"].FinishedAt = time.Now()
	tasks.mu.Unlock()

	spec, note = b.nextRunSpec(ownerChat)
	if spec.Mode != RunNew {
		t.Fatalf("must not fork a session whose run was killed, got mode %s", spec.Mode)
	}
	if !strings.Contains(note, "no terminó bien") {
		t.Fatalf("expected a note about the unclean finish, got %q", note)
	}

	// Only a cleanly finished run makes forking safe.
	tasks.mu.Lock()
	tasks.tasks["t1"].State = TaskDone
	tasks.mu.Unlock()

	spec, note = b.nextRunSpec(ownerChat)
	if spec.Mode != RunFork || spec.ParentSession != session {
		t.Fatalf("expected a fork off %s, got %#v", session, spec)
	}
	if note != "" {
		t.Fatalf("a fork carries context, so no note is needed, got %q", note)
	}
}

func TestTaskLifecycleAndOrphanDetection(t *testing.T) {
	a := newTestAssistant(t)
	m := a.TaskMgr

	if _, err := m.Run("", "", 0); err == nil {
		t.Fatal("expected an empty prompt to be rejected")
	}

	// Seed a task that looks like it was running when the process died.
	m.mu.Lock()
	seeded := &Task{ID: "abc123", Title: "stale", State: TaskRunning, StartedAt: time.Now().Add(-time.Hour)}
	m.tasks["abc123"] = seeded
	m.persistLocked(seeded)
	m.mu.Unlock()

	// Start reconciles it: the child died with the old process.
	m.Start(context.Background())
	task, ok := m.Get("abc123")
	if !ok || task.State != TaskOrphaned {
		t.Fatalf("expected the task to be marked orphaned, got %#v", task)
	}
	if task.Running() {
		t.Fatal("an orphaned task must not count as running")
	}
	if !strings.Contains(task.Summary(), "abc123") {
		t.Fatalf("summary should mention the id: %s", task.Summary())
	}

	// The reconciliation is durable, not just in memory.
	rows, err := a.Store.ListAssistantTasks(context.Background(), a.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != TaskOrphaned {
		t.Fatalf("the orphaned state must be persisted, got %+v", rows)
	}
}

func TestTaskPruneKeepsRunning(t *testing.T) {
	m := newTestAssistant(t).TaskMgr
	m.mu.Lock()
	m.tasks["old"] = &Task{ID: "old", State: TaskDone, StartedAt: time.Now().Add(-48 * time.Hour), FinishedAt: time.Now().Add(-47 * time.Hour)}
	m.tasks["live"] = &Task{ID: "live", State: TaskRunning, StartedAt: time.Now().Add(-48 * time.Hour)}
	m.mu.Unlock()

	if removed := m.Prune(24 * time.Hour); removed != 1 {
		t.Fatalf("expected 1 pruned task, got %d", removed)
	}
	if _, ok := m.Get("live"); !ok {
		t.Fatal("a running task must never be pruned")
	}
}

func TestMaxConcurrentTasksIsEnforced(t *testing.T) {
	// Background tasks now share the hub's cgroup, so an unbounded fan-out is an
	// OOM waiting to happen.
	m := newTestAssistant(t).TaskMgr
	m.opts.Agent.MaxConcurrentTasks = 1
	m.mu.Lock()
	m.tasks["live"] = &Task{ID: "live", State: TaskRunning, StartedAt: time.Now()}
	m.mu.Unlock()

	if _, err := m.Run("another one", "", 0); err == nil ||
		!strings.Contains(err.Error(), "too many background tasks") {
		t.Fatalf("expected the concurrency cap to reject the run, got %v", err)
	}
}

func TestRateLimitCounterWindowsAtADay(t *testing.T) {
	m := newTestAssistant(t).TaskMgr
	m.mu.Lock()
	m.rateLimited = []time.Time{time.Now().Add(-25 * time.Hour), time.Now().Add(-time.Hour)}
	m.mu.Unlock()

	if got := m.RateLimitedLast24h(); got != 1 {
		t.Fatalf("only refusals inside the window count, got %d", got)
	}
	m.NoteRateLimited()
	if got := m.RateLimitedLast24h(); got != 2 {
		t.Fatalf("expected the new refusal to be counted, got %d", got)
	}
}

func TestJobValidate(t *testing.T) {
	cases := []struct {
		name    string
		job     Job
		wantErr bool
	}{
		{"cron message", Job{Type: JobMessage, Payload: "hi", Cron: "0 9 * * 1"}, false},
		{"one-shot agent", Job{Type: JobAgent, Payload: "do it", At: time.Now()}, false},
		{"descriptor", Job{Type: JobMessage, Payload: "hi", Cron: "@daily"}, false},
		{"bad cron", Job{Type: JobMessage, Payload: "hi", Cron: "not a cron"}, true},
		{"no schedule", Job{Type: JobMessage, Payload: "hi"}, true},
		{"no payload", Job{Type: JobMessage, Cron: "@daily"}, true},
		{"bad type", Job{Type: "carrier pigeon", Payload: "hi", Cron: "@daily"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.job.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNextRunUsesMadridTimezone(t *testing.T) {
	loc, err := time.LoadLocation(ScheduleLocation)
	if err != nil {
		t.Fatalf("timezone unavailable: %v", err)
	}
	job := &Job{Type: JobMessage, Payload: "hi", Cron: "0 9 * * *"}
	// Midsummer, when Madrid is UTC+2.
	from := time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC)
	next, err := nextRun(job, from, loc)
	if err != nil {
		t.Fatal(err)
	}
	if next.In(loc).Hour() != 9 {
		t.Fatalf("expected 09:00 Madrid, got %s", next.In(loc).Format(time.RFC3339))
	}
	if next.UTC().Hour() != 7 {
		t.Fatalf("expected 07:00 UTC (Madrid is UTC+2 in July), got %s", next.UTC().Format(time.RFC3339))
	}
}

func TestJobStoreRoundTripAndOneShotRemoval(t *testing.T) {
	a := newTestAssistant(t)
	jobs := a.Sched.Store()
	loc := a.Sched.Location()

	oneShot, err := jobs.Add(&Job{Type: JobMessage, Payload: "ping", At: time.Now().Add(-time.Minute)}, loc)
	if err != nil {
		t.Fatal(err)
	}
	recurring, err := jobs.Add(&Job{Type: JobAgent, Payload: "report", Cron: "0 9 * * *"}, loc)
	if err != nil {
		t.Fatal(err)
	}

	if due := jobs.Due(time.Now().In(loc)); len(due) != 1 || due[0].ID != oneShot.ID {
		t.Fatalf("expected only the one-shot to be due, got %#v", due)
	}

	// Advancing a one-shot removes it; advancing a cron job moves it forward.
	if err := jobs.Advance(oneShot.ID, loc); err != nil {
		t.Fatal(err)
	}
	if _, ok := jobs.Get(oneShot.ID); ok {
		t.Fatal("one-shot should be gone after firing")
	}
	before, _ := jobs.Get(recurring.ID)
	if err := jobs.Advance(recurring.ID, loc); err != nil {
		t.Fatal(err)
	}
	after, _ := jobs.Get(recurring.ID)
	if after.NextRun.Before(before.NextRun) {
		t.Fatalf("recurring job went backwards: %s -> %s", before.NextRun, after.NextRun)
	}
	if after.Runs != 1 {
		t.Fatalf("expected 1 run recorded, got %d", after.Runs)
	}

	// State must survive a reload from the database.
	reloaded, err := NewJobStore(context.Background(), a.Store, a.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if list := reloaded.List(); len(list) != 1 || list[0].ID != recurring.ID {
		t.Fatalf("expected the recurring job to persist, got %#v", list)
	}
}

func TestRescheduleDropsStaleOneShots(t *testing.T) {
	a := newTestAssistant(t)
	jobs := a.Sched.Store()
	loc := a.Sched.Location()

	recent, _ := jobs.Add(&Job{Type: JobMessage, Payload: "recent", At: time.Now().Add(-time.Hour)}, loc)
	stale, _ := jobs.Add(&Job{Type: JobMessage, Payload: "stale", At: time.Now().Add(-48 * time.Hour)}, loc)

	missed, err := jobs.Reschedule(loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(missed) != 1 || missed[0].ID != recent.ID {
		t.Fatalf("expected only the recent one-shot to be replayed, got %#v", missed)
	}
	if _, ok := jobs.Get(stale.ID); ok {
		t.Fatal("stale one-shot should have been dropped")
	}
}
