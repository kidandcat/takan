package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AssistantHistoryCap is how many conversation messages stay in the database.
// Older ones are dropped on append. It is deliberately generous: the app pages
// backwards through this log, so a small cap would silently cut off history the
// user can still see in Telegram.
const AssistantHistoryCap = 5000

// Assistant meta keys.
const (
	MetaTelegramOffset = "telegram_offset"
	MetaBotTokenEnc    = "bot_token_enc"
	MetaBotUsername    = "bot_username"
)

// AssistantChat tracks one Telegram chat's agent conversation.
type AssistantChat struct {
	ChatID string
	Kind   string
	Title  string
	// SessionID is the agent session this chat currently resumes.
	SessionID string
	// ForkFrom is set when the previous turn was promoted to a background task
	// and still owns SessionID.
	ForkFrom string
	// ConversationStarted is false right after /new, so the next run starts a
	// fresh session instead of resuming one.
	ConversationStarted bool
	Runs                int64
	LastRunAt           *time.Time
}

// AssistantMessage is one persisted turn of the shared conversation the phone
// app pages.
type AssistantMessage struct {
	Seq       int64
	ID        string
	Role      string
	Text      string
	FilesJSON string
	Source    string
	CreatedAt time.Time
}

// AssistantJob is one reminder (message) or routine (agent run).
type AssistantJob struct {
	ID         string
	Type       string
	Name       string
	Payload    string
	Cron       string
	At         *time.Time
	ChatID     string
	NextRun    *time.Time
	LastRun    *time.Time
	LastStatus string
	Runs       int64
	CreatedAt  time.Time
}

// AssistantTask is one long-running agent run detached from the conversation.
type AssistantTask struct {
	ID         string
	Title      string
	Prompt     string
	ChatID     string
	State      string
	PID        int
	SessionID  string
	Dir        string
	OutputPath string
	Promoted   bool
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

// PushDevice is one phone allowed to receive notifications.
type PushDevice struct {
	Token     string
	Platform  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// JobChat records where a machine_ai_run result must be delivered. It replaces
// the bot outbox: one row per launched job, cleared when the result is sent.
type JobChat struct {
	JobID       string
	UserID      string
	ChatID      string
	Machine     string
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

// migrateAssistant creates the tables the in-process assistant owns.
func (s *Store) migrateAssistant() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS assistant_meta (
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  value   TEXT NOT NULL,
  PRIMARY KEY (user_id, key)
);

CREATE TABLE IF NOT EXISTS assistant_chats (
  user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chat_id  TEXT NOT NULL,
  kind     TEXT NOT NULL DEFAULT 'private',
  title    TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  fork_from  TEXT NOT NULL DEFAULT '',
  conversation_started INTEGER NOT NULL DEFAULT 0,
  runs     INTEGER NOT NULL DEFAULT 0,
  last_run_at TEXT,
  PRIMARY KEY (user_id, chat_id)
);

CREATE TABLE IF NOT EXISTS assistant_messages (
  seq  INTEGER PRIMARY KEY AUTOINCREMENT,
  id   TEXT NOT NULL UNIQUE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  files TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_user ON assistant_messages(user_id, seq);

CREATE TABLE IF NOT EXISTS assistant_jobs (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL,
  cron TEXT NOT NULL DEFAULT '',
  at TEXT,
  chat_id TEXT NOT NULL DEFAULT '',
  next_run TEXT, last_run TEXT,
  last_status TEXT NOT NULL DEFAULT '',
  runs INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_jobs_user ON assistant_jobs(user_id);

CREATE TABLE IF NOT EXISTS assistant_tasks (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title TEXT NOT NULL DEFAULT '',
  prompt TEXT NOT NULL,
  chat_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  pid INTEGER NOT NULL DEFAULT 0,
  session_id TEXT NOT NULL DEFAULT '',
  dir TEXT NOT NULL DEFAULT '',
  output_path TEXT NOT NULL DEFAULT '',
  promoted INTEGER NOT NULL DEFAULT 0,
  started_at TEXT NOT NULL, finished_at TEXT,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_assistant_tasks_user ON assistant_tasks(user_id, started_at);

CREATE TABLE IF NOT EXISTS push_devices (
  token TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  platform TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_push_devices_user ON push_devices(user_id);

CREATE TABLE IF NOT EXISTS job_chats (
  job_id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chat_id TEXT NOT NULL DEFAULT '',
  machine TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_job_chats_pending ON job_chats(user_id, delivered_at, created_at);
`)
	return err
}

// --- meta ---

// AssistantMeta reads one key, returning "" when it is not set.
func (s *Store) AssistantMeta(ctx context.Context, userID, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM assistant_meta WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetAssistantMeta upserts one key.
func (s *Store) SetAssistantMeta(ctx context.Context, userID, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_meta (user_id, key, value) VALUES (?,?,?)
ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, userID, key, value)
	return err
}

// --- chats ---

// AssistantChatState returns a chat's state, or a zero value when unknown.
func (s *Store) AssistantChatState(ctx context.Context, userID, chatID string) (AssistantChat, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT chat_id, kind, title, session_id, fork_from, conversation_started, runs, last_run_at
FROM assistant_chats WHERE user_id = ? AND chat_id = ?`, userID, chatID)
	c, err := scanAssistantChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AssistantChat{ChatID: chatID}, nil
	}
	return c, err
}

// ListAssistantChats returns every chat the assistant knows, busiest first.
func (s *Store) ListAssistantChats(ctx context.Context, userID string) ([]AssistantChat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT chat_id, kind, title, session_id, fork_from, conversation_started, runs, last_run_at
FROM assistant_chats WHERE user_id = ? ORDER BY COALESCE(last_run_at,'') DESC, chat_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantChat
	for rows.Next() {
		c, err := scanAssistantChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAssistantChat(row rowScanner) (AssistantChat, error) {
	var c AssistantChat
	var started int
	var last sql.NullString
	if err := row.Scan(&c.ChatID, &c.Kind, &c.Title, &c.SessionID, &c.ForkFrom, &started, &c.Runs, &last); err != nil {
		return AssistantChat{}, err
	}
	c.ConversationStarted = started != 0
	if last.Valid && last.String != "" {
		if t, err := time.Parse(time.RFC3339, last.String); err == nil {
			c.LastRunAt = &t
		}
	}
	return c, nil
}

// SeeAssistantChat records a chat the moment the assistant first serves it, so
// the panel can list it. Existing rows keep their session state.
func (s *Store) SeeAssistantChat(ctx context.Context, userID, chatID, kind, title string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_chats (user_id, chat_id, kind, title) VALUES (?,?,?,?)
ON CONFLICT(user_id, chat_id) DO UPDATE SET
  kind = CASE WHEN excluded.kind != '' THEN excluded.kind ELSE assistant_chats.kind END,
  title = CASE WHEN excluded.title != '' THEN excluded.title ELSE assistant_chats.title END`,
		userID, chatID, kind, title)
	return err
}

// MarkConversationStarted records that the chat now resumes sessionID.
func (s *Store) MarkConversationStarted(ctx context.Context, userID, chatID, sessionID string) error {
	return s.setChatSession(ctx, userID, chatID, sessionID, "", true, true)
}

// MarkConversationPromoted records that sessionID now belongs to a background
// task, so the next conversational turn forks from it rather than resuming it.
func (s *Store) MarkConversationPromoted(ctx context.Context, userID, chatID, sessionID string) error {
	return s.setChatSession(ctx, userID, chatID, sessionID, sessionID, true, true)
}

// ResetAssistantConversation rotates the session: the next run starts fresh.
func (s *Store) ResetAssistantConversation(ctx context.Context, userID, chatID string) error {
	return s.setChatSession(ctx, userID, chatID, "", "", false, false)
}

func (s *Store) setChatSession(ctx context.Context, userID, chatID, sessionID, forkFrom string, started, countRun bool) error {
	st := 0
	if started {
		st = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if !countRun {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_chats (user_id, chat_id, session_id, fork_from, conversation_started)
VALUES (?,?,?,?,?)
ON CONFLICT(user_id, chat_id) DO UPDATE SET
  session_id = excluded.session_id,
  fork_from = excluded.fork_from,
  conversation_started = excluded.conversation_started`,
			userID, chatID, sessionID, forkFrom, st)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_chats (user_id, chat_id, session_id, fork_from, conversation_started, runs, last_run_at)
VALUES (?,?,?,?,?,1,?)
ON CONFLICT(user_id, chat_id) DO UPDATE SET
  session_id = excluded.session_id,
  fork_from = excluded.fork_from,
  conversation_started = excluded.conversation_started,
  runs = assistant_chats.runs + 1,
  last_run_at = excluded.last_run_at`,
		userID, chatID, sessionID, forkFrom, st, now)
	return err
}

// ForgetAssistantChat removes a chat's session state from the panel.
func (s *Store) ForgetAssistantChat(ctx context.Context, userID, chatID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM assistant_chats WHERE user_id = ? AND chat_id = ?`, userID, chatID)
	return err
}

// --- conversation history ---

// AppendAssistantMessage stores one turn and trims the log to the cap.
func (s *Store) AppendAssistantMessage(ctx context.Context, userID string, m AssistantMessage) error {
	if m.FilesJSON == "" {
		m.FilesJSON = "[]"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO assistant_messages (id, user_id, role, text, files, source, created_at)
VALUES (?,?,?,?,?,?,?)`,
		m.ID, userID, m.Role, m.Text, m.FilesJSON, m.Source, m.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
DELETE FROM assistant_messages WHERE user_id = ? AND seq NOT IN (
  SELECT seq FROM assistant_messages WHERE user_id = ? ORDER BY seq DESC LIMIT ?
)`, userID, userID, AssistantHistoryCap)
	return err
}

// ListAssistantMessages returns a window of the conversation, always oldest
// first.
//
//   - after != "": the messages that follow that id, for catching up forwards.
//   - before != "": the messages that precede it, for scrolling back through
//     older history.
//   - neither: the newest `limit` messages.
func (s *Store) ListAssistantMessages(ctx context.Context, userID, after, before string, limit int) ([]AssistantMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	const cols = `SELECT seq, id, role, text, files, source, created_at`
	var rows *sql.Rows
	var err error
	switch {
	case after != "":
		rows, err = s.db.QueryContext(ctx, cols+` FROM assistant_messages
WHERE user_id = ? AND seq > COALESCE((SELECT seq FROM assistant_messages WHERE id = ?), 0)
ORDER BY seq ASC LIMIT ?`, userID, after, limit)
	case before != "":
		// Take the newest rows below the cursor, then flip them back to
		// chronological order so the caller always sees oldest first.
		rows, err = s.db.QueryContext(ctx, cols+` FROM (`+cols+` FROM assistant_messages
  WHERE user_id = ? AND seq < COALESCE((SELECT seq FROM assistant_messages WHERE id = ?), 9223372036854775807)
  ORDER BY seq DESC LIMIT ?
) ORDER BY seq ASC`, userID, before, limit)
	default:
		rows, err = s.db.QueryContext(ctx, cols+` FROM (`+cols+` FROM assistant_messages
  WHERE user_id = ? ORDER BY seq DESC LIMIT ?
) ORDER BY seq ASC`, userID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantMessage
	for rows.Next() {
		var m AssistantMessage
		var created string
		if err := rows.Scan(&m.Seq, &m.ID, &m.Role, &m.Text, &m.FilesJSON, &m.Source, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseStoredTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountAssistantMessages reports how many turns are stored for a user.
func (s *Store) CountAssistantMessages(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM assistant_messages WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// --- scheduled jobs ---

// ListAssistantJobs returns every reminder and routine for a user.
func (s *Store) ListAssistantJobs(ctx context.Context, userID string) ([]AssistantJob, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, type, name, payload, cron, at, chat_id, next_run, last_run, last_status, runs, created_at
FROM assistant_jobs WHERE user_id = ? ORDER BY COALESCE(next_run,'') ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantJob
	for rows.Next() {
		var j AssistantJob
		var at, next, last sql.NullString
		var created string
		if err := rows.Scan(&j.ID, &j.Type, &j.Name, &j.Payload, &j.Cron, &at, &j.ChatID,
			&next, &last, &j.LastStatus, &j.Runs, &created); err != nil {
			return nil, err
		}
		j.At = optionalTime(at)
		j.NextRun = optionalTime(next)
		j.LastRun = optionalTime(last)
		j.CreatedAt = parseStoredTime(created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// SaveAssistantJob upserts one job.
func (s *Store) SaveAssistantJob(ctx context.Context, userID string, j AssistantJob) error {
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_jobs (id, user_id, type, name, payload, cron, at, chat_id, next_run, last_run, last_status, runs, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  type = excluded.type, name = excluded.name, payload = excluded.payload,
  cron = excluded.cron, at = excluded.at, chat_id = excluded.chat_id,
  next_run = excluded.next_run, last_run = excluded.last_run,
  last_status = excluded.last_status, runs = excluded.runs`,
		j.ID, userID, j.Type, j.Name, j.Payload, j.Cron, storeTime(j.At), j.ChatID,
		storeTime(j.NextRun), storeTime(j.LastRun), j.LastStatus, j.Runs,
		j.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

// DeleteAssistantJob removes one job.
func (s *Store) DeleteAssistantJob(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_jobs WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// --- background tasks ---

// ListAssistantTasks returns every task, newest first.
func (s *Store) ListAssistantTasks(ctx context.Context, userID string) ([]AssistantTask, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, title, prompt, chat_id, state, pid, session_id, dir, output_path, promoted, started_at, finished_at, error
FROM assistant_tasks WHERE user_id = ? ORDER BY started_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantTask
	for rows.Next() {
		var t AssistantTask
		var promoted int
		var started string
		var finished sql.NullString
		if err := rows.Scan(&t.ID, &t.Title, &t.Prompt, &t.ChatID, &t.State, &t.PID, &t.SessionID,
			&t.Dir, &t.OutputPath, &promoted, &started, &finished, &t.Error); err != nil {
			return nil, err
		}
		t.Promoted = promoted != 0
		t.StartedAt = parseStoredTime(started)
		t.FinishedAt = optionalTime(finished)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveAssistantTask upserts one task.
func (s *Store) SaveAssistantTask(ctx context.Context, userID string, t AssistantTask) error {
	promoted := 0
	if t.Promoted {
		promoted = 1
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO assistant_tasks (id, user_id, title, prompt, chat_id, state, pid, session_id, dir, output_path, promoted, started_at, finished_at, error)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  title = excluded.title, prompt = excluded.prompt, chat_id = excluded.chat_id,
  state = excluded.state, pid = excluded.pid, session_id = excluded.session_id,
  dir = excluded.dir, output_path = excluded.output_path, promoted = excluded.promoted,
  started_at = excluded.started_at, finished_at = excluded.finished_at, error = excluded.error`,
		t.ID, userID, t.Title, t.Prompt, t.ChatID, t.State, t.PID, t.SessionID,
		t.Dir, t.OutputPath, promoted, t.StartedAt.UTC().Format(time.RFC3339Nano),
		storeTimeNano(t.FinishedAt), t.Error)
	return err
}

// DeleteAssistantTask removes one task.
func (s *Store) DeleteAssistantTask(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_tasks WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// --- push devices ---

// RegisterPushDevice records a device token, refreshing one already known.
func (s *Store) RegisterPushDevice(ctx context.Context, userID, token, platform string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO push_devices (token, user_id, platform, created_at, updated_at) VALUES (?,?,?,?,?)
ON CONFLICT(token) DO UPDATE SET
  user_id = excluded.user_id, platform = excluded.platform, updated_at = excluded.updated_at`,
		token, userID, platform, now, now)
	return err
}

// ListPushDevices returns every registered device for a user.
func (s *Store) ListPushDevices(ctx context.Context, userID string) ([]PushDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT token, platform, created_at, updated_at FROM push_devices WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		var created, updated string
		if err := rows.Scan(&d.Token, &d.Platform, &created, &updated); err != nil {
			return nil, err
		}
		d.CreatedAt = parseStoredTime(created)
		d.UpdatedAt = parseStoredTime(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletePushDevice forgets a device, used when FCM reports it is gone.
func (s *Store) DeletePushDevice(ctx context.Context, userID, token string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_devices WHERE user_id = ? AND token = ?`, userID, token)
	return err
}

// --- job → chat routing ---

// RecordJobChat remembers where a launched AI job's result must be delivered.
func (s *Store) RecordJobChat(ctx context.Context, jobID, userID, chatID, machine string) error {
	if strings.TrimSpace(jobID) == "" {
		return fmt.Errorf("job id required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO job_chats (job_id, user_id, chat_id, machine, created_at) VALUES (?,?,?,?,?)
ON CONFLICT(job_id) DO UPDATE SET chat_id = excluded.chat_id, machine = excluded.machine`,
		jobID, userID, chatID, machine, time.Now().UTC().Format(time.RFC3339))
	return err
}

// JobChatByID returns the delivery target of a job, or nil when unknown.
func (s *Store) JobChatByID(ctx context.Context, jobID string) (*JobChat, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT job_id, user_id, chat_id, machine, created_at, delivered_at FROM job_chats WHERE job_id = ?`, jobID)
	jc, err := scanJobChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &jc, nil
}

// PendingJobChats lists undelivered job results newer than the cutoff, oldest
// first. The 60s sweeper retries these.
func (s *Store) PendingJobChats(ctx context.Context, userID string, since time.Time) ([]JobChat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, user_id, chat_id, machine, created_at, delivered_at FROM job_chats
WHERE user_id = ? AND delivered_at IS NULL AND created_at > ?
ORDER BY created_at ASC LIMIT 50`, userID, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobChat
	for rows.Next() {
		jc, err := scanJobChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, jc)
	}
	return out, rows.Err()
}

// MarkJobChatDelivered stamps a job result as sent. It is also the dedupe guard
// against a duplicate ai_done event.
func (s *Store) MarkJobChatDelivered(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_chats SET delivered_at = ? WHERE job_id = ? AND delivered_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), jobID)
	return err
}

// PurgeJobChats drops routing rows older than the retention window.
func (s *Store) PurgeJobChats(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_chats WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanJobChat(row rowScanner) (JobChat, error) {
	var jc JobChat
	var created string
	var delivered sql.NullString
	if err := row.Scan(&jc.JobID, &jc.UserID, &jc.ChatID, &jc.Machine, &created, &delivered); err != nil {
		return JobChat{}, err
	}
	jc.CreatedAt = parseStoredTime(created)
	jc.DeliveredAt = optionalTime(delivered)
	return jc, nil
}

// --- helpers ---

func parseStoredTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func optionalTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseStoredTime(v.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

func storeTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func storeTimeNano(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
