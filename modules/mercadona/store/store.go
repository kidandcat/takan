// Package store opens the SQLite database used for accounts, sessions, aliases and pending choices.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// LocalAccountID is the single-tenant account used by stdio / env-credentials mode.
const LocalAccountID = "local"

// Open opens (and migrates) the SQLite database at path. Caller closes.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			path = "./data/mercadona-mcp.db"
		} else {
			path = filepath.Join(home, ".mercadona-mcp", "data.db")
		}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		// Hosted multi-tenant accounts. Tokens are stored encrypted (AES-GCM).
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			api_token_hash TEXT NOT NULL UNIQUE,
			email_hint TEXT,
			postal_code TEXT,
			warehouse TEXT,
			access_token_enc TEXT NOT NULL,
			refresh_token_enc TEXT,
			customer_id TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_used_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_token ON accounts(api_token_hash)`,

		// Legacy single-row session for stdio mode (account_id = 'local').
		`CREATE TABLE IF NOT EXISTS mercadona_session (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			access_token TEXT NOT NULL,
			refresh_token TEXT,
			customer_id TEXT NOT NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,

		// Fresh installs get multi-tenant columns. Older single-tenant DBs keep
		// the original table shape (CREATE IF NOT EXISTS is a no-op) and are
		// patched below with ALTER + indexes that need account_id.
		`CREATE TABLE IF NOT EXISTS grocery_aliases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL DEFAULT 'local',
			alias TEXT NOT NULL,
			product_id TEXT NOT NULL,
			product_name TEXT NOT NULL,
			use_count INTEGER NOT NULL DEFAULT 1,
			last_used TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(account_id, alias)
		)`,

		`CREATE TABLE IF NOT EXISTS grocery_pending (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL DEFAULT 'local',
			alias_text TEXT NOT NULL,
			options_json TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			resolved_at TIMESTAMP
		)`,

		// Products the user has chosen before. When a search returns several
		// options and exactly one is preferred, the server auto-picks it so the
		// agent does not re-ask.
		`CREATE TABLE IF NOT EXISTS grocery_preferred (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL DEFAULT 'local',
			product_id TEXT NOT NULL,
			product_name TEXT NOT NULL,
			use_count INTEGER NOT NULL DEFAULT 1,
			last_used TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(account_id, product_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// Best-effort: add account_id to old DBs that predate multi-tenant.
	// Must run before indexes that reference account_id.
	_, _ = db.Exec(`ALTER TABLE grocery_aliases ADD COLUMN account_id TEXT NOT NULL DEFAULT 'local'`)
	_, _ = db.Exec(`ALTER TABLE grocery_pending ADD COLUMN account_id TEXT NOT NULL DEFAULT 'local'`)

	// Indexes + unique constraints that older schemas may lack.
	// ON CONFLICT(account_id, alias) needs a matching unique index.
	post := []string{
		`CREATE INDEX IF NOT EXISTS idx_aliases_account ON grocery_aliases(account_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_aliases_account_alias ON grocery_aliases(account_id, alias)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_account ON grocery_pending(account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_preferred_account ON grocery_preferred(account_id)`,
	}
	for _, s := range post {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}
