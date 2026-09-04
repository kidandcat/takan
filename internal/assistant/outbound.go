package assistant

import (
	"context"
	"log"
	"path/filepath"
)

// SSE event types on the app channel. "message" is an unsolicited message,
// "done" closes a conversational turn, "error" reports a failed one.
const (
	EventMessage = "message"
	EventDone    = "done"
	EventError   = "error"
	// EventInterrupted terminates a turn whose run a newer message killed. It
	// carries no message: there is no half-answer to show. See AppEvent.
	EventInterrupted = "interrupted"
	// EventProgress is one step of the run in flight. It is advisory: it never
	// carries a message, it is not persisted, and dropping it costs nothing.
	EventProgress = "progress"
)

// Outbound kinds. An assistant message is normally sent, but the run-progress
// display needs to rewrite and remove one it already sent, and those have to go
// through the same choke point as everything else.
const (
	// OutboundSend posts a new message. The zero value.
	OutboundSend = ""
	// OutboundEdit rewrites an existing message, addressed by MessageID.
	OutboundEdit = "edit"
	// OutboundDelete removes an existing message, addressed by MessageID.
	OutboundDelete = "delete"
)

// Emitter is the outbound choke point, as seen by the scheduler, the task
// manager and the job-result router. Only *Bot implements it.
type Emitter interface {
	Emit(ctx context.Context, out Outbound) (Receipt, error)
}

// Receipt identifies a delivered message on both channels.
type Receipt struct {
	// MessageID is Telegram's id. It is only set for a single-part send, which
	// is what a caller needing an id (telegram_send) always does.
	MessageID int64
	// StoredID is the app history id, empty when the message was not recorded.
	StoredID string
}

// Outbound is one thing the assistant says. Every outbound message goes through
// Emit, which is the only place allowed to touch the Telegram client for the
// owner's chat.
//
// This exists because of a real bug: replies went to the app history but
// reminders, routine output, slash-command answers and system notices did not,
// so the phone showed a conversation with holes in it that Telegram did not
// have.
type Outbound struct {
	// Text is the message body.
	Text string
	// File is an optional local path to upload. It is sent as a photo when the
	// extension says so, otherwise as a document.
	File string
	// ChatID targets a chat; zero means the owner's own chat.
	ChatID int64
	// Source records how the message came about: app, telegram, send or job.
	Source string
	// Event is the SSE type the app sees. Defaults to EventMessage.
	Event string
	// SkipTelegram suppresses the Telegram send. Used when the turn arrived
	// only from the app, so the phone is the only channel expecting an answer.
	SkipTelegram bool
	// ParseMode requests an explicit markup mode ("plain", "HTML", "Markdown",
	// "MarkdownV2"). Setting it forces a single-message send, so the caller gets
	// a message id back; the text must fit in one Telegram message.
	//
	// Left empty, Emit uses the chunked send, which tries Markdown and falls
	// back to plain text — the right default for agent-written replies.
	ParseMode string
	// Kind selects the Telegram operation: send (default), edit or delete.
	Kind string
	// MessageID is the message an edit or a delete targets.
	MessageID int64
	// SkipHistory keeps a message out of the app history and out of the push
	// channel, delivering it to Telegram alone.
	//
	// The progress message is what this exists for: it is rewritten every few
	// seconds and then removed, so recording each frame would fill the phone's
	// conversation with lines that no longer exist in the chat. The app follows
	// the same run through SSE `progress` events instead. A background task's
	// final edit-into-result does NOT set it: that text is the task's answer and
	// belongs in the history exactly once.
	SkipHistory bool
}

// Emit delivers one outbound message and is the single choke point for
// everything the assistant says.
//
// For the owner's chat it (1) appends to the durable history, (2) broadcasts to
// the app's SSE clients and pushes when the app is closed, and (3) sends to
// Telegram. Group chats only get step 3: the app mirrors the owner's own
// conversation, not every room the assistant sits in.
//
// Editing and deleting go through here too (Kind), for the same reason: the
// run-progress display rewrites and removes a message it sent, and a second
// path to the Telegram client is exactly how the history drifts again. Those
// two set SkipHistory, so the app follows the run through SSE instead.
//
// No other code may call the Telegram client's Send*/Edit*/Delete* for the
// owner chat.
func (b *Bot) Emit(ctx context.Context, out Outbound) (Receipt, error) {
	target := out.ChatID
	if target == 0 {
		target = b.ownerTelegram
	}
	if out.Event == "" {
		out.Event = EventMessage
	}
	if out.Source == "" {
		out.Source = SourceSend
	}

	var receipt Receipt
	if target == b.ownerTelegram && !out.SkipHistory && out.Kind != OutboundDelete {
		receipt.StoredID = b.record(out)
	}
	if out.SkipTelegram || b.tg == nil {
		return receipt, nil
	}
	switch out.Kind {
	case OutboundEdit:
		return receipt, b.tg.EditMessageText(ctx, target, out.MessageID, out.Text, out.ParseMode)
	case OutboundDelete:
		return receipt, b.tg.DeleteMessage(ctx, target, out.MessageID)
	}
	switch {
	case out.File != "":
		return receipt, b.tg.SendFile(ctx, target, out.File, out.Text)
	case out.ParseMode != "":
		id, err := b.tg.SendMessage(ctx, target, out.Text, out.ParseMode)
		receipt.MessageID = id
		return receipt, err
	default:
		return receipt, b.tg.SendLongText(ctx, target, out.Text)
	}
}

// record persists an owner-chat message and fans it out to the app, returning
// the stored id.
func (b *Bot) record(out Outbound) string {
	if b.history == nil {
		return ""
	}
	text := out.Text
	var files []Attachment
	if out.File != "" {
		files = append(files, Attachment{Name: filepath.Base(out.File), Kind: kindForName(out.File)})
		if text == "" {
			text = filepath.Base(out.File)
		}
	}
	stored, err := b.history.Append(HistoryMessage{
		Role: RoleAssistant, Text: text, Files: files, Source: out.Source,
	})
	if err != nil {
		log.Printf("assistant: could not persist an outgoing message: %v", err)
		// Still tell any live client, so the app is not silently stale.
		b.events.Broadcast(AppEvent{Type: out.Event, Error: errorTextFor(out)})
		return ""
	}
	ev := AppEvent{Type: out.Event, Message: &stored}
	if out.Event == EventError {
		ev.Error = text
	}
	b.events.Broadcast(ev)
	if out.Event != EventError {
		b.pushOutbound(&stored)
	}
	return stored.ID
}

// errorTextFor is the error string carried by an event when persistence failed.
func errorTextFor(out Outbound) string {
	if out.Event == EventError {
		return out.Text
	}
	return ""
}

// say is the shorthand used by the daemon's own notices: unsolicited, to the
// owner, on every channel.
func (b *Bot) say(ctx context.Context, chatID int64, text string) {
	if _, err := b.Emit(ctx, Outbound{ChatID: chatID, Text: text, Source: SourceSend}); err != nil {
		log.Printf("assistant: could not deliver a notice: %v", err)
	}
}
