package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// --- bots module (Telegram assistant daemons running on machines) ---

// Chat approval states.
const (
	BotChatPending  = "pending"
	BotChatApproved = "approved"
	BotChatDenied   = "denied"
)

// Chat kinds. Telegram "supergroup"/"channel" are normalised to group.
const (
	BotChatPrivate = "private"
	BotChatGroup   = "group"
)

// Bot kinds. A legacy row is a placeholder for one of the old machine_ai_run
// enum owners: it can receive deliveries but has no daemon (and no token) yet.
const (
	BotKindDaemon = "daemon"
	BotKindLegacy = "legacy"
)

// LegacyBotOwners is the machine_ai_run owner enum that predates this module
// (the "Grok Bots"). Existing instances get one placeholder bot row per name so
// jobs launched with those owners keep resolving after the migration.
var LegacyBotOwners = []string{"Minerva", "Menta", "TPVLINE", "Gestor", "Hardware", "Games"}

// Bot is a Telegram assistant daemon instance managed by Takan. It is also the
// identity a machine AI job is attributed to (machine_ai_run owner).
// Its Telegram credential lives on the attached channel (sealed at rest); this
// row never holds one.
type Bot struct {
	ID          string
	UserID      string
	Name        string
	BotUsername string
	MachineID   string
	MachineName string
	// Kind is daemon (a real bot daemon) or legacy (seeded owner placeholder).
	Kind     string
	Version  string
	HasToken bool
	// Instance is the systemd unit base name on the target machine.
	Instance        string
	ProvisionStatus string // "" | queued | running | ok | failed
	ProvisionError  string
	ProvisionAt     *time.Time
	LastSeen        *time.Time
	CreatedAt       time.Time
	// Counters are filled by ListBots (not stored).
	PendingChats      int
	ApprovedChats     int
	PendingDeliveries int
}

// Legacy reports whether this is a seeded owner placeholder with no daemon yet.
func (b Bot) Legacy() bool { return b.Kind == BotKindLegacy }

// Provision job states.
const (
	ProvisionQueued  = "queued"
	ProvisionRunning = "running"
	ProvisionOK      = "ok"
	ProvisionFailed  = "failed"
)

// ProvisionTicketTTL bounds how long a provision run may fetch its manifest and
// binary from the hub.
const ProvisionTicketTTL = 10 * time.Minute

// Provisionable reports whether this row can be pushed to a machine. The
// telegram credential comes from its channel attachment, checked at dispatch.
func (b Bot) Provisionable() bool { return b.MachineName != "" }

// BotChat is one Telegram chat known to a bot, with its approval status.
type BotChat struct {
	ID           string
	BotID        string
	UserID       string
	ChatID       string
	Type         string
	Title        string
	Username     string
	Status       string
	FirstMessage string
	DecidedBy    string
	DecidedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Label is a short human name for the chat (group title or user name).
func (c BotChat) Label() string {
	if t := strings.TrimSpace(c.Title); t != "" {
		if u := strings.TrimSpace(c.Username); u != "" {
			return t + " (@" + strings.TrimPrefix(u, "@") + ")"
		}
		return t
	}
	if u := strings.TrimSpace(c.Username); u != "" {
		return "@" + strings.TrimPrefix(u, "@")
	}
	return c.ChatID
}

func (s *Store) migrateBots() error {
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS bots (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  bot_username TEXT NOT NULL DEFAULT '',
  machine_id TEXT REFERENCES machines(id) ON DELETE SET NULL,
  machine_name TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'daemon',
  version TEXT NOT NULL DEFAULT '',
  token_hash TEXT UNIQUE,
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_bots_user ON bots(user_id);

CREATE TABLE IF NOT EXISTS bot_chats (
  id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chat_id TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'private',
  title TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  first_message TEXT NOT NULL DEFAULT '',
  decided_by TEXT NOT NULL DEFAULT '',
  decided_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(bot_id, chat_id)
);
CREATE INDEX IF NOT EXISTS idx_bot_chats_bot ON bot_chats(bot_id, status);
CREATE INDEX IF NOT EXISTS idx_bot_chats_user ON bot_chats(user_id, status);
CREATE INDEX IF NOT EXISTS idx_bot_chats_updated ON bot_chats(bot_id, updated_at);

-- Hub -> bot outbox. A delivery stays here until the daemon acks it.
CREATE TABLE IF NOT EXISTS bot_deliveries (
  id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  dedupe_key TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  fetched_at TEXT,
  acked_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_bot_deliveries_open ON bot_deliveries(bot_id, acked_at, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bot_deliveries_dedupe
  ON bot_deliveries(bot_id, dedupe_key) WHERE dedupe_key <> '';

-- Machine AI jobs attributed to a bot, so the result can be routed back to the
-- Telegram chat that asked for it once the job finishes.
CREATE TABLE IF NOT EXISTS bot_jobs (
  job_id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chat_id TEXT NOT NULL DEFAULT '',
  machine TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bot_jobs_bot ON bot_jobs(bot_id, created_at);

-- Short-lived credential a provision run uses to fetch its manifest and the
-- daemon binary from the hub. Never reused after the run reports back.
CREATE TABLE IF NOT EXISTS bot_provision_tickets (
  id TEXT PRIMARY KEY,
  bot_id TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  machine TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bot_provision_tickets_bot ON bot_provision_tickets(bot_id);
`); err != nil {
		return err
	}
	if err := s.migrateBotProvisionCols(); err != nil {
		return err
	}
	return s.seedLegacyBotsAllUsers()
}

// migrateBotProvisionCols adds the channel link and provisioning state to bots
// created before zero-touch provisioning existed.
func (s *Store) migrateBotProvisionCols() error {
	cols, err := s.tableColumns("bots")
	if err != nil || len(cols) == 0 {
		return err
	}
	for _, a := range []struct{ col, ddl string }{
		{"instance", `ALTER TABLE bots ADD COLUMN instance TEXT NOT NULL DEFAULT ''`},
		{"provision_status", `ALTER TABLE bots ADD COLUMN provision_status TEXT NOT NULL DEFAULT ''`},
		{"provision_error", `ALTER TABLE bots ADD COLUMN provision_error TEXT NOT NULL DEFAULT ''`},
		{"provision_at", `ALTER TABLE bots ADD COLUMN provision_at TEXT`},
	} {
		if cols[a.col] {
			continue
		}
		if _, err := s.db.Exec(a.ddl); err != nil {
			return fmt.Errorf("migrate bots.%s: %w", a.col, err)
		}
	}
	// Bots created before provisioning existed have no unit name; derive one so
	// they can be provisioned without being recreated.
	rows, err := s.db.Query(`SELECT id, name FROM bots WHERE instance = ''`)
	if err != nil {
		return err
	}
	type row struct{ id, name string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
			return err
		}
		pending = append(pending, r)
	}
	rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range pending {
		inst := InstanceName(r.name)
		if inst == "" {
			continue
		}
		if _, err := s.db.Exec(`UPDATE bots SET instance = ? WHERE id = ?`, inst, r.id); err != nil {
			return fmt.Errorf("backfill bots.instance: %w", err)
		}
	}
	return nil
}

// tableColumns reports the column set of a table (empty when it does not exist).
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

// seedLegacyBotsAllUsers gives every existing account one placeholder bot per
// legacy machine_ai_run owner. Fresh installs have no users yet, so a brand new
// instance starts with an empty registry.
func (s *Store) seedLegacyBotsAllUsers() error {
	rows, err := s.db.Query(`SELECT id FROM users`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
			return err
		}
		ids = append(ids, id)
	}
	rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.SeedLegacyBots(context.Background(), id); err != nil {
			return err
		}
	}
	return nil
}

// SeedLegacyBots inserts a placeholder bot per LegacyBotOwners name. Idempotent:
// a name that already exists (as a real daemon or a placeholder) is left alone.
func (s *Store) SeedLegacyBots(ctx context.Context, userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, name := range LegacyBotOwners {
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO bots (id, user_id, name, kind, created_at)
SELECT ?,?,?,?,?
WHERE NOT EXISTS (SELECT 1 FROM bots WHERE user_id = ? AND name = ?)`,
			uuid.NewString(), userID, name, BotKindLegacy, now, userID, name); err != nil {
			return fmt.Errorf("seed legacy bot %s: %w", name, err)
		}
	}
	return nil
}

// NormalizeBotChatType maps Telegram chat types onto private|group.
func NormalizeBotChatType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "group", "supergroup", "channel":
		return BotChatGroup
	default:
		return BotChatPrivate
	}
}

// InstanceName derives the systemd unit base name from a bot name.
func InstanceName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return stringMin(out, 48)
}

// CreateBot returns (bot, rawToken, error). The raw token is shown once.
// machineID (optional) is the provisioning target; the telegram credential is
// bound separately with AttachChannel (consumer=bot, direction=receive).
func (s *Store) CreateBot(ctx context.Context, userID, name, machineID string) (*Bot, string, error) {
	name = normalizeName(name)
	if name == "" {
		return nil, "", fmt.Errorf("name required")
	}
	instance := InstanceName(name)
	if instance == "" {
		return nil, "", fmt.Errorf("name must contain letters or digits")
	}
	machineName := ""
	machineID = strings.TrimSpace(machineID)
	if machineID != "" {
		mac, err := s.MachineByID(ctx, userID, machineID)
		if err != nil {
			return nil, "", fmt.Errorf("unknown machine")
		}
		machineName = mac.Name
	}
	raw, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	var machineCol any
	if machineID != "" {
		machineCol = machineID
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO bots (id, user_id, name, machine_id, machine_name, kind, token_hash, instance, created_at)
VALUES (?,?,?,?,?,?,?,?,?)`,
		id, userID, name, machineCol, machineName, BotKindDaemon, hashToken(raw),
		instance, now.Format(time.RFC3339))
	if err != nil {
		return nil, "", fmt.Errorf("create bot: %w", err)
	}
	return &Bot{
		ID: id, UserID: userID, Name: name, MachineID: machineID, MachineName: machineName,
		Kind: BotKindDaemon, HasToken: true, Instance: instance, CreatedAt: now,
	}, raw, nil
}

// SetBotProvisionState records progress of a provision run.
func (s *Store) SetBotProvisionState(ctx context.Context, botID, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE bots SET provision_status = ?, provision_error = ?, provision_at = ? WHERE id = ?`,
		status, stringMin(strings.TrimSpace(errMsg), 500),
		time.Now().UTC().Format(time.RFC3339), botID)
	return err
}

// SetBotTarget updates the provisioning target machine of an existing bot.
func (s *Store) SetBotTarget(ctx context.Context, userID, id, machineID string) error {
	machineID = strings.TrimSpace(machineID)
	var machineCol any
	machineName := ""
	if machineID != "" {
		mac, err := s.MachineByID(ctx, userID, machineID)
		if err != nil {
			return fmt.Errorf("unknown machine")
		}
		machineCol, machineName = machineID, mac.Name
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE bots SET machine_id = ?, machine_name = ? WHERE id = ? AND user_id = ?`,
		machineCol, machineName, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return sql.ErrNoRows
	}
	return nil
}

// --- provision tickets ---

// IssueProvisionTicket mints the short-lived credential the provision script
// uses to pull its manifest and the daemon binary. Previous tickets for the bot
// are dropped, so only the newest run can fetch secrets.
func (s *Store) IssueProvisionTicket(ctx context.Context, botID, userID, machine string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM bot_provision_tickets WHERE bot_id = ?`, botID); err != nil {
		return "", err
	}
	raw, err := randomHex(24)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO bot_provision_tickets (id, bot_id, user_id, token_hash, machine, created_at, expires_at)
VALUES (?,?,?,?,?,?,?)`,
		uuid.NewString(), botID, userID, hashToken(raw), strings.TrimSpace(machine),
		now.Format(time.RFC3339), now.Add(ProvisionTicketTTL).Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return raw, nil
}

// BotByProvisionTicket resolves an unexpired ticket to its bot.
func (s *Store) BotByProvisionTicket(ctx context.Context, raw string) (*Bot, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, sql.ErrNoRows
	}
	var botID, expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT bot_id, expires_at FROM bot_provision_tickets WHERE token_hash = ?`,
		hashToken(raw)).Scan(&botID, &expires)
	if err != nil {
		return nil, err
	}
	exp, perr := time.Parse(time.RFC3339, expires)
	if perr != nil || time.Now().UTC().After(exp) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM bot_provision_tickets WHERE token_hash = ?`, hashToken(raw)) // safe-ignore: best-effort fixup; the next call re-derives the state
		return nil, sql.ErrNoRows
	}
	return scanBot(s.db.QueryRowContext(ctx, botSelect+` WHERE b.id = ?`, botID))
}

// RevokeProvisionTickets invalidates every ticket of a bot (end of a run).
func (s *Store) RevokeProvisionTickets(ctx context.Context, botID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bot_provision_tickets WHERE bot_id = ?`, botID)
	return err
}

// PurgeProvisionTickets drops expired rows.
func (s *Store) PurgeProvisionTickets(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bot_provision_tickets WHERE expires_at < ?`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
	return int(n), nil
}

// IssueBotToken (re)issues the daemon token for a bot and promotes a legacy
// placeholder to a real daemon. The previous token stops working immediately.
func (s *Store) IssueBotToken(ctx context.Context, userID, id string) (string, error) {
	raw, err := randomHex(24)
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE bots SET token_hash = ?, kind = ? WHERE id = ? AND user_id = ?`,
		hashToken(raw), BotKindDaemon, id, userID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return "", sql.ErrNoRows
	}
	return raw, nil
}

const botSelect = `
SELECT b.id, b.user_id, b.name, b.bot_username, COALESCE(b.machine_id, ''),
       COALESCE(m.name, b.machine_name), b.kind, b.version,
       b.token_hash IS NOT NULL, b.instance,
       b.provision_status, b.provision_error, b.provision_at,
       b.last_seen_at, b.created_at
FROM bots b LEFT JOIN machines m ON m.id = b.machine_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanBot(row rowScanner) (*Bot, error) {
	var b Bot
	var last, created, provAt sql.NullString
	err := row.Scan(&b.ID, &b.UserID, &b.Name, &b.BotUsername, &b.MachineID, &b.MachineName,
		&b.Kind, &b.Version, &b.HasToken, &b.Instance,
		&b.ProvisionStatus, &b.ProvisionError, &provAt, &last, &created)
	if err != nil {
		return nil, err
	}
	if provAt.Valid && provAt.String != "" {
		t, _ := time.Parse(time.RFC3339, provAt.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
		b.ProvisionAt = &t
	}
	if last.Valid && last.String != "" {
		t, _ := time.Parse(time.RFC3339, last.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
		b.LastSeen = &t
	}
	b.CreatedAt, _ = time.Parse(time.RFC3339, created.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
	return &b, nil
}

// BotByToken resolves a bot agent token (used by the daemon HTTP API).
func (s *Store) BotByToken(ctx context.Context, raw string) (*Bot, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, sql.ErrNoRows
	}
	return scanBot(s.db.QueryRowContext(ctx, botSelect+` WHERE b.token_hash = ?`, hashToken(raw)))
}

func (s *Store) BotByID(ctx context.Context, userID, id string) (*Bot, error) {
	return scanBot(s.db.QueryRowContext(ctx, botSelect+` WHERE b.user_id = ? AND b.id = ?`, userID, id))
}

// BotByUserAndName resolves a bot by its configurable name (case-insensitive).
func (s *Store) BotByUserAndName(ctx context.Context, userID, name string) (*Bot, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, sql.ErrNoRows
	}
	return scanBot(s.db.QueryRowContext(ctx,
		botSelect+` WHERE b.user_id = ? AND lower(b.name) = lower(?)`, userID, name))
}

// ListBots returns every bot with pending/approved chat counters.
func (s *Store) ListBots(ctx context.Context, userID string) ([]Bot, error) {
	rows, err := s.db.QueryContext(ctx, botSelect+` WHERE b.user_id = ? ORDER BY lower(b.name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].PendingChats, _ = s.CountBotChats(ctx, out[i].ID, BotChatPending)   // safe-ignore: counters are display-only and default to zero
		out[i].ApprovedChats, _ = s.CountBotChats(ctx, out[i].ID, BotChatApproved) // safe-ignore: counters are display-only and default to zero
		out[i].PendingDeliveries, _ = s.CountBotDeliveries(ctx, out[i].ID)         // safe-ignore: counters are display-only and default to zero
	}
	return out, nil
}

func (s *Store) CountBots(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM bots WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func (s *Store) CountBotChats(ctx context.Context, botID, status string) (int, error) {
	var n int
	var err error
	if strings.TrimSpace(status) == "" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM bot_chats WHERE bot_id = ?`, botID).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM bot_chats WHERE bot_id = ? AND status = ?`, botID, status).Scan(&n)
	}
	return n, err
}

// CountBotChatsPending counts pending approvals across all of a user's bots.
func (s *Store) CountBotChatsPending(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM bot_chats WHERE user_id = ? AND status = ?`, userID, BotChatPending).Scan(&n)
	return n, err
}

// UpdateBotIdentity is the idempotent registration write: it refreshes what the
// daemon announces (telegram username, machine, version) and touches last_seen.
// Empty fields are left untouched. Renaming a bot is a panel action, not a daemon one.
func (s *Store) UpdateBotIdentity(ctx context.Context, botID, botUsername, machineName, version string) error {
	botUsername = strings.TrimPrefix(strings.TrimSpace(botUsername), "@")
	machineName = strings.TrimSpace(machineName)
	version = strings.TrimSpace(version)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE bots SET
  bot_username = CASE WHEN ? = '' THEN bot_username ELSE ? END,
  machine_name = CASE WHEN ? = '' THEN machine_name ELSE ? END,
  machine_id = CASE
    WHEN ? = '' THEN machine_id
    ELSE COALESCE((SELECT id FROM machines WHERE user_id = bots.user_id AND name = ?), machine_id)
  END,
  version = CASE WHEN ? = '' THEN version ELSE ? END,
  last_seen_at = ?
WHERE id = ?`,
		botUsername, botUsername, machineName, machineName, machineName, machineName,
		version, version, now, botID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return sql.ErrNoRows
	}
	return nil
}

// TouchBot records a heartbeat.
func (s *Store) TouchBot(ctx context.Context, botID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE bots SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), botID)
	return err
}

// RenameBot changes the panel-visible name.
func (s *Store) RenameBot(ctx context.Context, userID, id, name string) error {
	name = normalizeName(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE bots SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteBot(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bots WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return sql.ErrNoRows
	}
	return nil
}

// --- chats ---

const botChatSelect = `
SELECT id, bot_id, user_id, chat_id, type, title, username, status, first_message,
       decided_by, decided_at, created_at, updated_at
FROM bot_chats`

func scanBotChat(row rowScanner) (*BotChat, error) {
	var c BotChat
	var decided, created, updated sql.NullString
	err := row.Scan(&c.ID, &c.BotID, &c.UserID, &c.ChatID, &c.Type, &c.Title, &c.Username,
		&c.Status, &c.FirstMessage, &c.DecidedBy, &decided, &created, &updated)
	if err != nil {
		return nil, err
	}
	if decided.Valid && decided.String != "" {
		t, _ := time.Parse(time.RFC3339, decided.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
		c.DecidedAt = &t
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, created.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
	c.UpdatedAt, _ = time.Parse(time.RFC3339, updated.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
	return &c, nil
}

// ReportBotChat records an unknown chat that wrote to the bot. Idempotent per
// (bot, chat_id): a chat that already exists is only refreshed with display
// info, never reset to pending. Returns the row and whether it was created.
func (s *Store) ReportBotChat(ctx context.Context, botID, userID string, in BotChat) (*BotChat, bool, error) {
	chatID := strings.TrimSpace(in.ChatID)
	if chatID == "" {
		return nil, false, fmt.Errorf("chat_id required")
	}
	typ := NormalizeBotChatType(in.Type)
	title := stringMin(strings.TrimSpace(in.Title), 200)
	username := stringMin(strings.TrimPrefix(strings.TrimSpace(in.Username), "@"), 100)
	snippet := stringMin(strings.TrimSpace(in.FirstMessage), 500)
	now := time.Now().UTC().Format(time.RFC3339)

	// INSERT … WHERE NOT EXISTS tells creation apart from a repeat report without
	// relying on timestamps, so the operator is notified exactly once per chat.
	res, err := s.db.ExecContext(ctx, `
INSERT INTO bot_chats (id, bot_id, user_id, chat_id, type, title, username, status, first_message, created_at, updated_at)
SELECT ?,?,?,?,?,?,?,?,?,?,?
WHERE NOT EXISTS (SELECT 1 FROM bot_chats WHERE bot_id = ? AND chat_id = ?)`,
		uuid.NewString(), botID, userID, chatID, typ, title, username, BotChatPending, snippet, now, now,
		botID, chatID)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
	created := n > 0
	if !created {
		// Refresh display info only; never resurrect a decided chat as pending.
		if _, err := s.db.ExecContext(ctx, `
UPDATE bot_chats SET
  type = ?,
  title = CASE WHEN ? = '' THEN title ELSE ? END,
  username = CASE WHEN ? = '' THEN username ELSE ? END,
  first_message = CASE WHEN first_message = '' THEN ? ELSE first_message END
WHERE bot_id = ? AND chat_id = ?`,
			typ, title, title, username, username, snippet, botID, chatID); err != nil {
			return nil, false, err
		}
	}
	c, err := s.BotChat(ctx, botID, chatID)
	if err != nil {
		return nil, false, err
	}
	return c, created, nil
}

// BotChat returns one chat of a bot by telegram chat id.
func (s *Store) BotChat(ctx context.Context, botID, chatID string) (*BotChat, error) {
	return scanBotChat(s.db.QueryRowContext(ctx,
		botChatSelect+` WHERE bot_id = ? AND chat_id = ?`, botID, strings.TrimSpace(chatID)))
}

// ListBotChats returns a bot's chats. status filters (empty = all); updatedSince
// (RFC3339, empty = no filter) returns only rows changed after that instant, which
// is what the daemon uses to refresh its local cache.
func (s *Store) ListBotChats(ctx context.Context, botID, status, updatedSince string) ([]BotChat, error) {
	q := botChatSelect + ` WHERE bot_id = ?`
	args := []any{botID}
	if st := strings.TrimSpace(status); st != "" {
		q += ` AND status = ?`
		args = append(args, st)
	}
	if us := strings.TrimSpace(updatedSince); us != "" {
		q += ` AND updated_at > ?`
		args = append(args, us)
	}
	q += ` ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END, updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotChat
	for rows.Next() {
		c, err := scanBotChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListPendingBotChats returns pending approvals across all of a user's bots,
// newest first, joined with the bot name.
func (s *Store) ListPendingBotChats(ctx context.Context, userID string) ([]BotChat, map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		botChatSelect+` WHERE user_id = ? AND status = ? ORDER BY created_at DESC`, userID, BotChatPending)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []BotChat
	for rows.Next() {
		c, err := scanBotChat(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	names := map[string]string{}
	bots, err := s.ListBots(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range bots {
		names[b.ID] = b.Name
	}
	return out, names, nil
}

// DecideBotChat approves or denies a chat. Idempotent; decidedBy is free text
// ("panel", "mcp", …). Returns the updated row.
func (s *Store) DecideBotChat(ctx context.Context, userID, botID, chatID, status, decidedBy string) (*BotChat, error) {
	switch status {
	case BotChatApproved, BotChatDenied, BotChatPending:
	default:
		return nil, fmt.Errorf("status must be approved, denied or pending")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE bot_chats SET status = ?, decided_by = ?, decided_at = ?, updated_at = ?
WHERE bot_id = ? AND chat_id = ? AND user_id = ?`,
		status, strings.TrimSpace(decidedBy), now, now, botID, strings.TrimSpace(chatID), userID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return nil, sql.ErrNoRows
	}
	return s.BotChat(ctx, botID, chatID)
}

// DeleteBotChat forgets a chat entirely (it becomes unknown again).
func (s *Store) DeleteBotChat(ctx context.Context, userID, botID, chatID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM bot_chats WHERE bot_id = ? AND chat_id = ? AND user_id = ?`,
		botID, strings.TrimSpace(chatID), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		return sql.ErrNoRows
	}
	return nil
}

// BotChatsUpdatedAt is the newest updated_at across a bot's chats (empty when none).
// The daemon uses it as a cheap cache validator.
func (s *Store) BotChatsUpdatedAt(ctx context.Context, botID string) (string, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM bot_chats WHERE bot_id = ?`, botID).Scan(&v)
	if err != nil {
		return "", err
	}
	if !v.Valid {
		return "", nil
	}
	return v.String, nil
}

// --- hub -> bot outbox (deliveries) ---

// Delivery types.
const BotDeliveryAIJobResult = "ai_job_result"

// DeliveryRetryAfter is how long a fetched-but-unacked delivery stays invisible
// before it is handed out again (at-least-once with a visibility timeout).
const DeliveryRetryAfter = 30 * time.Second

// BotDelivery is one queued hub -> bot message.
type BotDelivery struct {
	ID        string
	BotID     string
	UserID    string
	Type      string
	Payload   string // raw JSON object
	Attempts  int
	CreatedAt time.Time
}

// EnqueueBotDelivery queues a message for a bot daemon. dedupeKey (optional)
// makes the enqueue idempotent: a second call with the same key is a no-op
// while the first delivery is still queued. Reports whether a row was created.
func (s *Store) EnqueueBotDelivery(ctx context.Context, botID, userID, typ, dedupeKey string, payload []byte) (bool, error) {
	typ = strings.TrimSpace(typ)
	if botID == "" || typ == "" {
		return false, fmt.Errorf("bot and type required")
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	dedupeKey = strings.TrimSpace(dedupeKey)
	if dedupeKey == "" {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO bot_deliveries (id, bot_id, user_id, type, dedupe_key, payload, created_at)
VALUES (?,?,?,?,'',?,?)`, uuid.NewString(), botID, userID, typ, string(payload), now)
		return err == nil, err
	}
	// Dedupe only against still-queued rows: a job re-run with the same id after
	// the first result was acked must be delivered again.
	res, err := s.db.ExecContext(ctx, `
INSERT INTO bot_deliveries (id, bot_id, user_id, type, dedupe_key, payload, created_at)
SELECT ?,?,?,?,?,?,?
WHERE NOT EXISTS (
  SELECT 1 FROM bot_deliveries WHERE bot_id = ? AND dedupe_key = ? AND acked_at IS NULL
)`, uuid.NewString(), botID, userID, typ, dedupeKey, string(payload), now, botID, dedupeKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
	return n > 0, nil
}

// ListBotDeliveries hands out queued deliveries oldest first and marks them
// fetched. Rows handed out less than DeliveryRetryAfter ago are skipped so two
// overlapping polls do not process the same delivery twice.
func (s *Store) ListBotDeliveries(ctx context.Context, botID string, limit int) ([]BotDelivery, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	cutoff := time.Now().UTC().Add(-DeliveryRetryAfter).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, bot_id, user_id, type, payload, attempts, created_at
FROM bot_deliveries
WHERE bot_id = ? AND acked_at IS NULL AND (fetched_at IS NULL OR fetched_at <= ?)
ORDER BY created_at, id
LIMIT ?`, botID, cutoff, limit)
	if err != nil {
		return nil, err
	}
	var out []BotDelivery
	for rows.Next() {
		var d BotDelivery
		var created sql.NullString
		if err := rows.Scan(&d.ID, &d.BotID, &d.UserID, &d.Type, &d.Payload, &d.Attempts, &created); err != nil {
			rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339, created.String) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
		out = append(out, d)
	}
	rows.Close() // safe-ignore: rows already drained; close failure cannot change the result
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range out {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE bot_deliveries SET fetched_at = ?, attempts = attempts + 1 WHERE id = ?`,
			now, out[i].ID); err != nil {
			return nil, err
		}
		out[i].Attempts++
	}
	return out, nil
}

// AckBotDeliveries marks deliveries done. Unknown ids are ignored (idempotent).
func (s *Store) AckBotDeliveries(ctx context.Context, botID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	total := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE bot_deliveries SET acked_at = ? WHERE id = ? AND bot_id = ? AND acked_at IS NULL`,
			now, id, botID)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
		total += int(n)
	}
	return total, nil
}

// CountBotDeliveries counts queued (unacked) deliveries for a bot.
func (s *Store) CountBotDeliveries(ctx context.Context, botID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM bot_deliveries WHERE bot_id = ? AND acked_at IS NULL`, botID).Scan(&n)
	return n, err
}

// PurgeAckedBotDeliveries drops acked rows older than the given age.
func (s *Store) PurgeAckedBotDeliveries(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM bot_deliveries WHERE acked_at IS NOT NULL AND acked_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
	return int(n), nil
}

// --- machine AI jobs attributed to a bot ---

// BotJob links a machine AI job to the bot (and Telegram chat) that asked for it.
type BotJob struct {
	JobID   string
	BotID   string
	UserID  string
	ChatID  string
	Machine string
}

// RecordBotJob remembers which bot launched a job, so its result can be routed
// back when the job finishes. Idempotent per job id.
func (s *Store) RecordBotJob(ctx context.Context, jobID, botID, userID, chatID, machine string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || botID == "" {
		return fmt.Errorf("job_id and bot required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO bot_jobs (job_id, bot_id, user_id, chat_id, machine, created_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(job_id) DO UPDATE SET
  bot_id = excluded.bot_id,
  chat_id = CASE WHEN excluded.chat_id = '' THEN bot_jobs.chat_id ELSE excluded.chat_id END,
  machine = CASE WHEN excluded.machine = '' THEN bot_jobs.machine ELSE excluded.machine END`,
		jobID, botID, userID, strings.TrimSpace(chatID), strings.TrimSpace(machine),
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// BotJobByID returns the bot attribution for a job, or sql.ErrNoRows.
func (s *Store) BotJobByID(ctx context.Context, jobID string) (*BotJob, error) {
	var j BotJob
	err := s.db.QueryRowContext(ctx,
		`SELECT job_id, bot_id, user_id, chat_id, machine FROM bot_jobs WHERE job_id = ?`,
		strings.TrimSpace(jobID)).Scan(&j.JobID, &j.BotID, &j.UserID, &j.ChatID, &j.Machine)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// PurgeBotJobs drops job attributions older than the given age.
func (s *Store) PurgeBotJobs(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM bot_jobs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected() // safe-ignore: SQLite always reports it; a zero here is handled as not-found
	return int(n), nil
}
