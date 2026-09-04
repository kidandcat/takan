package assistant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

func toolStart(id, tool, detail string) ProgressEvent {
	return ProgressEvent{Kind: ProgressToolStart, Tool: tool, Detail: detail, Status: ProgressRunning, CallID: id}
}

func toolEnd(id, tool string) ProgressEvent {
	return ProgressEvent{Kind: ProgressToolEnd, Tool: tool, Status: ProgressCompleted, CallID: id}
}

// pollFor waits for a condition without touching *testing.T, so it is safe to
// call from inside a stubbed run.
func pollFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// indexOfMethod is the position of the nth (first) call to a Bot API method.
func indexOfMethod(sent []sentMessage, method string) int {
	for i, m := range sent {
		if m.Method == method {
			return i
		}
	}
	return -1
}

func indexOfText(sent []sentMessage, want string) int {
	for i, m := range sent {
		if strings.Contains(m.Text, want) {
			return i
		}
	}
	return -1
}

// progressSends counts the messages that are progress frames rather than
// conversation.
func progressSends(sent []sentMessage) int {
	n := 0
	for _, m := range sent {
		if m.Method == "sendMessage" && strings.Contains(m.Text, "⏱") {
			n++
		}
	}
	return n
}

// TestConversationalRunUsesOneProgressMessageAndRemovesIt is the whole promise
// for a chat turn: one message appears while tools run, it is edited in place,
// and it is gone before the answer lands.
func TestConversationalRunUsesOneProgressMessageAndRemovesIt(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	b.progressInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		spec.OnProgress(toolStart("c1", "run_terminal_command", "ls -la"))
		if !pollFor(func() bool { return progressSends(fake.Sent()) >= 1 }, 5*time.Second) {
			return nil, context.DeadlineExceeded
		}
		spec.OnProgress(toolEnd("c1", "run_terminal_command"))
		spec.OnProgress(toolStart("c2", "read_file", "notes.txt"))
		if !pollFor(func() bool { return fake.calls("editMessageText") >= 1 }, 5*time.Second) {
			return nil, context.DeadlineExceeded
		}
		return &AgentResult{Stdout: "la respuesta", Duration: time.Second}, nil
	})

	b.enqueue(ctx, ownerText(1, "haz algo con ficheros"))
	waitFor(t, func() bool { return containsText(historyTexts(b), "la respuesta") }, "the answer")
	waitFor(t, func() bool { return fake.calls("deleteMessage") == 1 }, "the progress message to be removed")

	sent := fake.Sent()
	if got := progressSends(sent); got != 1 {
		t.Fatalf("a run gets ONE progress message, not %d: %+v", got, sent)
	}
	if fake.calls("editMessageText") < 1 {
		t.Fatalf("the message must be edited in place, not resent: %+v", sent)
	}

	deleted, answered := indexOfMethod(sent, "deleteMessage"), indexOfText(sent, "la respuesta")
	if deleted < 0 || answered < 0 {
		t.Fatalf("expected both a delete and an answer: %+v", sent)
	}
	if deleted > answered {
		t.Fatalf("the progress message must be gone BEFORE the answer is posted: %+v", sent)
	}

	// The frames live in Telegram alone; the app follows the same run over SSE.
	for _, text := range historyTexts(b) {
		if strings.Contains(text, "⏱") || strings.Contains(text, "⚙️") {
			t.Fatalf("a progress frame must never be persisted: %q", text)
		}
	}
}

// TestShortRunNeverShowsProgress: a turn answered without touching a tool must
// look exactly as it did before this feature existed.
func TestShortRunNeverShowsProgress(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	b.progressInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		// Reasoning alone is not worth a message.
		spec.OnProgress(ProgressEvent{Kind: ProgressThinking})
		time.Sleep(50 * time.Millisecond)
		return &AgentResult{Stdout: "hola", Duration: time.Millisecond}, nil
	})

	b.enqueue(ctx, ownerText(1, "hola"))
	waitFor(t, func() bool { return containsText(historyTexts(b), "hola") }, "the answer")

	sent := fake.Sent()
	if got := progressSends(sent); got != 0 {
		t.Fatalf("a tool-less run must produce no progress message, got %d: %+v", got, sent)
	}
	if fake.calls("editMessageText") != 0 || fake.calls("deleteMessage") != 0 {
		t.Fatalf("nothing to edit or delete: %+v", sent)
	}
}

// TestProgressEditsAreCoalesced: a burst of tool calls inside one throttle
// window is one edit showing the latest state, not one edit per call.
func TestProgressEditsAreCoalesced(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.progressInterval = 400 * time.Millisecond
	tracker := b.NewProgress(ownerChat, true, time.Now())
	defer tracker.Discard(context.Background())

	tracker.Add(toolStart("c1", "run_terminal_command", "first"))
	if !pollFor(func() bool { return progressSends(fake.Sent()) == 1 }, 5*time.Second) {
		t.Fatalf("the first tool call creates the message: %+v", fake.Sent())
	}

	// Twenty steps inside one window.
	for i := 0; i < 20; i++ {
		id := "burst" + string(rune('a'+i))
		tracker.Add(toolStart(id, "read_file", "file"+string(rune('a'+i))))
		tracker.Add(toolEnd(id, "read_file"))
	}
	if !pollFor(func() bool { return fake.calls("editMessageText") >= 1 }, 5*time.Second) {
		t.Fatalf("the burst must reach Telegram: %+v", fake.Sent())
	}
	time.Sleep(300 * time.Millisecond)

	if edits := fake.calls("editMessageText"); edits > 2 {
		t.Fatalf("20 steps in one throttle window must coalesce, got %d edits", edits)
	}
	if last := fake.lastText("editMessageText"); !strings.Contains(last, "filet") {
		t.Fatalf("the coalesced edit must show the LATEST state, got:\n%s", last)
	}
}

// TestProgressStopsEditingAfterTwoRateLimits: the chat also carries the real
// conversation, so once Telegram pushes back twice the display gives up rather
// than competing with the answer for the quota.
func TestProgressStopsEditingAfterTwoRateLimits(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.progressInterval = 10 * time.Millisecond
	tracker := b.NewProgress(ownerChat, true, time.Now())
	defer tracker.Discard(context.Background())

	fake.failEditsWith(429, 1, 0, "Too Many Requests: retry after 1")

	tracker.Add(toolStart("c1", "run_terminal_command", "first"))
	if !pollFor(func() bool { return progressSends(fake.Sent()) == 1 }, 5*time.Second) {
		t.Fatalf("the message is created before any edit: %+v", fake.Sent())
	}
	for i := 0; i < 6; i++ {
		tracker.Add(toolStart("c"+string(rune('a'+i)), "read_file", "file"))
		time.Sleep(60 * time.Millisecond)
	}

	if !pollFor(func() bool { return fake.calls("editMessageText") >= 2 }, 5*time.Second) {
		t.Fatalf("expected the display to retry once after the first 429: %+v", fake.Sent())
	}
	after := fake.calls("editMessageText")
	tracker.Add(toolStart("later", "grep", "algo"))
	time.Sleep(200 * time.Millisecond)
	if got := fake.calls("editMessageText"); got != after {
		t.Fatalf("after two 429s the display must stop for the rest of the run: %d then %d", after, got)
	}
	if after > 3 {
		t.Fatalf("a 429 must back off, not retry in a loop: %d edits", after)
	}
}

// TestProgressMessageGoesWithTheInterruptedRun: nothing about a killed run
// survives it, including its progress display.
func TestProgressMessageGoesWithTheInterruptedRun(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	b.progressInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &runRecorder{started: make(chan struct{}, 4)}
	b.startHook = stubStart(func(spec RunSpec, cancelled <-chan struct{}) (*AgentResult, error) {
		n := rec.record(spec.Prompt)
		if n > 1 {
			rec.started <- struct{}{}
			return &AgentResult{Stdout: "respuesta final", Duration: time.Millisecond}, nil
		}
		spec.OnProgress(toolStart("c1", "run_terminal_command", "algo lento"))
		pollFor(func() bool { return progressSends(fake.Sent()) >= 1 }, 5*time.Second)
		rec.started <- struct{}{}
		<-cancelled
		return &AgentResult{Cancelled: true, Duration: time.Second}, context.Canceled
	})

	b.enqueue(ctx, ownerText(1, "haz algo largo"))
	waitStarted(t, rec, "the first run never started")
	waitFor(t, func() bool { return progressSends(fake.Sent()) == 1 }, "the progress message")

	b.enqueue(ctx, ownerText(2, "mejor esto otro"))
	waitFor(t, func() bool { return containsText(historyTexts(b), "respuesta final") }, "the replacement answer")

	if fake.calls("deleteMessage") < 1 {
		t.Fatalf("the killed run's progress message must be removed: %+v", fake.Sent())
	}
	if b.InterruptedRuns() != 1 {
		t.Fatalf("expected exactly one interrupt, got %d", b.InterruptedRuns())
	}
	// The replacement run answered without tools, so it never showed one.
	if got := progressSends(fake.Sent()); got != 1 {
		t.Fatalf("each run gets its own single message; got %d in total: %+v", got, fake.Sent())
	}
}

// TestPromotedRunKeepsItsSingleMessage: crossing the soft budget must not post
// a second message next to the one the chat is already watching.
func TestPromotedRunKeepsItsSingleMessage(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "120ms"
	b.progressInterval = 20 * time.Millisecond
	a.TaskMgr.Start(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		spec.OnProgress(toolStart("c1", "run_terminal_command", "algo largo"))
		pollFor(func() bool { return progressSends(fake.Sent()) >= 1 }, 5*time.Second)
		<-release
		return &AgentResult{Stdout: "el resultado del background", Duration: 2 * time.Second}, nil
	})

	b.enqueue(ctx, ownerText(1, "una tarea larga"))
	waitFor(t, func() bool { return containsText(historyTexts(b), "lo paso a background") },
		"the promotion notice")

	if got := progressSends(fake.Sent()); got != 1 {
		t.Fatalf("the notice must reuse the progress message, not add one: %+v", fake.Sent())
	}
	close(release)
	waitFor(t, func() bool { return containsText(historyTexts(b), "el resultado del background") },
		"the task result")

	sent := fake.Sent()
	if got := len(fake.TextsTo(ownerChat)); got == 0 {
		t.Fatal("nothing reached the chat at all")
	}
	sends := 0
	for _, m := range sent {
		if m.Method == "sendMessage" {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("a promoted run is ONE Telegram message from progress to result, got %d sends: %+v", sends, sent)
	}
	if last := fake.lastText("editMessageText"); !strings.Contains(last, "el resultado del background") {
		t.Fatalf("the message must end up holding the result, got:\n%s", last)
	}
	if fake.calls("deleteMessage") != 0 {
		t.Fatalf("a task's message is its only trace; it is edited, never deleted: %+v", sent)
	}
}

// fakeRunnerScript writes a shell stand-in for the CLI agent that replays an
// NDJSON stream, so the task path is exercised through the real Agent.
func fakeRunnerScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBackgroundTaskIsOneMessageEditedIntoTheResult is the invariant Jairo asked
// for in so many words: a background task must never turn into a stream of
// messages. It runs the real Agent over a stub runner, so the count is the one
// Telegram would actually see.
func TestBackgroundTaskIsOneMessageEditedIntoTheResult(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	a.Bot.progressInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.TaskMgr.Start(ctx)
	a.TaskMgr.opts.Agent.Command = fakeRunnerScript(t, strings.Join([]string{
		`printf '%s\n' '{"type":"tool_call","toolCallId":"c1","toolName":"run_terminal_command","status":"pending","rawInput":{"command":"ls -la"}}'`,
		`sleep 0.2`,
		`printf '%s\n' '{"type":"tool_call_update","toolCallId":"c1","status":"completed","rawOutput":{"exit_code":0}}'`,
		`printf '%s\n' '{"type":"tool_call","toolCallId":"c2","toolName":"read_file","status":"pending","rawInput":{"file_path":"AGENTS.md"}}'`,
		`sleep 0.2`,
		`printf '%s\n' '{"type":"tool_call_update","toolCallId":"c2","status":"completed","rawOutput":{"exit_code":0}}'`,
		`printf '%s\n' '{"type":"text","data":"todo "}'`,
		`printf '%s\n' '{"type":"text","data":"listo"}'`,
		`printf '%s\n' '{"type":"end","stopReason":"end_turn"}'`,
	}, "\n")+"\n")

	task, err := a.TaskMgr.Run("haz cosas", "cosas", ownerChat)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return containsText(historyTexts(a.Bot), "todo listo") }, "the task result")

	sent := fake.Sent()
	sends := 0
	for _, m := range sent {
		if m.Method == "sendMessage" {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("ONE message per task, edited in place — got %d sends: %+v", sends, sent)
	}
	if fake.calls("editMessageText") < 2 {
		t.Fatalf("the message must be updated while the task runs and again with the result: %+v", sent)
	}
	if fake.calls("deleteMessage") != 0 {
		t.Fatalf("a task's message is never deleted: %+v", sent)
	}

	final := fake.lastText("editMessageText")
	for _, want := range []string{"✅ Tarea " + task.ID, "todo listo"} {
		if !strings.Contains(final, want) {
			t.Fatalf("the final message must hold %q, got:\n%s", want, final)
		}
	}

	// The result belongs in the app history exactly once, through the ordinary
	// path — the intermediate frames do not belong there at all.
	results := 0
	for _, text := range historyTexts(a.Bot) {
		if strings.Contains(text, "todo listo") {
			results++
		}
		if strings.Contains(text, "⏱") {
			t.Fatalf("a progress frame reached the history: %q", text)
		}
	}
	if results != 1 {
		t.Fatalf("the result must be recorded exactly once, got %d", results)
	}
}

// TestLongTaskResultFallsBackToTheChunkedSend covers the one case where a task
// legitimately produces a second message.
func TestLongTaskResultFallsBackToTheChunkedSend(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.progressInterval = 20 * time.Millisecond
	ctx := context.Background()

	tracker := b.NewProgress(ownerChat, true, time.Now())
	tracker.SetHeader("⏳ Tarea t1 — larga")
	tracker.Add(toolStart("c1", "run_terminal_command", "algo"))
	waitFor(t, func() bool { return progressSends(fake.Sent()) == 1 }, "the progress message")

	long := strings.Repeat("línea de resultado muy larga\n", 400)
	if len([]rune(long)) <= tg.MaxMessageRunes {
		t.Fatal("the fixture must exceed one Telegram message")
	}
	if tracker.FinishWith(ctx, long) {
		t.Fatal("a result that does not fit must not claim to have been edited in")
	}
	if !tracker.FinishHeader(ctx, "✅ Tarea t1 — listo · resultado abajo ↓") {
		t.Fatal("the message must still be turned into a header")
	}
	if last := fake.lastText("editMessageText"); !strings.Contains(last, "resultado abajo") {
		t.Fatalf("unexpected final header: %q", last)
	}
	if fake.calls("deleteMessage") != 0 {
		t.Fatalf("the header replaces the steps rather than removing the message: %+v", fake.Sent())
	}
}

// TestProgressReachesTheAppAndIsReplayed: the phone follows the same run over
// SSE, and a client that connects mid-run is handed the steps so far.
func TestProgressReachesTheAppAndIsReplayed(t *testing.T) {
	a, fake := newTestAssistantWithTelegram(t)
	b := a.Bot
	b.opts.Agent.SoftTimeout = "30s"
	b.progressInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := b.events.Subscribe()
	defer b.events.Unsubscribe(events)

	running := make(chan struct{})
	release := make(chan struct{})
	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		spec.OnProgress(toolStart("c1", "run_terminal_command", "ls -la"))
		pollFor(func() bool { return progressSends(fake.Sent()) >= 1 }, 5*time.Second)
		close(running)
		<-release
		return &AgentResult{Stdout: "hecho", Duration: time.Millisecond}, nil
	})

	b.enqueue(ctx, ownerText(1, "haz algo"))
	<-running

	if replay := b.ProgressSnapshot(ownerChat); len(replay) != 1 || replay[0].Detail != "ls -la" {
		t.Fatalf("a client connecting now must be handed the current step, got %+v", replay)
	}

	var seen *ProgressEvent
	for seen == nil {
		select {
		case ev := <-events:
			if ev.Type == EventProgress {
				seen = ev.Progress
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no progress event reached the app channel")
		}
	}
	if seen.Kind != ProgressToolStart || seen.Detail != "ls -la" {
		t.Fatalf("unexpected progress event: %+v", seen)
	}
	close(release)

	waitFor(t, func() bool { return containsText(historyTexts(b), "hecho") }, "the answer")
	waitFor(t, func() bool { return len(b.ProgressSnapshot(ownerChat)) == 0 },
		"the finished run's buffer to be released")
}
