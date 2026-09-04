package config

import "testing"

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
