package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// migrateMercadonaTables embeds Mercadona multi-tenant tables into the main Takan DB.
// Table names match modules/mercadona (accounts, grocery_*).
func (s *Store) migrateMercadonaTables() error {
	stmts := []string{
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
		`CREATE INDEX IF NOT EXISTS idx_aliases_account ON grocery_aliases(account_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_aliases_account_alias ON grocery_aliases(account_id, alias)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_account ON grocery_pending(account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_preferred_account ON grocery_preferred(account_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("mercadona schema: %w", err)
		}
	}
	return nil
}

// ImportLegacyMercadonaDB copies rows from a standalone mercadona.db into the main store.
// Safe to call repeatedly. No-op if path is missing.
func (s *Store) ImportLegacyMercadonaDB(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	src, err := sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return err
	}
	defer src.Close()

	rows, err := src.Query(`SELECT id, api_token_hash, COALESCE(email_hint,''), COALESCE(postal_code,''),
		COALESCE(warehouse,''), access_token_enc, COALESCE(refresh_token_enc,''), customer_id,
		created_at, last_used_at FROM accounts`)
	if err != nil {
		log.Printf("mercadona import: skip accounts from %s: %v", path, err)
		return nil
	}
	nAcc := 0
	for rows.Next() {
		var id, hash, hint, postal, wh, access, refresh, cust string
		var created, last any
		if err := rows.Scan(&id, &hash, &hint, &postal, &wh, &access, &refresh, &cust, &created, &last); err != nil {
			rows.Close()
			return err
		}
		_, err := s.db.Exec(`
INSERT INTO accounts (id, api_token_hash, email_hint, postal_code, warehouse,
  access_token_enc, refresh_token_enc, customer_id, created_at, last_used_at)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  api_token_hash=excluded.api_token_hash,
  email_hint=excluded.email_hint,
  postal_code=excluded.postal_code,
  warehouse=excluded.warehouse,
  access_token_enc=excluded.access_token_enc,
  refresh_token_enc=excluded.refresh_token_enc,
  customer_id=excluded.customer_id,
  last_used_at=excluded.last_used_at`,
			id, hash, hint, postal, wh, access, refresh, cust, created, last)
		if err != nil {
			rows.Close()
			return err
		}
		nAcc++
	}
	rows.Close()

	for _, tbl := range []string{"grocery_aliases", "grocery_pending", "grocery_preferred"} {
		if err := copyGroceryTable(src, s.db, tbl); err != nil {
			log.Printf("mercadona import %s: %v", tbl, err)
		}
	}
	if nAcc > 0 {
		log.Printf("mercadona: imported %d account(s) from %s into main DB", nAcc, filepath.Base(path))
	} else {
		log.Printf("mercadona: legacy file %s had no accounts (tables ready in main DB)", filepath.Base(path))
	}
	return nil
}

func copyGroceryTable(src, dst *sql.DB, table string) error {
	rows, err := src.Query(`SELECT * FROM ` + table)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	// Skip auto id on insert conflict by using column list without relying on id uniqueness across DBs.
	colList := strings.Join(cols, ",")
	ph := strings.Repeat("?,", len(cols))
	ph = strings.TrimSuffix(ph, ",")
	q := fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)", table, colList, ph)
	n := 0
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if _, err := dst.Exec(q, raw...); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		log.Printf("mercadona: imported %d row(s) into %s", n, table)
	}
	return rows.Err()
}

// EnsureMercadonaSchema is exported for tests.
func (s *Store) EnsureMercadonaSchema(ctx context.Context) error {
	_ = ctx
	return s.migrateMercadonaTables()
}
