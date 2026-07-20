package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
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
	if err := s.migrateEmailSettings(); err != nil {
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

CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  code_challenge_method TEXT NOT NULL DEFAULT 'S256',
  scope TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  used INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_user ON oauth_tokens(user_id);

CREATE TABLE IF NOT EXISTS oauth_refresh (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS email_settings (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  api_key_enc TEXT NOT NULL,
  domains TEXT NOT NULL DEFAULT '[]',
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_memory (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  content TEXT NOT NULL DEFAULT '',
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

func (s *Store) CreateUser(ctx context.Context, email, password string) (*User, error) {
	email = normalizeEmail(email)
	if email == "" || len(password) < 8 {
		return nil, fmt.Errorf("email required and password min 8 chars")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, created_at) VALUES (?,?,?,?)`,
		id, email, string(hash), now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	for _, mid := range []string{"machine", "mercadona"} {
		_, _ = s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO user_modules (user_id, module_id, enabled) VALUES (?,?,0)`,
			id, mid)
	}
	return &User{ID: id, Email: email, PasswordHash: string(hash), CreatedAt: now}, nil
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

// --- OAuth access tokens (MCP Authorization: Bearer) ---

// SaveAuthCode stores a one-time authorization code (hashed).
func (s *Store) SaveAuthCode(ctx context.Context, rawCode, userID, clientID, redirectURI, challenge, method, scope string, ttl time.Duration) error {
	exp := time.Now().UTC().Add(ttl)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO oauth_codes (code_hash, user_id, client_id, redirect_uri, code_challenge, code_challenge_method, scope, expires_at, used)
VALUES (?,?,?,?,?,?,?,?,0)`,
		hashToken(rawCode), userID, clientID, redirectURI, challenge, method, scope, exp.Format(time.RFC3339))
	return err
}

// ConsumeAuthCode validates and marks a code used. Returns userID, clientID, redirectURI, challenge, method, scope.
func (s *Store) ConsumeAuthCode(ctx context.Context, rawCode string) (userID, clientID, redirectURI, challenge, method, scope string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	var exp string
	var used int
	err = tx.QueryRowContext(ctx, `
SELECT user_id, client_id, redirect_uri, code_challenge, code_challenge_method, scope, expires_at, used
FROM oauth_codes WHERE code_hash = ?`, hashToken(rawCode)).
		Scan(&userID, &clientID, &redirectURI, &challenge, &method, &scope, &exp, &used)
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("invalid code")
	}
	if used != 0 {
		return "", "", "", "", "", "", fmt.Errorf("code already used")
	}
	t, _ := time.Parse(time.RFC3339, exp)
	if time.Now().UTC().After(t) {
		return "", "", "", "", "", "", fmt.Errorf("code expired")
	}
	res, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET used = 1 WHERE code_hash = ? AND used = 0`, hashToken(rawCode))
	if err != nil {
		return "", "", "", "", "", "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", "", "", "", "", "", fmt.Errorf("code already used")
	}
	if err := tx.Commit(); err != nil {
		return "", "", "", "", "", "", err
	}
	return userID, clientID, redirectURI, challenge, method, scope, nil
}

// IssueAccessToken stores and returns a raw access token.
func (s *Store) IssueAccessToken(ctx context.Context, userID, clientID, scope string, ttl time.Duration) (string, time.Time, error) {
	raw, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().UTC().Add(ttl)
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO oauth_tokens (token_hash, user_id, client_id, scope, expires_at, created_at) VALUES (?,?,?,?,?,?)`,
		hashToken(raw), userID, clientID, scope, exp.Format(time.RFC3339), now.Format(time.RFC3339))
	return raw, exp, err
}

// IssueRefreshToken stores and returns a raw refresh token.
func (s *Store) IssueRefreshToken(ctx context.Context, userID, clientID, scope string, ttl time.Duration) (string, error) {
	raw, err := randomHex(32)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(ttl)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO oauth_refresh (token_hash, user_id, client_id, scope, expires_at, created_at) VALUES (?,?,?,?,?,?)`,
		hashToken(raw), userID, clientID, scope, exp.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	return raw, err
}

// ConsumeRefreshToken validates a refresh token.
func (s *Store) ConsumeRefreshToken(ctx context.Context, raw string) (userID, clientID, scope string, err error) {
	var exp string
	err = s.db.QueryRowContext(ctx, `
SELECT user_id, client_id, scope, expires_at FROM oauth_refresh WHERE token_hash = ?`, hashToken(raw)).
		Scan(&userID, &clientID, &scope, &exp)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid refresh token")
	}
	t, _ := time.Parse(time.RFC3339, exp)
	if time.Now().UTC().After(t) {
		return "", "", "", fmt.Errorf("refresh token expired")
	}
	return userID, clientID, scope, nil
}

// UserByAccessToken resolves an OAuth access token to a user.
func (s *Store) UserByAccessToken(ctx context.Context, raw string) (*User, error) {
	if raw == "" {
		return nil, sql.ErrNoRows
	}
	var userID, exp string
	err := s.db.QueryRowContext(ctx, `
SELECT user_id, expires_at FROM oauth_tokens WHERE token_hash = ?`, hashToken(raw)).
		Scan(&userID, &exp)
	if err != nil {
		return nil, err
	}
	t, _ := time.Parse(time.RFC3339, exp)
	if time.Now().UTC().After(t) {
		return nil, sql.ErrNoRows
	}
	return s.UserByID(ctx, userID)
}

// --- modules ---

type ModuleState struct {
	ModuleID   string
	Enabled    bool
	ConfigJSON string
}

// defaultModuleIDs must stay in sync with modules.Catalog.
var defaultModuleIDs = []string{"machine", "mercadona", "email", "memory"}

func (s *Store) ListModules(ctx context.Context, userID string) ([]ModuleState, error) {
	// ensure defaults exist
	for _, mid := range defaultModuleIDs {
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

// --- email (Resend) ---

// EmailDomain is a Resend domain with a user enable flag.
type EmailDomain struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
	Sending   string `json:"sending,omitempty"`   // Resend capability: enabled|disabled
	Receiving string `json:"receiving,omitempty"` // Resend capability: enabled|disabled
	Enabled   bool   `json:"enabled"`             // user toggle for Takan tools
}

// migrateEmailSettings upgrades legacy from_addr / plain domain lists.
func (s *Store) migrateEmailSettings() error {
	rows, err := s.db.Query(`PRAGMA table_info(email_settings)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if len(cols) == 0 {
		return nil
	}
	if !cols["domains"] && cols["from_addr"] {
		if _, err := s.db.Exec(`ALTER TABLE email_settings ADD COLUMN domains TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return err
		}
		r2, err := s.db.Query(`SELECT user_id, from_addr FROM email_settings`)
		if err != nil {
			return err
		}
		type row struct{ uid, from string }
		var list []row
		for r2.Next() {
			var u, f string
			if err := r2.Scan(&u, &f); err != nil {
				r2.Close()
				return err
			}
			list = append(list, row{u, f})
		}
		r2.Close()
		for _, it := range list {
			dom := domainFromEmail(it.from)
			raw := "[]"
			if dom != "" {
				b, _ := json.Marshal([]EmailDomain{{Name: dom, Enabled: true, Status: "legacy"}})
				raw = string(b)
			}
			if _, err := s.db.Exec(`UPDATE email_settings SET domains = ? WHERE user_id = ?`, raw, it.uid); err != nil {
				return err
			}
		}
	}
	// Upgrade plain string[] domains → EmailDomain objects.
	r3, err := s.db.Query(`SELECT user_id, domains FROM email_settings`)
	if err != nil {
		return err
	}
	defer r3.Close()
	type up struct{ uid, raw string }
	var ups []up
	for r3.Next() {
		var u, raw string
		if err := r3.Scan(&u, &raw); err != nil {
			return err
		}
		ups = append(ups, up{u, raw})
	}
	for _, it := range ups {
		if _, ok := tryParseEmailDomains(it.raw); ok {
			continue
		}
		// plain string list
		var names []string
		if err := json.Unmarshal([]byte(it.raw), &names); err != nil {
			continue
		}
		var doms []EmailDomain
		for _, n := range names {
			n = normalizeDomainName(n)
			if n == "" {
				continue
			}
			doms = append(doms, EmailDomain{Name: n, Enabled: true})
		}
		b, _ := json.Marshal(doms)
		if _, err := s.db.Exec(`UPDATE email_settings SET domains = ? WHERE user_id = ?`, string(b), it.uid); err != nil {
			return err
		}
	}
	return nil
}

func domainFromEmail(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	if strings.Contains(addr, ".") && !strings.Contains(addr, " ") {
		return strings.TrimPrefix(addr, "@")
	}
	return ""
}

func normalizeDomainName(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "@")
	d = strings.TrimSuffix(d, ".")
	if d == "" || !strings.Contains(d, ".") {
		return ""
	}
	return d
}

func (s *Store) SaveEmailAPIKey(ctx context.Context, userID, apiKeyEnc string) error {
	// Preserve existing domain toggles if row exists.
	_, err := s.db.ExecContext(ctx, `
INSERT INTO email_settings (user_id, api_key_enc, domains, updated_at) VALUES (?,?, '[]', ?)
ON CONFLICT(user_id) DO UPDATE SET
  api_key_enc = excluded.api_key_enc,
  updated_at = excluded.updated_at`,
		userID, apiKeyEnc, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) SaveEmailDomains(ctx context.Context, userID string, domains []EmailDomain) error {
	raw, err := json.Marshal(domains)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE email_settings SET domains = ?, updated_at = ? WHERE user_id = ?`,
		string(raw), time.Now().UTC().Format(time.RFC3339), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("email not configured")
	}
	return nil
}

func (s *Store) GetEmailSettings(ctx context.Context, userID string) (apiKeyEnc string, domains []EmailDomain, ok bool, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx,
		`SELECT api_key_enc, domains FROM email_settings WHERE user_id = ?`, userID).
		Scan(&apiKeyEnc, &raw)
	if err == sql.ErrNoRows {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	domains, _ = tryParseEmailDomains(raw)
	return apiKeyEnc, domains, true, nil
}

func (s *Store) DeleteEmailSettings(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM email_settings WHERE user_id = ?`, userID)
	return err
}

// EnabledEmailDomains returns names of domains the user enabled for tools.
func EnabledEmailDomains(domains []EmailDomain) []string {
	var out []string
	for _, d := range domains {
		if d.Enabled && d.Name != "" {
			out = append(out, d.Name)
		}
	}
	return out
}

func tryParseEmailDomains(raw string) ([]EmailDomain, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, true
	}
	var list []EmailDomain
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, false
	}
	// Detect plain string array mis-parsed as objects with empty Name
	if len(list) > 0 && list[0].Name == "" {
		var names []string
		if err := json.Unmarshal([]byte(raw), &names); err == nil {
			return nil, false
		}
	}
	for i := range list {
		list[i].Name = normalizeDomainName(list[i].Name)
	}
	return list, true
}

// MergeEmailDomains keeps user Enabled flags when refreshing from Resend.
func MergeEmailDomains(prev, fromAPI []EmailDomain) []EmailDomain {
	prevEn := map[string]bool{}
	for _, d := range prev {
		prevEn[d.Name] = d.Enabled
	}
	out := make([]EmailDomain, 0, len(fromAPI))
	for _, d := range fromAPI {
		d.Name = normalizeDomainName(d.Name)
		if d.Name == "" {
			continue
		}
		if en, ok := prevEn[d.Name]; ok {
			d.Enabled = en
		} else {
			// New domain: enable if verified (or sending enabled).
			d.Enabled = d.Status == "verified" || d.Sending == "enabled"
		}
		out = append(out, d)
	}
	return out
}

// --- memory ---

func (s *Store) GetMemory(ctx context.Context, userID string) (content string, updatedAt time.Time, ok bool, err error) {
	var updated string
	err = s.db.QueryRowContext(ctx,
		`SELECT content, updated_at FROM user_memory WHERE user_id = ?`, userID).
		Scan(&content, &updated)
	if err == sql.ErrNoRows {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	updatedAt, _ = time.Parse(time.RFC3339, updated)
	return content, updatedAt, true, nil
}

func (s *Store) SetMemory(ctx context.Context, userID, content string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO user_memory (user_id, content, updated_at) VALUES (?,?,?)
ON CONFLICT(user_id) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		userID, content, time.Now().UTC().Format(time.RFC3339))
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
