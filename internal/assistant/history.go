package assistant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// Message roles on the app channel.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Message sources. The conversation is shared; this only records where a turn
// entered the system.
const (
	SourceApp      = "app"
	SourceTelegram = "telegram"
	SourceSend     = "send"
	SourceJob      = "job"
)

// Attachment is a file that accompanied a message. Path is the server-side
// inbox location and is never sent to the app.
type Attachment struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
	Path string `json:"-"`
}

// HistoryMessage is one persisted turn of the shared conversation.
type HistoryMessage struct {
	ID        string       `json:"id"`
	Role      string       `json:"role"`
	Text      string       `json:"text"`
	Files     []Attachment `json:"files,omitempty"`
	Source    string       `json:"source"`
	CreatedAt time.Time    `json:"created_at"`
}

// History is the durable conversation the phone app pages on a cold start.
// Telegram's own history is not enough: the app needs a store it can query.
type History struct {
	st     *store.Store
	userID string
	ctx    context.Context
}

// NewHistory binds the conversation log to one owner.
func NewHistory(ctx context.Context, st *store.Store, userID string) *History {
	return &History{st: st, userID: userID, ctx: ctx}
}

// Append records a message and returns the stored copy (with id and timestamp).
func (h *History) Append(msg HistoryMessage) (HistoryMessage, error) {
	if h == nil {
		return HistoryMessage{}, fmt.Errorf("history is not configured")
	}
	if msg.ID == "" {
		msg.ID = newMessageID()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	files := "[]"
	if len(msg.Files) > 0 {
		raw, err := json.Marshal(msg.Files)
		if err != nil {
			return HistoryMessage{}, err
		}
		files = string(raw)
	}
	err := h.st.AppendAssistantMessage(h.ctx, h.userID, store.AssistantMessage{
		ID: msg.ID, Role: msg.Role, Text: msg.Text,
		FilesJSON: files, Source: msg.Source, CreatedAt: msg.CreatedAt,
	})
	if err != nil {
		return HistoryMessage{}, err
	}
	return msg, nil
}

// List returns a window of the conversation, oldest first. after pages forwards
// from an id, before pages backwards from one; neither returns the newest
// `limit` messages.
func (h *History) List(after, before string, limit int) []HistoryMessage {
	if h == nil {
		return nil
	}
	rows, err := h.st.ListAssistantMessages(h.ctx, h.userID, after, before, limit)
	if err != nil {
		return nil
	}
	out := make([]HistoryMessage, 0, len(rows))
	for _, r := range rows {
		m := HistoryMessage{
			ID: r.ID, Role: r.Role, Text: r.Text, Source: r.Source, CreatedAt: r.CreatedAt,
		}
		if r.FilesJSON != "" && r.FilesJSON != "[]" {
			_ = json.Unmarshal([]byte(r.FilesJSON), &m.Files)
		}
		out = append(out, m)
	}
	return out
}

// newMessageID returns a 16-char hex identifier.
func newMessageID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
