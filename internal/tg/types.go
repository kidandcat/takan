// Package tg is the single Telegram Bot API client used by the hub: the
// assistant's long-poll loop, the media downloads, and the telegram_send tool
// all go through it.
package tg

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Chat kinds, normalised from Telegram's five chat types.
const (
	ChatPrivate = "private"
	ChatGroup   = "group"
)

// User is the subset of Telegram's User object the hub needs.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat identifies the conversation a message belongs to.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

// Entity marks up a span of message text, used to spot @mentions.
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// File covers every Telegram media object the hub handles (voice, audio, photo
// size, video, video note and document all share these fields).
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
	FilePath     string `json:"file_path"`
	Duration     int    `json:"duration"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

// Message is the subset of Telegram's Message object the hub reacts to.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Voice     *File  `json:"voice"`
	Audio     *File  `json:"audio"`
	Photo     []File `json:"photo"`
	Video     *File  `json:"video"`
	VideoNote *File  `json:"video_note"`
	Document  *File  `json:"document"`
	// ReplyToMessage lets the owner address the bot by replying to it.
	ReplyToMessage  *Message `json:"reply_to_message"`
	Entities        []Entity `json:"entities"`
	CaptionEntities []Entity `json:"caption_entities"`
}

// IsGroup reports whether the message came from a group or supergroup.
func (m *Message) IsGroup() bool { return NormalizeChatType(m.Chat.Type) == ChatGroup }

// SenderLabel is how the agent should refer to the sender in a group.
func (m *Message) SenderLabel() string {
	if m.From == nil {
		return "alguien"
	}
	if name := strings.TrimSpace(m.From.FirstName); name != "" {
		return name
	}
	if m.From.Username != "" {
		return "@" + m.From.Username
	}
	return fmt.Sprintf("usuario %d", m.From.ID)
}

// ChatLabel is a human name for the chat, for logs and first-turn context.
func (m *Message) ChatLabel() string {
	if title := strings.TrimSpace(m.Chat.Title); title != "" {
		return title
	}
	if m.From != nil {
		if name := strings.TrimSpace(m.From.FirstName); name != "" {
			return name
		}
	}
	return fmt.Sprintf("chat %d", m.Chat.ID)
}

// Update is one entry of a getUpdates response.
type Update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *Message `json:"message"`
	EditedMessage *Message `json:"edited_message"`
}

// NormalizeChatType maps Telegram's chat types onto private / group.
func NormalizeChatType(telegramType string) string {
	switch telegramType {
	case "group", "supergroup", "channel":
		return ChatGroup
	default:
		return ChatPrivate
	}
}

// WebhookInfo describes the webhook currently registered for the bot.
type WebhookInfo struct {
	URL                  string   `json:"url"`
	PendingUpdateCount   int      `json:"pending_update_count"`
	AllowedUpdates       []string `json:"allowed_updates"`
	LastErrorMessage     string   `json:"last_error_message"`
	IPAddress            string   `json:"ip_address"`
	HasCustomCertificate bool     `json:"has_custom_certificate"`
}

// DiscoveredChat is a chat seen in a getUpdates sweep.
type DiscoveredChat struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
	First    string `json:"first_name,omitempty"`
	Last     string `json:"last_name,omitempty"`
}

// FormatChatLabel builds a short human label for a discovered chat.
func FormatChatLabel(c DiscoveredChat) string {
	if c.Title != "" {
		return c.Title
	}
	name := strings.TrimSpace(c.First + " " + c.Last)
	if name != "" {
		if c.Username != "" {
			return name + " (@" + c.Username + ")"
		}
		return name
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	if c.Type != "" {
		return c.Type + " " + c.ID
	}
	return c.ID
}

// APIError carries a failed Bot API call so callers can react to it (for
// example by retrying a message without Markdown parsing).
type APIError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is the seconds Telegram asked the caller to wait, set on a
	// 429. Zero on every other failure.
	RetryAfter int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed (%d): %s", e.Method, e.Code, e.Description)
}

// RetryDelay is how long to wait before trying again, or zero when Telegram did
// not ask for a wait.
func (e *APIError) RetryDelay() time.Duration {
	if e.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(e.RetryAfter) * time.Second
}

// AsAPIError reports whether err is an *APIError and stores it in target.
// It unwraps, so a wrapped 409 is still recognised as the webhook conflict.
func AsAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}
