package config

import "testing"

// TestLoadReadsTakanEnv pins the variable names the deployed env file uses.
// A rename here silently reverts every setting to its default on the next boot.
func TestLoadReadsTakanEnv(t *testing.T) {
	t.Setenv("TAKAN_PUBLIC_URL", "https://panel.example.com/")
	t.Setenv("TAKAN_DATA_DIR", "/srv/takan/data")
	t.Setenv("TAKAN_LISTEN", "127.0.0.1:8096")
	t.Setenv("TAKAN_AUTH_PER_MIN", "7")

	c := Load()
	if c.PublicURL != "https://panel.example.com" {
		t.Fatalf("public url = %q (the trailing slash must be trimmed)", c.PublicURL)
	}
	if c.DataDir != "/srv/takan/data" {
		t.Fatalf("data dir = %q", c.DataDir)
	}
	if c.Listen != "127.0.0.1:8096" {
		t.Fatalf("listen = %q", c.Listen)
	}
	if c.AuthPerMin != 7 {
		t.Fatalf("auth per min = %d", c.AuthPerMin)
	}
}

// TestSessionKeyFallsBackToTheInsecureDefault: main warns on this exact value,
// so it must stay the one an unset key produces.
func TestSessionKeyFallsBackToTheInsecureDefault(t *testing.T) {
	t.Setenv("TAKAN_SESSION_KEY", "")
	if got := Load().SessionKey; got != "dev-insecure-change-me" {
		t.Fatalf("session key = %q", got)
	}
}

// TestEnvIntIgnoresGarbage: a typo in the env file must not disable a limit.
func TestEnvIntIgnoresGarbage(t *testing.T) {
	t.Setenv("TAKAN_MACHINE_BASH_PER_MIN", "not-a-number")
	if got := Load().MachineBashPerMin; got != 30 {
		t.Fatalf("machine bash per min = %d, want the default", got)
	}
}
