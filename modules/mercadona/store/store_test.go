package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenMigratesLegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE grocery_aliases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			alias TEXT NOT NULL UNIQUE,
			product_id TEXT NOT NULL,
			product_name TEXT NOT NULL,
			use_count INTEGER NOT NULL DEFAULT 1,
			last_used TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE grocery_pending (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			alias_text TEXT NOT NULL,
			options_json TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			resolved_at TIMESTAMP
		)`,
		`INSERT INTO grocery_aliases (alias, product_id, product_name) VALUES ('leche', '1', 'Leche')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	defer db.Close()

	var accountID, alias string
	if err := db.QueryRow(`SELECT account_id, alias FROM grocery_aliases`).Scan(&accountID, &alias); err != nil {
		t.Fatal(err)
	}
	if accountID != "local" || alias != "leche" {
		t.Fatalf("got account=%q alias=%q", accountID, alias)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='grocery_preferred'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("preferred table missing")
	}

	_, err = db.Exec(`
		INSERT INTO grocery_aliases (account_id, alias, product_id, product_name) VALUES ('local', 'leche', '2', 'Leche 2')
		ON CONFLICT(account_id, alias) DO UPDATE SET product_id = excluded.product_id
	`)
	if err != nil {
		t.Fatalf("on conflict upsert: %v", err)
	}

	var pid string
	if err := db.QueryRow(`SELECT product_id FROM grocery_aliases WHERE alias='leche'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if pid != "2" {
		t.Fatalf("want product_id 2 after upsert, got %s", pid)
	}
}

func TestOpenFreshHasPreferred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='grocery_preferred'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("preferred table: n=%d err=%v", n, err)
	}
}

func TestOpenRealHomeDBIfPresent(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(home, ".takan", "data.db")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no local data.db")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open home db: %v", err)
	}
	_ = db.Close()
}
