package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Chat kinds. Telegram "supergroup"/"channel" are normalised to group.
const (
	TelegramChatPrivate = "private"
	TelegramChatGroup   = "group"
)

// Attachment directions.
//
// A channel's bot token supports a single getUpdates consumer, so a channel can
// have at most one receive attachment; sending is unlimited.
const (
	DirectionSend    = "send"
	DirectionReceive = "receive"
)

// Consumer kinds that can attach to a channel.
const (
	ConsumerBot      = "bot"      // a bot daemon instance (ConsumerID = bot id)
	ConsumerNotifier = "notifier" // the internal operator notifier / telegram_send
	ConsumerEmail    = "email"    // email module notifications
)

// TelegramChannel is a named Telegram bot credential plus the chats it serves.
//
// It is the unit every consumer addresses: the bots module, the internal
// notifier and the email module all attach to channels rather than holding
// tokens of their own. The clear BotFather token never leaves cryptox.Box.
type TelegramChannel struct {
	ID        string
	UserID    string
	Name      string
	TokenEnc  string
	BotUser   string // @username reported by getMe
	BotName   string // display name reported by getMe
	IsDefault bool
	CreatedAt time.Time
	// Chats and Attachments are filled by the List/Get helpers.
	Chats       []TelegramChannelChat
	Attachments []ChannelAttachment
}

// TelegramChannelChat is one destination inside a channel.
type TelegramChannelChat struct {
	ID        string
	ChannelID string
	ChatID    string
	Type      string // private | group
	Label     string
	CreatedAt time.Time
}

// Label is a short human description of a chat.
func (c TelegramChannelChat) Display() string {
	if l := strings.TrimSpace(c.Label); l != "" {
		return l
	}
	return c.ChatID
}

// ChannelAttachment binds a consumer to a channel in one direction.
type ChannelAttachment struct {
	ID         string
	UserID     string
	ChannelID  string
	Consumer   string // bot | notifier | email
	ConsumerID string // bot id; empty for singleton consumers
	Direction  string // send | receive
	// ChatID is the consumer's primary chat inside the channel (empty = first).
	ChatID    string
	CreatedAt time.Time
	// ChannelName / ConsumerLabel are filled for display by the List helpers.
	ChannelName   string
	ConsumerLabel string
}

// ReceiveConsumer reports whether this attachment consumes inbound messages.
func (a ChannelAttachment) ReceiveConsumer() bool { return a.Direction == DirectionReceive }

func (s *Store) migrateTelegramChannels() error {
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS telegram_channels (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  token_enc TEXT NOT NULL,
  bot_username TEXT NOT NULL DEFAULT '',
  bot_name TEXT NOT NULL DEFAULT '',
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_telegram_channels_user ON telegram_channels(user_id);

CREATE TABLE IF NOT EXISTS telegram_channel_chats (
  id TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL REFERENCES telegram_channels(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chat_id TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'private',
  label TEXT NOT NULL DEFAULT '',
  position INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(channel_id, chat_id)
);
CREATE INDEX IF NOT EXISTS idx_telegram_channel_chats ON telegram_channel_chats(channel_id);

CREATE TABLE IF NOT EXISTS telegram_attachments (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel_id TEXT NOT NULL REFERENCES telegram_channels(id) ON DELETE CASCADE,
  consumer TEXT NOT NULL,
  consumer_id TEXT NOT NULL DEFAULT '',
  direction TEXT NOT NULL,
  chat_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(channel_id, consumer, consumer_id, direction)
);
CREATE INDEX IF NOT EXISTS idx_telegram_attachments_channel ON telegram_attachments(channel_id);
CREATE INDEX IF NOT EXISTS idx_telegram_attachments_consumer ON telegram_attachments(user_id, consumer, consumer_id);
-- One getUpdates consumer per bot token.
CREATE UNIQUE INDEX IF NOT EXISTS idx_telegram_attachments_receive
  ON telegram_attachments(channel_id) WHERE direction = 'receive';
`); err != nil {
		return err
	}
	return s.seedTelegramChannels()
}

// seedTelegramChannels turns the single credential the telegram module used
// before channels existed into the default channel (with the operator's chat and
// a notifier send-attachment), so notifications and telegram_send keep working
// with no reconfiguration.
func (s *Store) seedTelegramChannels() error {
	rows, err := s.db.Query(`
SELECT user_id, bot_token_enc, bot_username, default_chat_id, allowed_chats
FROM telegram_settings
WHERE bot_token_enc <> ''
  AND user_id NOT IN (SELECT user_id FROM telegram_channels)`)
	if err != nil {
		return err
	}
	type seed struct{ user, tok, botUser, chat, allowed string }
	var seeds []seed
	for rows.Next() {
		var sd seed
		if err := rows.Scan(&sd.user, &sd.tok, &sd.botUser, &sd.chat, &sd.allowed); err != nil {
			rows.Close()
			return err
		}
		seeds = append(seeds, sd)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, sd := range seeds {
		chID := uuid.NewString()
		if _, err := s.db.Exec(`
INSERT INTO telegram_channels (id, user_id, name, token_enc, bot_username, is_default, created_at)
VALUES (?,?,?,?,?,1,?)`,
			chID, sd.user, "default", sd.tok, strings.TrimPrefix(sd.botUser, "@"), now); err != nil {
			return fmt.Errorf("seed telegram channel: %w", err)
		}
		var chats []TelegramChat
		_ = json.Unmarshal([]byte(sd.allowed), &chats)
		if c := strings.TrimSpace(sd.chat); c != "" {
			chats = append([]TelegramChat{{ID: c, Label: "Operator"}}, chats...)
		}
		seen := map[string]bool{}
		pos := 0
		for _, c := range chats {
			id := strings.TrimSpace(c.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			kind := TelegramChatPrivate
			if strings.HasPrefix(id, "-") {
				kind = TelegramChatGroup
			}
			if _, err := s.db.Exec(`
INSERT INTO telegram_channel_chats (id, channel_id, user_id, chat_id, type, label, position, created_at)
VALUES (?,?,?,?,?,?,?,?)`,
				uuid.NewString(), chID, sd.user, id, kind, stringMin(c.Label, 160), pos, now); err != nil {
				return fmt.Errorf("seed channel chat: %w", err)
			}
			pos++
		}
		// The operator notifier keeps sending exactly where it did before.
		if _, err := s.db.Exec(`
INSERT INTO telegram_attachments (id, user_id, channel_id, consumer, consumer_id, direction, chat_id, created_at)
VALUES (?,?,?,?,'',?,?,?)`,
			uuid.NewString(), sd.user, chID, ConsumerNotifier, DirectionSend,
			strings.TrimSpace(sd.chat), now); err != nil {
			return fmt.Errorf("seed notifier attachment: %w", err)
		}
	}
	return nil
}

// NormalizeTelegramChatType maps Telegram chat types onto private|group.
func NormalizeTelegramChatType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "group", "supergroup", "channel":
		return TelegramChatGroup
	default:
		return TelegramChatPrivate
	}
}

const telegramChannelSelect = `
SELECT id, user_id, name, token_enc, bot_username, bot_name, is_default, created_at
FROM telegram_channels`

func scanTelegramChannel(row rowScanner) (*TelegramChannel, error) {
	var c TelegramChannel
	var def int
	var created sql.NullString
	if err := row.Scan(&c.ID, &c.UserID, &c.Name, &c.TokenEnc, &c.BotUser, &c.BotName,
		&def, &created); err != nil {
		return nil, err
	}
	c.IsDefault = def != 0
	c.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &c, nil
}

// CreateTelegramChannel stores a validated channel. tokenEnc must already be
// sealed by the caller; this layer never sees the clear BotFather token.
func (s *Store) CreateTelegramChannel(ctx context.Context, userID, name, tokenEnc, botUser, botName string) (*TelegramChannel, error) {
	name = normalizeName(name)
	if name == "" {
		return nil, fmt.Errorf("channel name required")
	}
	if strings.TrimSpace(tokenEnc) == "" {
		return nil, fmt.Errorf("sealed token required")
	}
	n, err := s.CountTelegramChannels(ctx, userID)
	if err != nil {
		return nil, err
	}
	isDefault := 0
	if n == 0 {
		isDefault = 1 // first channel is the default destination
	}
	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO telegram_channels (id, user_id, name, token_enc, bot_username, bot_name, is_default, created_at)
VALUES (?,?,?,?,?,?,?,?)`,
		id, userID, name, tokenEnc, strings.TrimPrefix(strings.TrimSpace(botUser), "@"),
		stringMin(strings.TrimSpace(botName), 120), isDefault,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("create channel: %w", err)
	}
	return s.TelegramChannelByID(ctx, userID, id)
}

func (s *Store) CountTelegramChannels(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM telegram_channels WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// ListTelegramChannels returns channels with their chats and attachments.
func (s *Store) ListTelegramChannels(ctx context.Context, userID string) ([]TelegramChannel, error) {
	rows, err := s.db.QueryContext(ctx,
		telegramChannelSelect+` WHERE user_id = ? ORDER BY is_default DESC, lower(name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TelegramChannel
	for rows.Next() {
		c, err := scanTelegramChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Chats, _ = s.ListChannelChats(ctx, out[i].ID)
		out[i].Attachments, _ = s.ListChannelAttachments(ctx, userID, out[i].ID)
	}
	return out, nil
}

func (s *Store) TelegramChannelByID(ctx context.Context, userID, id string) (*TelegramChannel, error) {
	c, err := scanTelegramChannel(s.db.QueryRowContext(ctx,
		telegramChannelSelect+` WHERE user_id = ? AND id = ?`, userID, id))
	if err != nil {
		return nil, err
	}
	c.Chats, _ = s.ListChannelChats(ctx, c.ID)
	c.Attachments, _ = s.ListChannelAttachments(ctx, userID, c.ID)
	return c, nil
}

func (s *Store) TelegramChannelByName(ctx context.Context, userID, name string) (*TelegramChannel, error) {
	c, err := scanTelegramChannel(s.db.QueryRowContext(ctx,
		telegramChannelSelect+` WHERE user_id = ? AND name = ?`, userID, strings.TrimSpace(name)))
	if err != nil {
		return nil, err
	}
	c.Chats, _ = s.ListChannelChats(ctx, c.ID)
	return c, nil
}

// DefaultTelegramChannel returns the channel used when none is named.
func (s *Store) DefaultTelegramChannel(ctx context.Context, userID string) (*TelegramChannel, error) {
	c, err := scanTelegramChannel(s.db.QueryRowContext(ctx,
		telegramChannelSelect+` WHERE user_id = ? ORDER BY is_default DESC, created_at LIMIT 1`, userID))
	if err != nil {
		return nil, err
	}
	c.Chats, _ = s.ListChannelChats(ctx, c.ID)
	return c, nil
}

// ResolveTelegramChannel accepts a channel name or id; empty picks the default.
func (s *Store) ResolveTelegramChannel(ctx context.Context, userID, nameOrID string) (*TelegramChannel, error) {
	ref := strings.TrimSpace(nameOrID)
	if ref == "" {
		return s.DefaultTelegramChannel(ctx, userID)
	}
	if c, err := s.TelegramChannelByID(ctx, userID, ref); err == nil {
		return c, nil
	}
	return s.TelegramChannelByName(ctx, userID, ref)
}

// SetDefaultTelegramChannel moves the default flag.
func (s *Store) SetDefaultTelegramChannel(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE telegram_channels SET is_default = CASE WHEN id = ? THEN 1 ELSE 0 END WHERE user_id = ?`,
		id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteTelegramChannel refuses while any consumer is still attached.
func (s *Store) DeleteTelegramChannel(ctx context.Context, userID, id string) error {
	atts, err := s.ListChannelAttachments(ctx, userID, id)
	if err != nil {
		return err
	}
	if len(atts) > 0 {
		return fmt.Errorf("channel still has %d attachment(s) — detach them first", len(atts))
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM telegram_channels WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	// Losing the default leaves the oldest remaining channel in charge.
	var count int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM telegram_channels WHERE user_id = ? AND is_default = 1`, userID).Scan(&count)
	if count == 0 {
		_, _ = s.db.ExecContext(ctx, `
UPDATE telegram_channels SET is_default = 1
WHERE id = (SELECT id FROM telegram_channels WHERE user_id = ? ORDER BY created_at LIMIT 1)`, userID)
	}
	return nil
}

// --- chats ---

// AddChannelChat registers a destination. Idempotent per (channel, chat_id).
func (s *Store) AddChannelChat(ctx context.Context, userID, channelID, chatID, kind, label string) error {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return fmt.Errorf("chat id required")
	}
	if _, err := s.TelegramChannelByID(ctx, userID, channelID); err != nil {
		return fmt.Errorf("unknown channel")
	}
	if strings.TrimSpace(kind) == "" {
		kind = TelegramChatPrivate
		if strings.HasPrefix(chatID, "-") {
			kind = TelegramChatGroup
		}
	}
	// Append at the end so "the channel's first chat" stays the first one added.
	var next int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), -1) + 1 FROM telegram_channel_chats WHERE channel_id = ?`,
		channelID).Scan(&next)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO telegram_channel_chats (id, channel_id, user_id, chat_id, type, label, position, created_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(channel_id, chat_id) DO UPDATE SET
  type = excluded.type,
  label = CASE WHEN excluded.label = '' THEN telegram_channel_chats.label ELSE excluded.label END`,
		uuid.NewString(), channelID, userID, chatID, NormalizeTelegramChatType(kind),
		stringMin(strings.TrimSpace(label), 160), next, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) ListChannelChats(ctx context.Context, channelID string) ([]TelegramChannelChat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, channel_id, chat_id, type, label, created_at
FROM telegram_channel_chats WHERE channel_id = ? ORDER BY position, created_at, id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TelegramChannelChat
	for rows.Next() {
		var c TelegramChannelChat
		var created sql.NullString
		if err := rows.Scan(&c.ID, &c.ChannelID, &c.ChatID, &c.Type, &c.Label, &created); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RemoveChannelChat drops a destination unless an attachment points at it.
func (s *Store) RemoveChannelChat(ctx context.Context, userID, channelID, chatID string) error {
	var n int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM telegram_attachments WHERE channel_id = ? AND chat_id = ?`,
		channelID, strings.TrimSpace(chatID)).Scan(&n)
	if n > 0 {
		return fmt.Errorf("chat is the primary destination of %d attachment(s)", n)
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM telegram_channel_chats WHERE channel_id = ? AND chat_id = ? AND user_id = ?`,
		channelID, strings.TrimSpace(chatID), userID)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PrimaryChat returns the attachment's chat, or the channel's first chat.
func (c TelegramChannel) PrimaryChat(preferred string) string {
	if p := strings.TrimSpace(preferred); p != "" {
		for _, ch := range c.Chats {
			if ch.ChatID == p {
				return p
			}
		}
	}
	if len(c.Chats) > 0 {
		return c.Chats[0].ChatID
	}
	return ""
}

// --- attachments ---

// AttachChannel binds a consumer to a channel. Idempotent per
// (channel, consumer, consumer_id, direction).
func (s *Store) AttachChannel(ctx context.Context, userID string, a ChannelAttachment) error {
	if strings.TrimSpace(a.ChannelID) == "" || strings.TrimSpace(a.Consumer) == "" {
		return fmt.Errorf("channel and consumer required")
	}
	switch a.Direction {
	case DirectionSend, DirectionReceive:
	default:
		return fmt.Errorf("direction must be send or receive")
	}
	if _, err := s.TelegramChannelByID(ctx, userID, a.ChannelID); err != nil {
		return fmt.Errorf("unknown channel")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO telegram_attachments (id, user_id, channel_id, consumer, consumer_id, direction, chat_id, created_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(channel_id, consumer, consumer_id, direction) DO UPDATE SET chat_id = excluded.chat_id`,
		uuid.NewString(), userID, a.ChannelID, a.Consumer, strings.TrimSpace(a.ConsumerID),
		a.Direction, strings.TrimSpace(a.ChatID), time.Now().UTC().Format(time.RFC3339))
	if err != nil && strings.Contains(err.Error(), "idx_telegram_attachments_receive") {
		return fmt.Errorf("another consumer already receives on this channel (one getUpdates consumer per bot token)")
	}
	return err
}

// DetachChannel removes one binding.
func (s *Store) DetachChannel(ctx context.Context, userID, channelID, consumer, consumerID, direction string) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM telegram_attachments
WHERE user_id = ? AND channel_id = ? AND consumer = ? AND consumer_id = ? AND direction = ?`,
		userID, channelID, consumer, strings.TrimSpace(consumerID), direction)
	return err
}

// DetachConsumer removes every binding of a consumer (e.g. a deleted bot).
func (s *Store) DetachConsumer(ctx context.Context, userID, consumer, consumerID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM telegram_attachments WHERE user_id = ? AND consumer = ? AND consumer_id = ?`,
		userID, consumer, strings.TrimSpace(consumerID))
	return err
}

const attachmentSelect = `
SELECT a.id, a.user_id, a.channel_id, a.consumer, a.consumer_id, a.direction, a.chat_id,
       a.created_at, c.name
FROM telegram_attachments a JOIN telegram_channels c ON c.id = a.channel_id`

func scanAttachments(rows *sql.Rows) ([]ChannelAttachment, error) {
	defer rows.Close()
	var out []ChannelAttachment
	for rows.Next() {
		var a ChannelAttachment
		var created sql.NullString
		if err := rows.Scan(&a.ID, &a.UserID, &a.ChannelID, &a.Consumer, &a.ConsumerID,
			&a.Direction, &a.ChatID, &created, &a.ChannelName); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListChannelAttachments returns everything bound to one channel.
func (s *Store) ListChannelAttachments(ctx context.Context, userID, channelID string) ([]ChannelAttachment, error) {
	rows, err := s.db.QueryContext(ctx,
		attachmentSelect+` WHERE a.user_id = ? AND a.channel_id = ? ORDER BY a.direction, a.consumer`,
		userID, channelID)
	if err != nil {
		return nil, err
	}
	return scanAttachments(rows)
}

// ConsumerAttachments returns the channels a consumer is bound to.
// consumerID may be empty for singleton consumers (notifier, email).
func (s *Store) ConsumerAttachments(ctx context.Context, userID, consumer, consumerID, direction string) ([]ChannelAttachment, error) {
	q := attachmentSelect + ` WHERE a.user_id = ? AND a.consumer = ? AND a.consumer_id = ?`
	args := []any{userID, consumer, strings.TrimSpace(consumerID)}
	if d := strings.TrimSpace(direction); d != "" {
		q += ` AND a.direction = ?`
		args = append(args, d)
	}
	q += ` ORDER BY a.created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanAttachments(rows)
}

// ChannelForConsumer resolves the channel a consumer sends/receives on,
// falling back to the default channel when it has no attachment yet.
func (s *Store) ChannelForConsumer(ctx context.Context, userID, consumer, consumerID, direction string) (*TelegramChannel, string, error) {
	atts, err := s.ConsumerAttachments(ctx, userID, consumer, consumerID, direction)
	if err != nil {
		return nil, "", err
	}
	if len(atts) == 0 {
		c, err := s.DefaultTelegramChannel(ctx, userID)
		if err != nil {
			return nil, "", err
		}
		return c, c.PrimaryChat(""), nil
	}
	c, err := s.TelegramChannelByID(ctx, userID, atts[0].ChannelID)
	if err != nil {
		return nil, "", err
	}
	return c, c.PrimaryChat(atts[0].ChatID), nil
}

// ReceiverOf returns the consumer currently receiving on a channel, if any.
func (s *Store) ReceiverOf(ctx context.Context, userID, channelID string) (*ChannelAttachment, error) {
	rows, err := s.db.QueryContext(ctx,
		attachmentSelect+` WHERE a.user_id = ? AND a.channel_id = ? AND a.direction = ? LIMIT 1`,
		userID, channelID, DirectionReceive)
	if err != nil {
		return nil, err
	}
	list, err := scanAttachments(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}
