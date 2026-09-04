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
)

// Emitter is the outbound choke point, as seen by the scheduler, the task
// manager and the job-result router. Only *Bot implements it.
type Emitter interface {
	Emit(ctx context.Context, out Outbound) error
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
}

// Emit delivers one outbound message and is the single choke point for
// everything the assistant says.
//
// For the owner's chat it (1) appends to the durable history, (2) broadcasts to
// the app's SSE clients and pushes when the app is closed, and (3) sends to
// Telegram. Group chats only get step 3: the app mirrors the owner's own
// conversation, not every room the assistant sits in.
//
// No other code may call the Telegram client's Send* for the owner chat.
func (b *Bot) Emit(ctx context.Context, out Outbound) error {
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

	if target == b.ownerTelegram {
		b.record(out)
	}
	if out.SkipTelegram || b.tg == nil {
		return nil
	}
	if out.File != "" {
		return b.tg.SendFile(ctx, target, out.File, out.Text)
	}
	return b.tg.SendLongText(ctx, target, out.Text)
}

// record persists an owner-chat message and fans it out to the app.
func (b *Bot) record(out Outbound) {
	if b.history == nil {
		return
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
		return
	}
	ev := AppEvent{Type: out.Event, Message: &stored}
	if out.Event == EventError {
		ev.Error = text
	}
	b.events.Broadcast(ev)
	if out.Event != EventError {
		b.pushOutbound(&stored)
	}
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
	if err := b.Emit(ctx, Outbound{ChatID: chatID, Text: text, Source: SourceSend}); err != nil {
		log.Printf("assistant: could not deliver a notice: %v", err)
	}
}
