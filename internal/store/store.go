package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/mentasystems/colmena"
	"github.com/mentasystems/colmena/backup/s3"
	"golang.org/x/crypto/bcrypt"
)

// Store wraps Colmena SQLite.
type Store struct {
	node *colmena.Node
	db   *sql.DB
}

// Open starts Colmena (optional continuous backup) and migrates schema.
func Open(dataDir string, backup *BackupOpts) (*Store, error) {
	cfg := colmena.Config{DataDir: dataDir}
	if backup != nil && backup.Bucket != "" && backup.AccessKey != "" {
		b := *backup
		cfg.Backup = &colmena.BackupConfig{
			NewBackend: func(db string) (colmena.BackupBackend, error) {
				return s3.NewBackend(s3.Config{
					Endpoint:  b.Endpoint,
					Region:    b.Region,
					Bucket:    b.Bucket,
					Prefix:    b.Prefix + db,
					AccessKey: b.AccessKey,
					SecretKey: b.SecretKey,
				})
			},
			OnError: func(db string, err error) {
				log.Printf("colmena backup %s: %v", db, err)
			},
		}
		log.Printf("colmena: continuous backup enabled → s3://%s/%s", backup.Bucket, backup.Prefix)
	} else {
		log.Printf("colmena: backup disabled (set TAKAN_BACKUP_BUCKET + AWS keys to enable)")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	node, err := colmena.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("colmena: %w", err)
	}
	s := &Store{node: node, db: node.DB()}
	if err := s.migrate(); err != nil {
		_ = node.Close()
		return nil, err
	}
	return s, nil
}

// BackupOpts configures Colmena S3 backup.
type BackupOpts struct {
	Endpoint, Region, Bucket, Prefix, AccessKey, SecretKey string
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s.node != nil {
		return s.node.Close()
	}
	return nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS web_sessions (
  token TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_web_sessions_user ON web_sessions(user_id);

CREATE TABLE IF NOT EXISTS mcp_tokens (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT 'default',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_tokens_user ON mcp_tokens(user_id);

CREATE TABLE IF NOT EXISTS user_modules (
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  module_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0,
  config_json TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (user_id, module_id)
);

CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  agent_token_hash TEXT NOT NULL UNIQUE,
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_machines_user ON machines(user_id);

CREATE TABLE IF NOT EXISTS mercadona_creds (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  email TEXT NOT NULL,
  password_enc TEXT NOT NULL,
  postal_code TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`)
	return err
}

// --- users ---

type User struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

func (s *Store) CreateUser(ctx context.Context, email, password string) (*User, string, error) {
	email = normalizeEmail(email)
	if email == "" || len(password) < 8 {
		return nil, "", fmt.Errorf("email required and password min 8 chars")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, created_at) VALUES (?,?,?,?)`,
		id, email, string(hash), now.Format(time.RFC3339))
	if err != nil {
		return nil, "", fmt.Errorf("create user: %w", err)
	}
	// default modules rows (disabled)
	for _, mid := range []string{"machine", "mercadona"} {
		_, _ = s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO user_modules (user_id, module_id, enabled) VALUES (?,?,0)`,
			id, mid)
	}
	// issue MCP token
	raw, err := s.CreateMCPToken(ctx, id, "default")
	if err != nil {
		return nil, "", err
	}
	return &User{ID: id, Email: email, PasswordHash: string(hash), CreatedAt: now}, raw, nil
}

func (s *Store) Authenticate(ctx context.Context, email, password string) (*User, error) {
	email = normalizeEmail(email)
	var u User
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &created)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &u, nil
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	var u User
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &created)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &u, nil
}

// --- web sessions ---

func (s *Store) CreateWebSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	tok, err := randomHex(32)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(ttl)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO web_sessions (token, user_id, expires_at) VALUES (?,?,?)`,
		tok, userID, exp.Format(time.RFC3339))
	return tok, err
}

func (s *Store) UserByWebSession(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, sql.ErrNoRows
	}
	var userID, exp string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM web_sessions WHERE token = ?`, token).
		Scan(&userID, &exp)
	if err != nil {
		return nil, err
	}
	t, _ := time.Parse(time.RFC3339, exp)
	if time.Now().UTC().After(t) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token = ?`, token)
		return nil, sql.ErrNoRows
	}
	return s.UserByID(ctx, userID)
}

func (s *Store) DeleteWebSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token = ?`, token)
	return err
}

// --- MCP tokens ---

func (s *Store) CreateMCPToken(ctx context.Context, userID, name string) (string, error) {
	raw, err := randomHex(32)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mcp_tokens (id, user_id, token_hash, name, created_at) VALUES (?,?,?,?,?)`,
		id, userID, hashToken(raw), name, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Store) UserByMCPToken(ctx context.Context, raw string) (*User, error) {
	if raw == "" {
		return nil, sql.ErrNoRows
	}
	var userID string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM mcp_tokens WHERE token_hash = ?`, hashToken(raw)).
		Scan(&userID)
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}

func (s *Store) LatestMCPTokenHint(ctx context.Context, userID string) (createdAt string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT created_at FROM mcp_tokens WHERE user_id = ? ORDER BY created_at DESC LIMIT 1`, userID).
		Scan(&createdAt)
	return
}

// --- modules ---

type ModuleState struct {
	ModuleID   string
	Enabled    bool
	ConfigJSON string
}

func (s *Store) ListModules(ctx context.Context, userID string) ([]ModuleState, error) {
	// ensure defaults exist
	for _, mid := range []string{"machine", "mercadona"} {
		_, _ = s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO user_modules (user_id, module_id, enabled) VALUES (?,?,0)`,
			userID, mid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT module_id, enabled, config_json FROM user_modules WHERE user_id = ? ORDER BY module_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModuleState
	for rows.Next() {
		var m ModuleState
		var en int
		if err := rows.Scan(&m.ModuleID, &en, &m.ConfigJSON); err != nil {
			return nil, err
		}
		m.Enabled = en != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetModuleEnabled(ctx context.Context, userID, moduleID string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO user_modules (user_id, module_id, enabled) VALUES (?,?,?)
ON CONFLICT(user_id, module_id) DO UPDATE SET enabled = excluded.enabled`,
		userID, moduleID, en)
	return err
}

func (s *Store) ModuleEnabled(ctx context.Context, userID, moduleID string) (bool, error) {
	var en int
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled FROM user_modules WHERE user_id = ? AND module_id = ?`, userID, moduleID).
		Scan(&en)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return en != 0, err
}

// --- machines ---

type Machine struct {
	ID        string
	UserID    string
	Name      string
	LastSeen  *time.Time
	CreatedAt time.Time
}

// CreateMachine returns (machine, rawAgentToken, error).
func (s *Store) CreateMachine(ctx context.Context, userID, name string) (*Machine, string, error) {
	name = normalizeName(name)
	if name == "" {
		return nil, "", fmt.Errorf("name required")
	}
	raw, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO machines (id, user_id, name, agent_token_hash, created_at) VALUES (?,?,?,?,?)`,
		id, userID, name, hashToken(raw), now.Format(time.RFC3339))
	if err != nil {
		return nil, "", fmt.Errorf("create machine: %w", err)
	}
	return &Machine{ID: id, UserID: userID, Name: name, CreatedAt: now}, raw, nil
}

func (s *Store) MachineByAgentToken(ctx context.Context, raw string) (*Machine, error) {
	var m Machine
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, name, last_seen_at, created_at FROM machines WHERE agent_token_hash = ?`,
		hashToken(raw)).Scan(&m.ID, &m.UserID, &m.Name, &last, &created)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339, last.String)
		m.LastSeen = &t
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &m, nil
}

func (s *Store) TouchMachine(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE machines SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) ListMachines(ctx context.Context, userID string) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, name, last_seen_at, created_at FROM machines WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		var m Machine
		var last, created sql.NullString
		if err := rows.Scan(&m.ID, &m.UserID, &m.Name, &last, &created); err != nil {
			return nil, err
		}
		if last.Valid {
			t, _ := time.Parse(time.RFC3339, last.String)
			m.LastSeen = &t
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMachine(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MachineByUserAndName(ctx context.Context, userID, name string) (*Machine, error) {
	var m Machine
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, name, last_seen_at, created_at FROM machines WHERE user_id = ? AND name = ?`,
		userID, name).Scan(&m.ID, &m.UserID, &m.Name, &last, &created)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339, last.String)
		m.LastSeen = &t
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &m, nil
}

// --- mercadona creds (encrypted blob stored as text; key from env later) ---

func (s *Store) SaveMercadonaCreds(ctx context.Context, userID, email, passwordEnc, postal string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mercadona_creds (user_id, email, password_enc, postal_code, updated_at) VALUES (?,?,?,?,?)
ON CONFLICT(user_id) DO UPDATE SET
  email = excluded.email,
  password_enc = excluded.password_enc,
  postal_code = excluded.postal_code,
  updated_at = excluded.updated_at`,
		userID, email, passwordEnc, postal, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetMercadonaCreds(ctx context.Context, userID string) (email, passwordEnc, postal string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT email, password_enc, postal_code FROM mercadona_creds WHERE user_id = ?`, userID).
		Scan(&email, &passwordEnc, &postal)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return email, passwordEnc, postal, true, nil
}

func (s *Store) DeleteMercadonaCreds(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mercadona_creds WHERE user_id = ?`, userID)
	return err
}

// --- helpers ---

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func normalizeEmail(e string) string {
	return stringMin(trimSpaceLower(e), 200)
}

func normalizeName(s string) string {
	s = trimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

func trimSpaceLower(s string) string {
	s = trimSpace(s)
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func stringMin(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
