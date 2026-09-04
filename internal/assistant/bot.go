package assistant

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// queueSize bounds the per-chat backlog of pending messages.
const queueSize = 32

// pollStallThreshold is how long getUpdates may keep failing before /health
// starts reporting the assistant as unhealthy.
const pollStallThreshold = 5 * time.Minute

// strangerLogWindow rate-limits the "ignored a stranger" log line to one per
// chat per hour, so a bot that gets crawled does not fill the journal.
const strangerLogWindow = time.Hour

// Bot wires the Telegram poller to the CLI agent runner.
type Bot struct {
	opts  Options
	tg    *tg.Client
	state *StateStore
	stt   *Transcriber
	push  *PushSender
	// me is written by the poll loop after getMe and read from HTTP handlers
	// (the panel, /health, takan_status), so it is atomic rather than a plain
	// pointer field.
	me atomic.Pointer[tg.User]

	// ownerTelegram is the owner's Telegram USER id. In a private chat it equals
	// the chat id, so it doubles as the default DM target.
	ownerTelegram int64
	// dataDir holds the inbox; workdir is the agent workspace.
	dataDir string
	workdir string
	home    string

	// runners holds one runner per chat. Runs are serialised per chat, but the
	// chat itself never stops responding: commands are answered instantly and
	// messages arriving mid-run are acknowledged and coalesced. Telegram and the
	// native app share the owner's runner so they cannot double-run the agent.
	runners   map[int64]*chatRunner
	runnersMu sync.Mutex

	// history is the durable conversation the native app pages.
	history *History
	// events fans typing / done / error events out to SSE clients.
	events *EventBus

	// tasks runs long work detached from the conversation.
	tasks *TaskManager
	// usage reports the CLI agent's consumption for /usage.
	usage UsageReporter

	// startHook replaces the CLI agent in tests.
	startHook func(ctx context.Context, spec RunSpec) (*RunHandle, error)
	// noticeHook captures outgoing notices in tests.
	noticeHook func(text string)

	// runCtx is the process lifetime. App-channel requests must not use the HTTP
	// request context or the worker dies when the handler returns.
	runCtx context.Context

	startedAt     time.Time
	lastUpdateAt  atomic.Int64
	processedRuns atomic.Int64
	// interruptedRuns counts conversational runs killed by a newer message.
	// They are not processedRuns: nothing was delivered and nothing was billed
	// as a completed turn, and the question they answered is re-asked by the
	// run that replaced them.
	interruptedRuns atomic.Int64
	// runsToday counts conversational runs since midnight, for /usage.
	runsMu    sync.Mutex
	runDays   map[string]int
	strangers map[int64]time.Time

	// lastPollOK is the unix time of the last successful getUpdates call, and
	// lastPollErr the most recent polling failure. Together they let /health
	// report a bot that is running but not actually receiving anything, which is
	// what happens if another service steals the bot's webhook.
	lastPollOK  atomic.Int64
	lastPollErr atomic.Value
	// lastWebhookReclaim is when the assistant last deleted a foreign webhook.
	lastWebhookReclaim atomic.Int64

	// ready closes once getMe has succeeded.
	ready     chan struct{}
	readyOnce sync.Once

	// stopped records why the poll loop exited for good. A dead poller and a
	// slow start look identical to a timeout-based health check for the whole
	// grace window, which is five minutes of a silent assistant.
	stopped atomic.Value
}

// markStopped records that the poll loop has exited and will not resume.
func (b *Bot) markStopped(err error) {
	if err != nil {
		b.stopped.Store(err.Error())
	}
}

// StoppedReason is why polling ended, or "" while it is still running.
func (b *Bot) StoppedReason() string {
	reason, _ := b.stopped.Load().(string)
	return reason
}

// Ready is closed once the Telegram identity is known.
func (b *Bot) Ready() <-chan struct{} { return b.ready }

func (b *Bot) markReady() { b.readyOnce.Do(func() { close(b.ready) }) }

// setMe records the bot's own Telegram identity.
func (b *Bot) setMe(u *tg.User) { b.me.Store(u) }

// identity is the bot's own account, nil before getMe has answered.
func (b *Bot) identity() *tg.User { return b.me.Load() }

// Username is the bot's @handle: the live one once getMe has answered, else the
// last one recorded, so the panel is not blank while polling is broken.
func (b *Bot) Username() string {
	if me := b.identity(); me != nil && me.Username != "" {
		return me.Username
	}
	return b.state.BotUsername()
}

// PollHealth reports whether polling is currently working.
func (b *Bot) PollHealth() (healthy bool, lastOK time.Time, lastErr string) {
	if v := b.lastPollErr.Load(); v != nil {
		lastErr, _ = v.(string)
	}
	if ts := b.lastPollOK.Load(); ts > 0 {
		lastOK = time.Unix(ts, 0)
	}
	// Before the first successful poll, allow one threshold of grace from startup.
	reference := b.startedAt
	if !lastOK.IsZero() {
		reference = lastOK
	}
	return time.Since(reference) < pollStallThreshold, lastOK, lastErr
}

// SetTasks wires the background task manager used by /tasks and /cancel.
func (b *Bot) SetTasks(tasks *TaskManager) { b.tasks = tasks }

// SetUsage wires the consumption reporter behind /usage.
func (b *Bot) SetUsage(u UsageReporter) { b.usage = u }

// isOwnerChat reports whether a chat is the operator's own DM. The app channel
// and the durable history mirror only that conversation.
func (b *Bot) isOwnerChat(chatID int64) bool { return chatID == b.ownerTelegram }

// SetRunContext binds queue workers to the process lifetime.
func (b *Bot) SetRunContext(ctx context.Context) { b.runCtx = ctx }

func (b *Bot) background() context.Context {
	if b.runCtx != nil {
		return b.runCtx
	}
	return context.Background()
}

// inboxDir is where inbound media is stored for the agent to read.
func (b *Bot) inboxDir() string { return filepath.Join(b.dataDir, "inbox") }

// inboxDirFor keeps each chat's files in its own subdirectory so attachments
// from one chat are never visible as loose files belonging to another.
func (b *Bot) inboxDirFor(chatID int64) string {
	return filepath.Join(b.dataDir, "inbox", strconv.FormatInt(chatID, 10))
}

// queuedMsg is one inbound turn waiting for the agent, from any channel.
type queuedMsg struct {
	tg  *tg.Message
	app *appInbound

	// superseded marks a turn whose run was interrupted by a later message. It
	// is atomic because the interrupting goroutine sets it while the worker may
	// still be rendering the batch it belongs to.
	superseded atomic.Bool

	// rendered caches the prompt built for this message. An interrupted turn is
	// re-queued and rendered again, and re-rendering would download its photo a
	// second time and pay Groq for a second transcription of the same voice note.
	rendered   string
	renderedOK bool
	// persisted records that this Telegram message already reached the app
	// history, so a re-queued turn does not appear twice on the phone.
	persisted bool
}

// appInbound is a message that arrived through the native app.
type appInbound struct {
	ID        string
	Text      string
	Files     []string
	VoicePath string
}

// chatRunner owns one chat's serial execution slot plus its pending backlog.
type chatRunner struct {
	mu sync.Mutex
	// pending holds messages waiting for the current run to finish. They are
	// coalesced into a single prompt rather than replayed one run at a time.
	pending []*queuedMsg
	// current is the batch the in-flight run is answering. It exists so an
	// interrupt can put that turn back at the front of pending instead of
	// losing it: the next run answers it together with the message that
	// interrupted it.
	current []*queuedMsg
	// running reports whether an agent run is in flight for this chat.
	running bool
	// startedAt is when the in-flight run began.
	startedAt time.Time
	// cancel stops the in-flight run; nil when idle.
	cancel context.CancelFunc
	// handle is the live agent process, once started. It is cleared when the
	// turn ends, whether it completed or was promoted to a background task.
	handle *RunHandle
	// cancelledByUser distinguishes /cancel from the process shutting down.
	// They produce the same cancelled result, but only one of them is something
	// the operator already knows about.
	cancelledByUser bool
	// interrupted marks the in-flight run as superseded by a newer message. Its
	// answer must be discarded rather than delivered: by the time it lands, the
	// question it answers has already been replaced.
	interrupted bool
	// wake nudges the worker that new work arrived.
	wake chan struct{}
}

// setHandle points the runner at the live agent process. It reports false when
// the run was interrupted while it was still starting, in which case the caller
// owns stopping it.
func (r *chatRunner) setHandle(h *RunHandle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		return false
	}
	r.handle = h
	return true
}

// superseded reports whether the in-flight run has been interrupted.
func (r *chatRunner) superseded() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted
}

// claimTurn ends the turn and reports whether this caller owns its delivery.
//
// It is the single point where "the run finished" races "a new message
// arrived": whoever gets here first wins. On true the batch is released, so a
// later interrupt cannot re-queue a turn that has already been answered — and,
// just as importantly, cannot kill a run that has just been handed to the task
// manager. On false the caller must emit nothing at all.
func (r *chatRunner) claimTurn() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		return false
	}
	r.current = nil
	r.handle = nil
	return true
}

// releaseTurn ends a turn that has nothing to deliver, so a late interrupt does
// not re-queue a batch nobody is working on any more.
func (r *chatRunner) releaseTurn() { _ = r.claimTurn() }

// interrupt stops the in-flight conversational run because a newer message has
// arrived, and folds the interrupted turn back into the backlog so the next run
// answers both together. It reports whether a run was actually interrupted.
//
// It deliberately does nothing once the turn has been claimed (delivered, or
// promoted to a background task): background work is never cancelled by a new
// message, only the conversational run in flight is.
func (r *chatRunner) interrupt() bool {
	r.mu.Lock()
	if !r.running || r.interrupted || len(r.current) == 0 {
		r.mu.Unlock()
		return false
	}
	handle, cancel := r.handle, r.cancel
	if handle != nil && handle.Finished() {
		// The answer is already written; let it be delivered and let the new
		// message start the next run behind it.
		r.mu.Unlock()
		return false
	}
	r.interrupted = true
	for _, m := range r.current {
		m.superseded.Store(true)
	}
	r.pending = append(append(make([]*queuedMsg, 0, len(r.current)+len(r.pending)), r.current...), r.pending...)
	r.current = nil
	r.mu.Unlock()

	// Kill the process itself, not just the bookkeeping: the run was started on
	// the process context so it survives promotion, which means cancelling the
	// turn context alone would leave grok running and burning tokens.
	if handle != nil {
		handle.Cancel()
	} else if cancel != nil {
		// No process yet — the turn is still downloading attachments or
		// transcribing. Cancelling the turn context stops it before it starts.
		cancel()
	}
	return true
}

// clearHandle detaches the agent process from the runner.
func (r *chatRunner) clearHandle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handle = nil
}

// busy reports whether a run is in flight and for how long.
func (r *chatRunner) busy() (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false, 0
	}
	return true, time.Since(r.startedAt)
}

// stop cancels the in-flight run, reporting whether there was one. It targets
// the agent process when one is running, so /cancel kills the real work rather
// than just the bookkeeping around it.
func (r *chatRunner) stop() bool {
	r.mu.Lock()
	handle, cancel, running := r.handle, r.cancel, r.running
	r.cancelledByUser = true
	r.mu.Unlock()

	if handle != nil && !handle.Finished() {
		handle.Cancel()
		return true
	}
	if running && cancel != nil {
		cancel()
		return true
	}

	r.mu.Lock()
	r.cancelledByUser = false
	r.mu.Unlock()
	return false
}

// userCancelled reports whether /cancel stopped the current run.
func (r *chatRunner) userCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelledByUser
}

// Run polls Telegram until the context is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	me, err := b.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	b.setMe(me)
	b.markReady()
	if err := b.state.SetBotUsername(me.Username); err != nil {
		log.Printf("could not record the bot username: %v", err)
	}
	log.Printf("%s authorized as @%s (id %d), owner telegram id %d", InstanceName, me.Username, me.ID, b.ownerTelegram)
	log.Printf("agent command: %s (workdir %s, soft budget %s, hard cap %s)",
		b.describeAgent(), b.workdir, b.opts.SoftTimeout(), b.opts.TaskTimeout())

	if b.stt == nil {
		log.Printf("warning: GROQ_API_KEY is not set, voice messages cannot be transcribed")
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := b.tg.GetUpdates(ctx, b.state.Offset(), b.opts.PollTimeout())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b.lastPollErr.Store(err.Error())
			var apiErr *tg.APIError
			if tg.AsAPIError(err, &apiErr) && apiErr.Code == 409 {
				// Another service sharing this bot token registered a webhook,
				// which locks getUpdates out entirely. Take the bot back.
				log.Printf("CONFLICT: another consumer owns this bot's updates, the assistant is receiving nothing: %s", apiErr.Description)
				if b.reclaimWebhook(ctx) {
					continue
				}
			}
			log.Printf("getUpdates error: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		b.lastPollOK.Store(time.Now().Unix())
		b.lastPollErr.Store("")

		for i := range updates {
			update := updates[i]
			if err := b.state.SetOffset(update.UpdateID + 1); err != nil {
				log.Printf("failed to persist offset: %v", err)
			}
			b.lastUpdateAt.Store(time.Now().Unix())
			b.dispatch(ctx, &update)
		}
	}
}

func (b *Bot) describeAgent() string {
	return NewAgent(b.opts.Agent, b.workdir, b.home, b.opts.TaskTimeout()).Describe(RunNew)
}

// webhookReclaimWindow rate-limits the self-heal. Two services fighting over
// the same bot must not turn into a tight delete/register loop: after the first
// attempt inside the window the assistant backs off and lets /health go 503.
const webhookReclaimWindow = 10 * time.Minute

// reclaimWebhook deletes a foreign webhook so polling can resume, reporting
// whether the caller should retry immediately.
func (b *Bot) reclaimWebhook(ctx context.Context) bool {
	last := b.lastWebhookReclaim.Load()
	if last > 0 && time.Since(time.Unix(last, 0)) < webhookReclaimWindow {
		log.Printf("webhook conflict again within %s of the last reclaim; backing off instead of fighting for the bot",
			webhookReclaimWindow)
		return false
	}
	b.lastWebhookReclaim.Store(time.Now().Unix())

	url := "(unknown)"
	if info, err := b.tg.GetWebhookInfo(ctx); err != nil {
		log.Printf("could not read webhook info: %v", err)
	} else if info.URL != "" {
		url = info.URL
	}

	if err := b.tg.DeleteWebhook(ctx); err != nil {
		log.Printf("could not delete the conflicting webhook: %v", err)
		return false
	}
	log.Printf("removed conflicting webhook %s and resumed polling", url)

	go func() {
		b.say(context.WithoutCancel(ctx), 0,
			fmt.Sprintf("♻️ Otro servicio registró un webhook en el bot (%s); lo he quitado para recuperar el chat.", url))
	}()
	return true
}

// dispatch filters an update and hands accepted messages to the chat queue.
func (b *Bot) dispatch(ctx context.Context, update *tg.Update) {
	msg := update.Message
	if msg == nil {
		msg = update.EditedMessage
	}
	if msg == nil {
		return
	}
	if !b.accept(msg) {
		return
	}
	// A chat becomes known the first time the owner uses it; there is no
	// approval step, so the panel list is purely informational.
	if err := b.state.See(msg.Chat.ID, tg.NormalizeChatType(msg.Chat.Type), msg.ChatLabel()); err != nil {
		log.Printf("could not record chat %d: %v", msg.Chat.ID, err)
	}
	// Commands are answered outside the backlog, so they still work while an
	// agent run is in flight — but NOT on this goroutine. dispatch runs inline
	// in the getUpdates loop, and a command answer makes network calls (and, for
	// /usage, walks the whole session tree on disk). Blocking here stops the
	// assistant receiving anything at all until it finishes.
	if isCommand(msg) {
		go b.handleCommand(context.WithoutCancel(ctx), msg.Chat.ID, msg)
		return
	}
	b.enqueue(ctx, msg)
}

// accept is the whole authorization model: the gate is IDENTITY, not chat.
//
//  1. Private chat: served only when the sender is the owner. Anyone else gets
//     silence — no reply, no refusal, nothing in the panel.
//  2. Group: only messages the owner wrote, and only when addressed (a mention
//     or a reply to the assistant). respond_to_all lifts the mention
//     requirement but never the owner check. Other members' messages are
//     dropped before they can reach the prompt.
//  3. Channel posts arrive with no From and are ignored.
//
// Known limitation: a message posted anonymously as a group admin arrives as
// GroupAnonymousBot (id 1087968824), so it is ignored even when the owner sent it.
func (b *Bot) accept(msg *tg.Message) bool {
	if msg.From == nil {
		return false // channel posts, service messages
	}
	if msg.From.ID != b.ownerTelegram {
		b.logStranger(msg)
		return false
	}
	if !msg.IsGroup() {
		return true
	}
	if b.opts.RespondToAll {
		return true
	}
	return b.addressedToUs(msg)
}

// logStranger records an ignored sender once per chat per hour.
func (b *Bot) logStranger(msg *tg.Message) {
	b.runsMu.Lock()
	defer b.runsMu.Unlock()
	if b.strangers == nil {
		b.strangers = map[int64]time.Time{}
	}
	if last, ok := b.strangers[msg.Chat.ID]; ok && time.Since(last) < strangerLogWindow {
		return
	}
	b.strangers[msg.Chat.ID] = time.Now()
	log.Printf("ignoring message from non-owner %d in %s chat %d (%q)",
		msg.From.ID, tg.NormalizeChatType(msg.Chat.Type), msg.Chat.ID, msg.ChatLabel())
}

// addressedToUs decides whether a group message is for the assistant: an
// @mention or a reply to one of its own messages.
func (b *Bot) addressedToUs(msg *tg.Message) bool {
	if !msg.IsGroup() || b.opts.RespondToAll {
		return true
	}
	me := b.identity()
	// A reply to one of the assistant's own messages is addressed to it.
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
		me != nil && msg.ReplyToMessage.From.ID == me.ID {
		return true
	}
	if me == nil || me.Username == "" {
		return false
	}
	if mentionsUser(msg, me.Username) {
		return true
	}
	log.Printf("ignoring group message in %d (%q): not addressed to @%s",
		msg.Chat.ID, msg.ChatLabel(), me.Username)
	return false
}

// mentionsUser reports whether the message @-mentions username.
func mentionsUser(msg *tg.Message, username string) bool {
	needle := "@" + strings.ToLower(username)
	for _, field := range []struct {
		text     string
		entities []tg.Entity
	}{
		{msg.Text, msg.Entities},
		{msg.Caption, msg.CaptionEntities},
	} {
		lower := strings.ToLower(field.text)
		runes := []rune(field.text)
		for _, e := range field.entities {
			if e.Type != "mention" {
				continue
			}
			if e.Offset < 0 || e.Offset+e.Length > len(runes) {
				continue
			}
			if strings.EqualFold(string(runes[e.Offset:e.Offset+e.Length]), needle) {
				return true
			}
		}
		// Clients do not always send entities (edited or forwarded messages),
		// so fall back to a plain scan.
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// runner returns the chat's runner, starting its worker on first use.
func (b *Bot) runner(ctx context.Context, chatID int64) *chatRunner {
	b.runnersMu.Lock()
	defer b.runnersMu.Unlock()
	r, ok := b.runners[chatID]
	if !ok {
		r = &chatRunner{wake: make(chan struct{}, 1)}
		b.runners[chatID] = r
		go b.worker(ctx, chatID, r)
	}
	return r
}

// enqueue adds a Telegram message to the chat's backlog, interrupting whatever
// the chat was answering.
func (b *Bot) enqueue(ctx context.Context, msg *tg.Message) {
	b.enqueueMsg(ctx, msg.Chat.ID, &queuedMsg{tg: msg}, true)
}

// enqueueApp adds a native-app message to the owner's runner, the same one
// Telegram uses. The app is the same conversation, so it interrupts too.
func (b *Bot) enqueueApp(_ context.Context, in *appInbound) {
	b.enqueueMsg(b.background(), b.ownerTelegram, &queuedMsg{app: in}, false)
}

// RunningConversations is how many chats have a conversational run in flight.
//
// It is a gauge, not a counter, and it is the number that has to come back down
// after an interrupt: a killed run must free its chat's slot, or /health would
// show a conversation in flight forever and deploy.sh would wait out its whole
// window before every restart.
func (b *Bot) RunningConversations() int {
	b.runnersMu.Lock()
	runners := make([]*chatRunner, 0, len(b.runners))
	for _, r := range b.runners {
		runners = append(runners, r)
	}
	b.runnersMu.Unlock()

	n := 0
	for _, r := range runners {
		if busy, _ := r.busy(); busy {
			n++
		}
	}
	return n
}

// InterruptedRuns is how many conversational runs a newer message has killed.
func (b *Bot) InterruptedRuns() int64 { return b.interruptedRuns.Load() }

// ChatBusy reports whether the shared runner is in flight.
func (b *Bot) ChatBusy(chatID int64) (bool, time.Duration) {
	b.runnersMu.Lock()
	r := b.runners[chatID]
	b.runnersMu.Unlock()
	if r == nil {
		return false, 0
	}
	return r.busy()
}

// enqueueMsg is the shared queue entry point for every channel.
//
// A new message from the owner INTERRUPTS the conversational run in flight
// rather than queueing behind it. In practice the second message is a
// clarification of the first ("is this fixed?" + screenshot, then "it's Astra
// in the App Store"), so waiting out an answer to a question that has already
// been refined is wasted time and a wasted answer. The interrupted turn is not
// lost: it is folded into the next prompt, marked as superseded.
//
// Background work is never touched. Only the chat's own conversational run is
// interruptible; tasks (including runs auto-promoted past the soft budget),
// scheduled jobs and machine_ai_run jobs keep going.
func (b *Bot) enqueueMsg(ctx context.Context, chatID int64, msg *queuedMsg, telegramAck bool) {
	r := b.runner(ctx, chatID)

	r.mu.Lock()
	if len(r.pending) >= queueSize {
		r.mu.Unlock()
		log.Printf("chat %d backlog full, dropping inbound message", chatID)
		go b.emitError(context.WithoutCancel(ctx), chatID, !telegramAck,
			"Voy demasiado atrasado para encolar ese mensaje. Prueba en un momento.")
		return
	}
	r.pending = append(r.pending, msg)
	r.mu.Unlock()

	if r.interrupt() {
		b.interruptedRuns.Add(1)
		log.Printf("chat %d: a new message interrupted the run in flight; coalescing it into the next turn", chatID)
		// Deliberately silent on Telegram: the new run's typing indicator is
		// the whole story, and a "⏹ interrumpido" line every time he corrects
		// himself is noise in a chat he reads all day. The app gets a terminal
		// event instead, so it stops waiting on the discarded turn.
		b.events.Broadcast(AppEvent{Type: EventInterrupted})
	}

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// worker drains one chat's backlog, one agent run at a time. Each run takes
// every message that piled up and answers them as a single prompt.
func (b *Bot) worker(ctx context.Context, chatID int64, r *chatRunner) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}

		for {
			r.mu.Lock()
			if len(r.pending) == 0 {
				r.running = false
				r.cancel = nil
				r.current = nil
				r.interrupted = false
				r.mu.Unlock()
				break
			}
			batch := r.pending
			r.pending = nil
			runCtx, cancel := context.WithCancel(ctx)
			r.running = true
			r.interrupted = false
			r.startedAt = time.Now()
			r.cancel = cancel
			r.current = batch
			r.mu.Unlock()

			b.handleBatch(runCtx, chatID, r, batch)
			cancel()
		}
	}
}

// handleBatch turns a batch of messages into one agent run and one reply.
func (b *Bot) handleBatch(ctx context.Context, chatID int64, r *chatRunner, batch []*queuedMsg) {
	defer func() {
		if rec := recover(); rec != nil {
			r.releaseTurn()
			log.Printf("panic handling batch of %d message(s): %v\n%s", len(batch), rec, debug.Stack())
			b.emitError(context.WithoutCancel(ctx), chatID, false, "Error interno al procesar ese mensaje.")
		}
	}()

	r.mu.Lock()
	r.cancelledByUser = false
	r.mu.Unlock()

	prompt := b.buildBatchPrompt(ctx, chatID, batch)
	if strings.TrimSpace(prompt) == "" {
		r.releaseTurn()
		log.Printf("batch of %d message(s) produced an empty prompt, ignoring", len(batch))
		return
	}

	hasTG, _ := batchChannels(batch)
	b.persistTelegramInbound(chatID, batch)

	// Building the prompt downloads attachments and transcribes voice, which
	// takes seconds. If he wrote again in the meantime there is no point
	// starting a run for a question he has already replaced.
	if r.superseded() {
		return
	}

	var stopTyping func()
	if hasTG && b.tg != nil {
		stopTyping = b.startTyping(ctx, chatID)
		defer stopTyping()
	}
	started := time.Now()
	b.events.Broadcast(typingEvent(started))

	spec, note := b.nextRunSpec(chatID)
	if note != "" {
		prompt = note + "\n\n" + prompt
	}
	// A new session has no idea where it is; tell it once.
	if spec.Mode == RunNew {
		if intro := b.chatContext(chatID, batch); intro != "" {
			prompt = intro + "\n\n" + prompt
		}
	}
	spec.Prompt = prompt
	log.Printf("running agent for %d message(s) (mode=%s, session=%s, carried_context=%t, prompt %d chars)",
		len(batch), spec.Mode, spec.SessionID, note == "", len(prompt))

	// The run is started on the process context, not the batch context, so it
	// survives being promoted to a background task when this turn returns.
	handle, err := b.startAgent(b.background(), spec)
	if err != nil {
		if stopTyping != nil {
			stopTyping()
		}
		r.releaseTurn()
		log.Printf("could not start agent: %v", err)
		b.deliverError(context.WithoutCancel(ctx), chatID, hasTG, "No he podido arrancar el agente: "+tg.TruncateRunes(err.Error(), 200))
		return
	}

	// /cancel and the interrupt path target the live process from here on.
	if !r.setHandle(handle) {
		// He wrote again in the window between exec and this line. Kill it here
		// or nothing else will: interrupt() had no handle to cancel.
		handle.Cancel()
		return
	}
	defer r.clearHandle()

	sendCtx := context.WithoutCancel(ctx)

	select {
	case <-handle.Done():
		if stopTyping != nil {
			stopTyping()
		}
		if !r.claimTurn() {
			b.discardInterrupted(chatID, spec, handle)
			return
		}
		b.processedRuns.Add(1)
		b.noteRun()
		b.finishConversationalRun(sendCtx, chatID, hasTG, spec, handle, r)

	case <-time.After(b.opts.SoftTimeout()):
		// Over budget. Do NOT kill it: hand the live process to the task
		// manager, free the chat, and let the result arrive as a task.
		if stopTyping != nil {
			stopTyping()
		}
		// Claim before adopting: once the task manager owns the process, an
		// interrupt must not reach it. Background work outlives the chat.
		if !r.claimTurn() {
			b.discardInterrupted(chatID, spec, handle)
			return
		}
		b.promote(sendCtx, chatID, hasTG, spec, handle, r)
	}
}

// discardInterrupted throws away the output of a run that a newer message
// superseded. Nothing is emitted on any channel: the operator is about to get
// one answer covering both turns, and a half-answer to the question he just
// replaced would only confuse the thread.
//
// What this leaves behind in the runner's session (measured against grok 1.0.13,
// killed 15s into a turn): the session directory survives with the user turn
// written to chat_history.jsonl and NO assistant turn after it. That dangling
// user turn does not break anything — a later `grok --resume <session>` starts
// normally, appends the new user turn after it and answers, so the next turn
// needs no repair and no fresh session. The interrupted text is therefore in
// the session twice: once as the dangling turn, once in the coalesced prompt
// marked superseded. That is deliberate — the marker is what tells the model
// which of the two won.
func (b *Bot) discardInterrupted(chatID int64, spec RunSpec, handle *RunHandle) {
	elapsed := time.Since(handle.StartedAt())
	if handle.Finished() {
		res, _ := handle.Result() // safe-ignore: the outcome is discarded on purpose, this is only for the log line
		if d := durationOf(res); d > 0 {
			elapsed = d
		}
	}
	log.Printf("chat %d: discarding the interrupted run after %s (session %s); its turn is folded into the next prompt",
		chatID, elapsed.Truncate(time.Millisecond), spec.SessionID)
}

// noteRun counts a completed conversational run against today, for /usage.
func (b *Bot) noteRun() {
	b.runsMu.Lock()
	defer b.runsMu.Unlock()
	if b.runDays == nil {
		b.runDays = map[string]int{}
	}
	b.runDays[todayKey()]++
}

// RunsToday is how many conversational runs finished since midnight in Madrid.
func (b *Bot) RunsToday() int {
	b.runsMu.Lock()
	defer b.runsMu.Unlock()
	return b.runDays[todayKey()]
}

func todayKey() string {
	loc, err := time.LoadLocation(ScheduleLocation)
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc).Format("2006-01-02")
}

// finishConversationalRun delivers a run that completed within the budget.
func (b *Bot) finishConversationalRun(ctx context.Context, chatID int64, hasTG bool,
	spec RunSpec, handle *RunHandle, r *chatRunner) {
	res, err := handle.Result()

	if err != nil {
		if res != nil && res.Cancelled {
			log.Printf("agent run cancelled after %s", res.Duration)
			b.events.Broadcast(AppEvent{Type: "error", Error: "cancelled"})
			// /cancel is something the operator just asked for and was already
			// told about. A restart is not: without a word, his message simply
			// vanished and he is left waiting for an answer that never comes.
			if r != nil && !r.userCancelled() {
				b.noticeInterrupted(chatID, hasTG)
			}
			return
		}
		if res != nil && res.RateLimited && b.tasks != nil {
			b.tasks.NoteRateLimited()
		}
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		log.Printf("agent run failed after %s: %v\nstderr: %s", durationOf(res), err, tg.TruncateRunes(stderr, 4000))
		reply := "La ejecución del agente ha fallado: " + tg.TruncateRunes(err.Error(), 200)
		if res != nil && res.TimedOut {
			reply = fmt.Sprintf("El agente ha superado el límite de %s.", b.opts.TaskTimeout())
		}
		if res != nil && res.Stdout != "" {
			b.deliverAssistant(ctx, chatID, hasTG, res.Stdout)
		}
		b.deliverError(ctx, chatID, hasTG, reply)
		return
	}

	log.Printf("agent run finished in %s (%d chars of output)", res.Duration, len(res.Stdout))
	if err := b.state.MarkConversationStarted(chatID, spec.SessionID); err != nil {
		log.Printf("failed to persist chat state: %v", err)
	}
	b.deliverAssistant(ctx, chatID, hasTG, res.Stdout)
}

// promote moves an over-budget run to the background and says so.
func (b *Bot) promote(ctx context.Context, chatID int64, hasTG bool,
	spec RunSpec, handle *RunHandle, r *chatRunner) {
	if b.tasks == nil {
		// Without a task manager there is nowhere to hand it: keep waiting.
		log.Printf("soft budget exceeded but the task manager is unavailable; waiting for the run")
		<-handle.Done()
		b.processedRuns.Add(1)
		b.noteRun()
		b.finishConversationalRun(ctx, chatID, hasTG, spec, handle, r)
		return
	}

	task, err := b.tasks.Adopt(handle, tg.TruncateRunes(strings.TrimSpace(spec.Prompt), 60), chatID)
	if err != nil {
		log.Printf("failed to promote run to a task: %v", err)
		<-handle.Done()
		b.processedRuns.Add(1)
		b.noteRun()
		b.finishConversationalRun(ctx, chatID, hasTG, spec, handle, r)
		return
	}

	b.processedRuns.Add(1)
	b.noteRun()
	log.Printf("promoted over-budget run (session %s) to task %s after %s",
		spec.SessionID, task.ID, b.opts.SoftTimeout())

	// The promoted task owns this session now; the next turn forks from it.
	if err := b.state.MarkPromoted(chatID, spec.SessionID); err != nil {
		log.Printf("failed to persist promotion state: %v", err)
	}

	notice := fmt.Sprintf(
		"⏳ Esto está tardando; lo paso a background (tarea %s). Te aviso con el resultado — puedes seguir escribiéndome.",
		task.ID)
	b.deliverNotice(ctx, chatID, hasTG, notice)
}

// interruptedNotice is what the operator sees when a reply died with the
// process rather than being cancelled on purpose.
const interruptedNotice = "♻️ Me he reiniciado a mitad de respuesta; repíteme lo último."

// noticeInterrupted tells the chat its answer was lost to a restart.
//
// The send runs on a detached context with a short deadline: by the time this
// fires the run context is already cancelled, and during an actual shutdown the
// process may exit first. Best-effort is the right bar — the alternative is
// saying nothing at all.
func (b *Bot) noticeInterrupted(chatID int64, hasTG bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.background()), 5*time.Second)
	defer cancel()
	if _, err := b.Emit(ctx, Outbound{
		ChatID: chatID, Text: interruptedNotice, Source: SourceApp,
		Event: EventError, SkipTelegram: !hasTG,
	}); err != nil {
		log.Printf("could not report the interrupted run: %v", err)
	}
}

// deliverNotice sends an informational message on every active channel.
func (b *Bot) deliverNotice(ctx context.Context, chatID int64, hasTG bool, text string) {
	if b.noticeHook != nil {
		b.noticeHook(text)
	}
	if _, err := b.Emit(ctx, Outbound{
		ChatID: chatID, Text: text, Source: SourceApp, Event: EventDone, SkipTelegram: !hasTG,
	}); err != nil {
		log.Printf("failed to send notice: %v", err)
	}
}

// nextRunSpec decides how the next turn addresses its agent session. It also
// returns a note to prepend to the prompt, used when context could not be
// carried over.
//
// Forking is only safe once the promoted run has finished. Measured on vps2:
// forking a COMPLETED session takes ~5s and preserves context, but forking one
// that is still being written hung for over 6 minutes. So while the promoted
// run is in flight the conversation starts a fresh session and says so.
func (b *Bot) nextRunSpec(chatID int64) (RunSpec, string) {
	chat := b.state.Chat(chatID)
	switch {
	case chat.ForkFrom != "":
		if b.tasks != nil {
			if safe, taskID, reason := b.tasks.ForkSafety(chat.ForkFrom); !safe {
				log.Printf("not forking session %s (task %s %s); starting fresh", chat.ForkFrom, taskID, reason)
				note := fmt.Sprintf("[el turno anterior sigue ejecutándose en background como tarea %s; no tienes su contexto]", taskID)
				if reason != "still running" {
					note = fmt.Sprintf("[el turno anterior (tarea %s) no terminó bien; no tienes su contexto]", taskID)
				}
				return RunSpec{Mode: RunNew, SessionID: newSessionID()}, note
			}
		}
		// The promoted run finished cleanly, so its session is safe to branch
		// off: the conversation keeps its full context.
		return RunSpec{Mode: RunFork, ParentSession: chat.ForkFrom, SessionID: newSessionID()}, ""
	case chat.ConversationStarted && chat.SessionID != "":
		return RunSpec{Mode: RunResume, SessionID: chat.SessionID}, ""
	default:
		return RunSpec{Mode: RunNew, SessionID: newSessionID()}, ""
	}
}

func (b *Bot) startAgent(ctx context.Context, spec RunSpec) (*RunHandle, error) {
	if b.startHook != nil {
		return b.startHook(ctx, spec)
	}
	return NewAgent(b.opts.Agent, b.workdir, b.home, b.opts.TaskTimeout()).Start(ctx, spec)
}

func durationOf(res *AgentResult) time.Duration {
	if res == nil {
		return 0
	}
	return res.Duration
}

func batchChannels(batch []*queuedMsg) (hasTG, hasApp bool) {
	for _, m := range batch {
		if m.tg != nil {
			hasTG = true
		}
		if m.app != nil {
			hasApp = true
		}
	}
	return
}

func (b *Bot) persistTelegramInbound(chatID int64, batch []*queuedMsg) {
	// The app channel mirrors the OWNER conversation; group traffic must not
	// leak into the phone's history.
	if b.history == nil || chatID != b.ownerTelegram {
		return
	}
	for _, m := range batch {
		if m.tg == nil || m.persisted {
			continue
		}
		m.persisted = true
		text := strings.TrimSpace(m.tg.Text)
		if caption := strings.TrimSpace(m.tg.Caption); caption != "" {
			if text != "" {
				text += "\n\n" + caption
			} else {
				text = caption
			}
		}
		if text == "" {
			text = "(attachment)"
		}
		if _, err := b.history.Append(HistoryMessage{
			Role:   RoleUser,
			Text:   text,
			Source: SourceTelegram,
		}); err != nil {
			log.Printf("failed to persist telegram message: %v", err)
		}
	}
}

// deliverAssistant closes a conversational turn with the agent's answer.
func (b *Bot) deliverAssistant(ctx context.Context, chatID int64, hasTG bool, text string) {
	if _, err := b.Emit(ctx, Outbound{
		ChatID: chatID, Text: text, Source: SourceApp, Event: EventDone, SkipTelegram: !hasTG,
	}); err != nil {
		log.Printf("failed to send reply: %v", err)
	}
}

// deliverError closes a conversational turn that failed.
func (b *Bot) deliverError(ctx context.Context, chatID int64, hasTG bool, text string) {
	b.emitError(ctx, chatID, !hasTG, text)
}

// emitError is the error-shaped Emit, used for both turn failures and the
// daemon's own "something went wrong" notices.
func (b *Bot) emitError(ctx context.Context, chatID int64, skipTelegram bool, text string) {
	if _, err := b.Emit(ctx, Outbound{
		ChatID: chatID, Text: text, Source: SourceApp, Event: EventError, SkipTelegram: skipTelegram,
	}); err != nil {
		log.Printf("failed to send error: %v", err)
	}
}

// chatContext describes the chat to a freshly started session, so the agent
// knows whether it is talking to the owner alone or sitting in a group.
func (b *Bot) chatContext(chatID int64, batch []*queuedMsg) string {
	var msg *tg.Message
	for _, m := range batch {
		if m.tg != nil {
			msg = m.tg
			break
		}
	}
	if msg == nil {
		// App-channel only: that is always the owner's private conversation.
		return ""
	}
	if !msg.IsGroup() {
		if b.isOwnerChat(chatID) {
			return ""
		}
		return fmt.Sprintf("[contexto: chat privado con %s]", msg.ChatLabel())
	}
	intro := fmt.Sprintf("[contexto: estás en el grupo de Telegram %q. Solo te llegan los mensajes de Jairo", msg.ChatLabel())
	if !b.opts.RespondToAll {
		intro += " que te mencionan o que responden a un mensaje tuyo"
	}
	return intro + "; los demás participantes pueden leer tus respuestas.]"
}

// supersededMarker labels a turn whose run was interrupted by a later message.
// The interrupted text is still carried: in practice the newer message refines
// the older one rather than replacing it outright, so the agent needs both, in
// order, and needs to know which one is the live question.
const supersededMarker = "[interrupted, superseded by the next message]"

// promptPart is one rendered message inside a coalesced prompt.
type promptPart struct {
	text       string
	superseded bool
}

// buildBatchPrompt renders one prompt covering every message in the batch.
func (b *Bot) buildBatchPrompt(ctx context.Context, chatID int64, batch []*queuedMsg) string {
	parts := make([]promptPart, 0, len(batch))
	superseded := 0
	for _, msg := range batch {
		prompt, err := b.renderQueued(ctx, chatID, msg)
		if err != nil {
			log.Printf("failed to build prompt: %v", err)
			b.emitError(context.WithoutCancel(ctx), chatID, msg.tg == nil,
				"No he podido leer ese mensaje: "+tg.TruncateRunes(err.Error(), 300))
			continue
		}
		if strings.TrimSpace(prompt) == "" {
			continue
		}
		part := promptPart{text: prompt, superseded: msg.superseded.Load()}
		if part.superseded {
			superseded++
		}
		parts = append(parts, part)
	}

	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && superseded == 0 {
		return parts[0].text
	}

	var sb strings.Builder
	if superseded > 0 {
		// He wrote again mid-answer. Say plainly that the run was cut short so
		// the agent does not treat the first turn as still-open work.
		fmt.Fprintf(&sb, "Jairo interrupted you: you were answering the first %d message(s) below when he wrote again, "+
			"and that run was stopped. Answer everything together in one reply, treating the last message as the live question.\n",
			superseded)
	} else {
		// Several messages arrived while the previous run was busy. Answer them
		// as one turn rather than firing a run per message.
		fmt.Fprintf(&sb, "Jairo sent %d messages while you were busy. Answer them together.\n", len(parts))
	}
	for i, part := range parts {
		fmt.Fprintf(&sb, "\n--- message %d of %d ---\n", i+1, len(parts))
		if part.superseded {
			sb.WriteString(supersededMarker + "\n")
		}
		fmt.Fprintf(&sb, "%s\n", part.text)
	}
	return sb.String()
}

// renderQueued builds a message's prompt once and remembers it. An interrupted
// turn goes back on the queue and is rendered again for the coalesced prompt;
// without the cache that would re-download its photo from Telegram and pay for
// a second transcription of the same voice note. The cached text still names
// the inbox paths, so the attachments of both turns reach the new run.
func (b *Bot) renderQueued(ctx context.Context, chatID int64, msg *queuedMsg) (string, error) {
	if msg.renderedOK {
		return msg.rendered, nil
	}
	prompt, err := b.buildQueuedPrompt(ctx, chatID, msg)
	if err != nil {
		return "", err
	}
	msg.rendered, msg.renderedOK = prompt, true
	return prompt, nil
}

// isCommand reports whether a message is a slash command.
func isCommand(msg *tg.Message) bool {
	return strings.HasPrefix(strings.TrimSpace(msg.Text), "/")
}

// handleCommand implements the small set of Telegram slash commands and reports
// whether the message was consumed.
func (b *Bot) handleCommand(ctx context.Context, chatID int64, msg *tg.Message) bool {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	command, _, _ := strings.Cut(text, " ")
	command, _, _ = strings.Cut(command, "@")

	switch strings.ToLower(command) {
	case "/new", "/reset":
		// Keeping a run alive across a reset makes no sense: its answer belongs
		// to a conversation that no longer exists. Unlike an ordinary interrupt
		// the turn is NOT carried forward — starting fresh is the whole point —
		// so this is /cancel's path, not the coalescing one.
		if b.runner(ctx, chatID).stop() {
			log.Printf("chat %d: /new stopped the run in flight", chatID)
		}
		if err := b.state.ResetConversation(chatID); err != nil {
			log.Printf("failed to reset conversation: %v", err)
			b.emitError(ctx, chatID, false, "No he podido reiniciar la conversación.")
			return true
		}
		log.Printf("conversation reset for chat %d (session rotated)", chatID)
		b.say(ctx, chatID, "Empezamos conversación nueva.")
		return true

	case "/cancel", "/stop":
		if b.runner(ctx, chatID).stop() {
			log.Printf("cancelling in-flight run for chat %d on request", chatID)
			b.say(ctx, chatID, "🛑 He cancelado lo que estaba en marcha.")
			return true
		}
		// Nothing inline: what he means is almost certainly the run that was
		// just promoted to the background, so cancel that instead.
		if task, ok := b.cancelLatestPromoted(chatID); ok {
			b.say(ctx, chatID, fmt.Sprintf("🛑 He cancelado la tarea %s (%s).", task.ID, task.Title))
			return true
		}
		b.say(ctx, chatID, "No hay nada en marcha ahora mismo.")
		return true

	case "/tasks":
		b.say(ctx, chatID, b.tasksSummary(chatID))
		return true

	case "/usage":
		// Consumption is account-wide and names what the operator has been
		// working on, so it is only ever answered in his own chat.
		if !b.isOwnerChat(chatID) {
			b.say(ctx, chatID, "El consumo solo lo consulto en nuestro chat privado.")
			return true
		}
		b.say(ctx, chatID, b.UsageSummary())
		return true

	case "/start", "/help":
		b.say(ctx, chatID, strings.Join([]string{
			InstanceName + " — tu asistente personal.",
			"",
			"Mándame texto, un audio, una foto o un fichero y se lo paso al agente.",
			"Si me escribes mientras te estoy contestando, corto lo que estaba haciendo y respondo a todo junto.",
			fmt.Sprintf("Si una respuesta tarda más de %s, la paso sola a background y te aviso con el resultado. Eso ya no lo corto.",
				b.opts.SoftTimeout().Truncate(time.Second)),
			"",
			"/new — empezar conversación nueva",
			"/cancel — parar lo que esté en marcha",
			"/tasks — listar tareas en background",
			"/usage — consumo del agente (hoy, 7d, 30d)",
			"/status — estado y configuración",
		}, "\n"))
		return true

	case "/status":
		chat := b.state.Chat(chatID)
		lines := []string{
			fmt.Sprintf("activo desde hace: %s", time.Since(b.startedAt).Truncate(time.Second)),
			fmt.Sprintf("agente: %s", b.opts.Agent.Command),
			fmt.Sprintf("directorio: %s", b.workdir),
			fmt.Sprintf("presupuesto conversacional: %s", b.opts.SoftTimeout().Truncate(time.Second)),
			fmt.Sprintf("límite duro: %s", b.opts.TaskTimeout()),
			fmt.Sprintf("sesión: %s", sessionLabel(chat)),
			fmt.Sprintf("turnos: %d", chat.Runs),
		}
		if busy, elapsed := b.runner(ctx, chatID).busy(); busy {
			lines = append(lines, fmt.Sprintf("en ejecución: sí (%s)", elapsed.Truncate(time.Second)))
		} else {
			lines = append(lines, "en ejecución: no")
		}
		if b.tasks != nil {
			running := 0
			for _, t := range b.tasksFor(chatID) {
				if t.Running() {
					running++
				}
			}
			lines = append(lines, fmt.Sprintf("tareas en background: %d", running))
		}
		b.say(ctx, chatID, strings.Join(lines, "\n"))
		return true
	}
	return false
}

// sessionLabel renders the chat's session state for /status.
func sessionLabel(chat ChatState) string {
	switch {
	case chat.ForkFrom != "":
		return "se bifurcará de " + shortSession(chat.ForkFrom)
	case chat.ConversationStarted && chat.SessionID != "":
		return shortSession(chat.SessionID)
	default:
		return "nueva en el próximo mensaje"
	}
}

func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// cancelLatestPromoted kills the most recent still-running promoted task that
// belongs to this chat. A /cancel in a group must not reach into the operator's
// private conversation and stop work he started there.
func (b *Bot) cancelLatestPromoted(chatID int64) (Task, bool) {
	if b.tasks == nil {
		return Task{}, false
	}
	// List is newest first, so the first running promoted task is the one the
	// user just saw the promotion notice for.
	for _, t := range b.tasksFor(chatID) {
		if t.Running() && t.Promoted {
			if killed, err := b.tasks.Kill(t.ID); err != nil || !killed {
				log.Printf("could not kill promoted task %s: killed=%t err=%v", t.ID, killed, err)
				return Task{}, false
			}
			return t, true
		}
	}
	return Task{}, false
}

// tasksFor returns the tasks belonging to one chat, newest first. A task with
// no chat id was started from the operator's own conversation.
//
// Task titles and prompts are the operator's own words, so a group must only
// ever see what was started in that group.
func (b *Bot) tasksFor(chatID int64) []Task {
	if b.tasks == nil {
		return nil
	}
	var out []Task
	for _, t := range b.tasks.List() {
		owner := t.ChatID
		if owner == 0 {
			owner = b.ownerTelegram
		}
		if owner == chatID {
			out = append(out, t)
		}
	}
	return out
}

// tasksSummary renders the background task list for /tasks, scoped to the chat
// that asked.
func (b *Bot) tasksSummary(chatID int64) string {
	if b.tasks == nil {
		return "El subsistema de tareas no está disponible."
	}
	tasks := b.tasksFor(chatID)
	if len(tasks) == 0 {
		return "No hay tareas en background."
	}
	lines := make([]string, 0, len(tasks)+1)
	lines = append(lines, "Tareas en background:")
	for _, t := range tasks {
		lines = append(lines, "• "+t.Summary())
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) buildQueuedPrompt(ctx context.Context, chatID int64, msg *queuedMsg) (string, error) {
	if msg.app != nil {
		return b.buildAppPrompt(ctx, chatID, msg.app)
	}
	prompt, err := b.buildPrompt(ctx, chatID, msg.tg)
	if err != nil {
		return "", err
	}
	// In a group the assistant only ever sees the owner's messages, but the
	// attribution keeps the transcript readable when he quotes other people.
	if msg.tg != nil && msg.tg.IsGroup() && strings.TrimSpace(prompt) != "" {
		prompt = msg.tg.SenderLabel() + ": " + prompt
	}
	return prompt, nil
}

func (b *Bot) buildAppPrompt(ctx context.Context, chatID int64, in *appInbound) (string, error) {
	var sections []string
	if in.VoicePath != "" {
		transcript, err := b.transcribeFile(ctx, chatID, in.VoicePath)
		if err != nil {
			log.Printf("app: voice transcription failed: %v", err)
			sections = append(sections, voiceFailureNote(in.VoicePath, err))
		} else {
			sections = append(sections, "[voice message transcript] "+transcript)
		}
	}
	if text := strings.TrimSpace(in.Text); text != "" {
		sections = append(sections, text)
	}
	if len(in.Files) > 0 {
		lines := []string{"[attached files saved for you to open]"}
		lines = append(lines, in.Files...)
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n"), nil
}

// buildPrompt renders the agent prompt for a message, downloading any media and
// transcribing voice notes first.
func (b *Bot) buildPrompt(ctx context.Context, chatID int64, msg *tg.Message) (string, error) {
	var sections []string

	if audio := pickAudio(msg); audio != nil {
		transcript, path, err := b.transcribe(ctx, chatID, audio)
		switch {
		case err != nil && path == "":
			return "", err
		case err != nil:
			log.Printf("voice transcription failed: %v", err)
			sections = append(sections, voiceFailureNote(path, err))
		default:
			sections = append(sections, "[voice message transcript] "+transcript)
		}
	}

	if text := strings.TrimSpace(msg.Text); text != "" {
		sections = append(sections, text)
	}
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		sections = append(sections, caption)
	}

	paths, err := b.downloadAttachments(ctx, chatID, msg)
	if err != nil {
		return "", err
	}
	if len(paths) > 0 {
		lines := []string{"[attached files saved for you to open]"}
		lines = append(lines, paths...)
		sections = append(sections, strings.Join(lines, "\n"))
	}

	return strings.Join(sections, "\n\n"), nil
}

// transcribe downloads a voice or audio file and returns its transcript plus
// the saved path. The path is returned even when transcription fails so the
// caller can still hand the audio to the agent instead of losing the message.
func (b *Bot) transcribe(ctx context.Context, chatID int64, audio *tg.File) (string, string, error) {
	if b.tg != nil {
		_ = b.tg.SendChatAction(ctx, chatID, "typing")
	}
	path, err := b.download(ctx, chatID, audio, "voice")
	if err != nil {
		return "", "", err
	}
	transcript, err := b.transcribeFile(ctx, chatID, path)
	return transcript, path, err
}

// voiceFailureNote keeps a turn alive when speech-to-text is broken: the agent
// still receives the audio path and the reason, so it can answer in the
// conversation instead of the user getting a raw API error.
func voiceFailureNote(path string, err error) string {
	return "[voice message could not be transcribed: " + tg.TruncateRunes(err.Error(), 200) + "]\n" +
		"The audio file is saved for you to open: " + path
}

func (b *Bot) transcribeFile(ctx context.Context, chatID int64, path string) (string, error) {
	if b.stt == nil {
		return "", fmt.Errorf("voice transcription is unavailable (GROQ_API_KEY not set)")
	}
	if b.tg != nil {
		_ = b.tg.SendChatAction(ctx, chatID, "typing")
	}
	transcript, err := b.stt.Transcribe(ctx, path)
	if err != nil {
		return "", err
	}
	if transcript == "" {
		return "", fmt.Errorf("the voice message transcribed to nothing")
	}
	log.Printf("transcribed %s (%d chars)", filepath.Base(path), len(transcript))
	return transcript, nil
}

// downloadAttachments saves photos, videos and documents into the inbox and
// returns their absolute paths.
func (b *Bot) downloadAttachments(ctx context.Context, chatID int64, msg *tg.Message) ([]string, error) {
	var paths []string

	if len(msg.Photo) > 0 {
		// Telegram sends ascending sizes; the last entry is the largest.
		largest := msg.Photo[len(msg.Photo)-1]
		path, err := b.download(ctx, chatID, &largest, "photo")
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	for _, item := range []struct {
		file *tg.File
		kind string
	}{
		{msg.Video, "video"},
		{msg.VideoNote, "video-note"},
		{msg.Document, "document"},
	} {
		if item.file == nil {
			continue
		}
		path, err := b.download(ctx, chatID, item.file, item.kind)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// download resolves a file_id and stores the content under the inbox directory.
func (b *Bot) download(ctx context.Context, chatID int64, file *tg.File, kind string) (string, error) {
	resolved, err := b.tg.GetFile(ctx, file.FileID)
	if err != nil {
		return "", err
	}
	name := file.FileName
	if name == "" {
		ext := extForMime(file.MimeType, filepath.Ext(resolved.FilePath))
		if ext == "" {
			ext = ".bin"
		}
		name = kind + ext
	}
	dir := b.inboxDirFor(chatID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := inboxPath(dir, name)
	if err := b.tg.DownloadFile(ctx, resolved.FilePath, dst); err != nil {
		return "", err
	}
	if info, err := os.Stat(dst); err == nil {
		log.Printf("saved %s to %s (%d bytes)", kind, dst, info.Size())
	}
	return dst, nil
}

// startTyping refreshes the typing indicator until the returned func is called.
func (b *Bot) startTyping(ctx context.Context, chatID int64) func() {
	typingCtx, cancel := context.WithCancel(ctx)
	var once sync.Once

	go func() {
		ticker := time.NewTicker(b.opts.TypingInterval())
		defer ticker.Stop()
		for {
			if err := b.tg.SendChatAction(typingCtx, chatID, "typing"); err != nil && typingCtx.Err() == nil {
				log.Printf("sendChatAction failed: %v", err)
			}
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() { once.Do(cancel) }
}

// pickAudio returns the voice or audio attachment of a message, if any.
func pickAudio(msg *tg.Message) *tg.File {
	if msg.Voice != nil {
		return msg.Voice
	}
	if msg.Audio != nil {
		return msg.Audio
	}
	return nil
}
