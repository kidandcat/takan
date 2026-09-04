package assistant

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

const (
	// progressEditInterval is the floor between two edits of the progress
	// message. Telegram tolerates roughly one message per second per chat, and
	// this chat also carries the real conversation, so the display is paced well
	// under the limit and coalesces whatever arrived in between.
	progressEditInterval = 2500 * time.Millisecond
	// progressRateLimitStop is how many 429s end the display for the rest of the
	// run. Once Telegram is pushing back twice, the progress lines are the least
	// important thing in the chat and they stop, permanently, so the answer
	// itself is not the request that gets refused.
	progressRateLimitStop = 2
	// progressFailureStop is the same idea for ordinary failures.
	progressFailureStop = 3
	// progressCallTimeout bounds one Telegram call made by the display.
	progressCallTimeout = 15 * time.Second
)

// progressTracker owns THE single Telegram message a run gets.
//
// The rule it exists to enforce: one message per run, edited in place, never a
// stream of new ones. It is created on the run's first tool call (a run that
// answers straight away never produces one), rewritten at most every
// progressEditInterval with the last few steps, and then either deleted — a
// conversational run, just before its real answer is posted — or edited into the
// final result, which is what a background task does, because that message is
// the task's only trace in the chat.
type progressTracker struct {
	out  Emitter
	sink func(ProgressEvent)
	base context.Context

	chatID int64
	// telegram is false for a turn that arrived only through the app. There is
	// no message to edit then; the app still gets the SSE events.
	telegram bool
	started  time.Time

	wake chan struct{}
	quit chan struct{}
	once sync.Once

	mu         sync.Mutex
	ring       progressRing
	header     string
	messageID  int64
	shown      string
	nextAt     time.Time
	interval   time.Duration
	rateLimits int
	failures   int
	// stopped ends the edit loop without ending the run.
	stopped bool
	// done marks the message as finished with: deleted, or edited into a result.
	done bool
	// released hands ownership to someone else (the task manager, after a
	// promotion), so the conversational turn's cleanup leaves it alone.
	released bool
}

// newProgressTracker starts a display for one run.
func newProgressTracker(base context.Context, out Emitter, sink func(ProgressEvent),
	chatID int64, telegram bool, started time.Time) *progressTracker {
	if started.IsZero() {
		started = time.Now()
	}
	p := &progressTracker{
		out: out, sink: sink, base: base,
		chatID: chatID, telegram: telegram, started: started,
		wake: make(chan struct{}, 1), quit: make(chan struct{}),
		interval: progressEditInterval,
	}
	go p.loop()
	return p
}

// NewProgress builds a tracker wired to this bot's outbound choke point and its
// app event bus.
func (b *Bot) NewProgress(chatID int64, telegram bool, started time.Time) *progressTracker {
	p := newProgressTracker(b.background(), b, b.broadcastProgress, chatID, telegram, started)
	if b.progressInterval > 0 {
		p.mu.Lock()
		p.interval = b.progressInterval
		p.mu.Unlock()
	}
	return p
}

// broadcastProgress puts one step on the app's SSE channel.
func (b *Bot) broadcastProgress(ev ProgressEvent) {
	b.events.Broadcast(AppEvent{Type: EventProgress, Progress: &ev})
}

// Add records one step. It is called from the runner's stdout reader, so it
// never blocks: the work happens on the tracker's own goroutine.
func (p *progressTracker) Add(ev ProgressEvent) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.ring.store(ev)
	p.mu.Unlock()
	if p.sink != nil {
		p.sink(ev)
	}
	p.kick()
}

// Snapshot is the run's steps so far, for an app that connects mid-run.
func (p *progressTracker) Snapshot() []ProgressEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ring.snapshot()
}

// SetHeader pins a line above the steps — a background task's identity, which
// has to stay visible while the steps underneath it scroll.
func (p *progressTracker) SetHeader(text string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.header = strings.TrimSpace(text)
	p.nextAt = time.Time{}
	p.mu.Unlock()
	p.kick()
}

// MessageID is the Telegram message being edited, or zero when the run has not
// produced one.
func (p *progressTracker) MessageID() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.messageID
}

// Release hands the message to another owner. Discard becomes a no-op, so a
// promoted run's display survives the conversational turn that started it.
func (p *progressTracker) Release() *progressTracker {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.released = true
	p.mu.Unlock()
	return p
}

// kick nudges the edit loop.
func (p *progressTracker) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// stop ends the edit loop. It is idempotent.
func (p *progressTracker) stop() { p.once.Do(func() { close(p.quit) }) }

// loop paces the edits: one flush per wake, never closer together than the
// throttle allows, so a burst of ten tool calls in a second produces one edit
// showing the last of them rather than ten showing each.
func (p *progressTracker) loop() {
	for {
		select {
		case <-p.quit:
			return
		case <-p.wake:
		}

		p.mu.Lock()
		wait := time.Until(p.nextAt)
		p.mu.Unlock()
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-p.quit:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		p.flush()
	}
}

// flush renders the current state and sends or edits the message.
func (p *progressTracker) flush() {
	p.mu.Lock()
	if p.stopped || p.done || !p.telegram {
		p.mu.Unlock()
		return
	}
	body := renderProgress(p.header, p.ring.snapshot(), time.Since(p.started))
	if body == "" || body == p.shown {
		p.mu.Unlock()
		return
	}
	messageID := p.messageID
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.base), progressCallTimeout)
	defer cancel()

	var newID int64
	var err error
	if messageID == 0 {
		// ParseMode forces a single-message send, which is what returns an id —
		// and plain text, because a redacted command line is not Markdown.
		var receipt Receipt
		receipt, err = p.out.Emit(ctx, Outbound{
			ChatID: p.chatID, Text: body, Source: SourceSend,
			ParseMode: "plain", SkipHistory: true,
		})
		newID = receipt.MessageID
	} else {
		_, err = p.out.Emit(ctx, Outbound{
			ChatID: p.chatID, Kind: OutboundEdit, MessageID: messageID, Text: body,
			Source: SourceSend, ParseMode: "plain", SkipHistory: true,
		})
	}

	if orphan := p.settle(err, body, newID); orphan != 0 {
		// The run ended while this frame was in flight and created the message
		// after the cleanup had already looked for one. Take it back out.
		p.deleteMessage(ctx, orphan)
	}
}

// settle records the outcome of one edit and returns a message id that was
// created too late to keep.
func (p *progressTracker) settle(err error, body string, newID int64) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err == nil {
		if p.done && newID != 0 {
			return newID
		}
		if newID != 0 {
			p.messageID = newID
		}
		p.shown = body
		p.rateLimits, p.failures = 0, 0
		p.nextAt = time.Now().Add(p.interval)
		return 0
	}

	var apiErr *tg.APIError
	if tg.AsAPIError(err, &apiErr) && apiErr.Code == 429 {
		p.rateLimits++
		if p.rateLimits >= progressRateLimitStop {
			p.stopped = true
			log.Printf("assistant: Telegram rate-limited the progress message twice; no more edits for this run")
			return 0
		}
		p.interval *= 2
		delay := apiErr.RetryDelay()
		if delay < p.interval {
			delay = p.interval
		}
		p.nextAt = time.Now().Add(delay)
		p.kick()
		return 0
	}

	p.failures++
	log.Printf("assistant: progress message update failed (%d/%d): %v", p.failures, progressFailureStop, err)
	if p.failures >= progressFailureStop {
		p.stopped = true
		return 0
	}
	p.nextAt = time.Now().Add(p.interval)
	return 0
}

// Discard removes the progress message. It is what closes a conversational run:
// the message is deleted right before the real answer is posted, and equally on
// an interrupt, a /cancel or a /new, because nothing about a killed run should
// survive it. Calling it twice, or on a run that never showed anything, does
// nothing.
func (p *progressTracker) Discard(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.released || p.done {
		p.mu.Unlock()
		return
	}
	p.done = true
	id := p.messageID
	p.messageID = 0
	p.mu.Unlock()

	p.stop()
	if id != 0 {
		p.deleteMessage(ctx, id)
	}
	p.terminal()
}

// deleteMessage removes one message, best effort.
func (p *progressTracker) deleteMessage(ctx context.Context, id int64) {
	if !p.telegram || id == 0 {
		return
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), progressCallTimeout)
	defer cancel()
	if _, err := p.out.Emit(callCtx, Outbound{
		ChatID: p.chatID, Kind: OutboundDelete, MessageID: id, Source: SourceSend, SkipHistory: true,
	}); err != nil {
		log.Printf("assistant: could not remove the progress message: %v", err)
	}
}

// FinishWith edits the progress message into the run's final text and records
// that text in the conversation exactly once. It reports false when there is no
// message to edit, the text does not fit one, or Telegram refused — in which
// case the caller falls back to sending it normally.
func (p *progressTracker) FinishWith(ctx context.Context, text string) bool {
	return p.finish(ctx, text, true)
}

// FinishHeader edits the progress message into a short line WITHOUT recording
// it. It is the long-result path: the message becomes a one-line header and the
// full text follows through the ordinary chunked send, which is the only case
// where a run legitimately produces a second message.
func (p *progressTracker) FinishHeader(ctx context.Context, text string) bool {
	return p.finish(ctx, text, false)
}

func (p *progressTracker) finish(ctx context.Context, text string, record bool) bool {
	if p == nil {
		return false
	}
	body := strings.TrimSpace(tg.RewriteTablesForTelegram(text))
	if body == "" || len([]rune(body)) > tg.MaxMessageRunes {
		return false
	}

	p.mu.Lock()
	id := p.messageID
	if p.done || id == 0 || !p.telegram {
		p.mu.Unlock()
		return false
	}
	p.done = true
	p.mu.Unlock()
	p.stop()

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), progressCallTimeout)
	defer cancel()

	if _, err := p.out.Emit(callCtx, Outbound{
		ChatID: p.chatID, Kind: OutboundEdit, MessageID: id, Text: body,
		Source: SourceSend, ParseMode: "Markdown", SkipHistory: true,
	}); err != nil {
		log.Printf("assistant: could not edit the progress message into the result: %v", err)
		p.mu.Lock()
		p.done = false
		p.mu.Unlock()
		return false
	}

	if record {
		// The Telegram side is already done; this is the app history and the
		// push, through the same path every other assistant message takes.
		if _, err := p.out.Emit(callCtx, Outbound{
			ChatID: p.chatID, Text: text, Source: SourceSend, SkipTelegram: true,
		}); err != nil {
			log.Printf("assistant: could not record the task result: %v", err)
		}
	}
	p.terminal()
	return true
}

// Announce makes text the header of the progress message and records it in the
// conversation.
//
// This is how a promoted run's "moved to background" notice works: the message
// already in the chat becomes the task's message rather than a second one being
// posted next to it. A run that never showed progress sends the notice normally
// and adopts it, so the task still ends up with exactly one message to edit.
func (p *progressTracker) Announce(ctx context.Context, text string) {
	if p == nil {
		return
	}
	p.SetHeader(text)

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), progressCallTimeout)
	defer cancel()

	if !p.telegram || p.MessageID() != 0 {
		if _, err := p.out.Emit(callCtx, Outbound{
			ChatID: p.chatID, Text: text, Source: SourceApp, Event: EventDone, SkipTelegram: true,
		}); err != nil {
			log.Printf("assistant: could not record the promotion notice: %v", err)
		}
		return
	}

	receipt, err := p.out.Emit(callCtx, Outbound{
		ChatID: p.chatID, Text: text, Source: SourceApp, Event: EventDone, ParseMode: "plain",
	})
	if err != nil {
		log.Printf("assistant: could not send the promotion notice: %v", err)
		return
	}
	p.mu.Lock()
	if receipt.MessageID != 0 {
		p.messageID = receipt.MessageID
		p.shown = text
	}
	p.mu.Unlock()
}

// terminal tells the app the run's progress stream is over, so it stops showing
// a step that will never be followed by another.
func (p *progressTracker) terminal() {
	if p.sink == nil {
		return
	}
	p.sink(ProgressEvent{Kind: ProgressEnd, Elapsed: time.Since(p.started)})
}
