package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnv2PrefersAtlasAndFallsBackToTakan(t *testing.T) {
	t.Setenv("ATLAS_PUBLIC_URL", "https://new.example.com")
	t.Setenv("TAKAN_PUBLIC_URL", "https://old.example.com")
	t.Setenv("TAKAN_DATA_DIR", "/legacy/data")

	c := Load()
	if c.PublicURL != "https://new.example.com" {
		t.Fatalf("ATLAS_ must win, got %q", c.PublicURL)
	}
	if c.DataDir != "/legacy/data" {
		t.Fatalf("TAKAN_ must still be honoured, got %q", c.DataDir)
	}
}

func TestAppURLDefaultsToPublicURL(t *testing.T) {
	t.Setenv("ATLAS_PUBLIC_URL", "https://panel.example.com/")
	c := Load()
	if c.AppURL != "https://panel.example.com" {
		t.Fatalf("app url should default to the panel host, got %q", c.AppURL)
	}

	t.Setenv("ATLAS_APP_URL", "https://app.example.com")
	c = Load()
	if c.AppURL != "https://app.example.com" {
		t.Fatalf("explicit app url should win, got %q", c.AppURL)
	}
}

func TestOwnerTelegramIDParsing(t *testing.T) {
	t.Setenv("OWNER_TELEGRAM_ID", "282611642")
	if got := Load().OwnerTelegramID; got != 282611642 {
		t.Fatalf("owner telegram id = %d", got)
	}
	// A malformed value must not take down the process; it falls back to 0 and
	// the assistant then refuses to start.
	t.Setenv("OWNER_TELEGRAM_ID", "not-a-number")
	if got := Load().OwnerTelegramID; got != 0 {
		t.Fatalf("expected 0 for a malformed id, got %d", got)
	}
}

// TestFirebaseServiceAccountFromFile covers the shape prod was already using:
// a path rather than the inline JSON. Push silently staying off because the
// binary only read one of the two names is the bug this guards.
func TestFirebaseServiceAccountFromFile(t *testing.T) {
	const account = `{"project_id":"demo","type":"service_account"}`
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, []byte(account+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_FILE", path)
	if got := Load().FirebaseServiceAccount; got != account {
		t.Fatalf("file contents should be loaded, got %q", got)
	}

	// The inline value wins, so an env that carries both does not depend on a
	// file that may not exist on this host.
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", `{"project_id":"inline"}`)
	if got := Load().FirebaseServiceAccount; got != `{"project_id":"inline"}` {
		t.Fatalf("inline JSON must win, got %q", got)
	}

	// A bad path leaves push off instead of killing the process.
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("FIREBASE_SERVICE_ACCOUNT_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if got := Load().FirebaseServiceAccount; got != "" {
		t.Fatalf("an unreadable path must yield an empty account, got %q", got)
	}
}

// TestCheckLocalAddrRejectsNonLoopback guards an unauthenticated control API.
// The local listener serves /jobs, /tasks and /internal/send with no auth at
// all — it is safe only because nothing off the host can reach it.
func TestCheckLocalAddrRejectsNonLoopback(t *testing.T) {
	ok := []string{"127.0.0.1:8099", "[::1]:8099", "127.0.0.5:9000"}
	for _, addr := range ok {
		t.Setenv("ATLAS_LOCAL_ADDR", addr)
		if err := Load().CheckLocalAddr(); err != nil {
			t.Fatalf("%s should be accepted: %v", addr, err)
		}
	}

	bad := map[string]string{
		"0.0.0.0:8099":      "binds every interface",
		":8099":             "binds every interface",
		"192.168.1.10:8099": "not a loopback",
		"10.0.0.1:8099":     "not a loopback",
		"localhost:8099":    "must be a loopback IP",
		"[::]:8099":         "binds every interface",
		"127.0.0.1":         "not host:port",
	}
	for addr, want := range bad {
		t.Setenv("ATLAS_LOCAL_ADDR", addr)
		err := Load().CheckLocalAddr()
		if err == nil {
			t.Fatalf("%s must be rejected", addr)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: expected an error mentioning %q, got %v", addr, want, err)
		}
	}
}

func TestCheckLocalAddrDefaultIsLoopback(t *testing.T) {
	if err := Load().CheckLocalAddr(); err != nil {
		t.Fatalf("the default must be safe: %v", err)
	}
}
