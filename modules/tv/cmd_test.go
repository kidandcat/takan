package tv

import (
	"strings"
	"testing"
)

func TestCurlStatus(t *testing.T) {
	cmd, err := curlStatus(DefaultHost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "curl -sS -m 8") {
		t.Fatalf("expected short curl: %s", cmd)
	}
	if !strings.Contains(cmd, "http://192.168.68.102:8001/api/v2/") {
		t.Fatalf("REST URL: %s", cmd)
	}
	if strings.Contains(cmd, "8002") {
		t.Fatal("status must be REST :8001, not websocket")
	}
}

func TestCurlApp(t *testing.T) {
	launch, err := curlApp(DefaultHost, "POST", "3201907018807")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(launch, "-X POST") || !strings.Contains(launch, "/applications/3201907018807") {
		t.Fatalf("launch: %s", launch)
	}
	closeCmd, err := curlApp(DefaultHost, "DELETE", "9Ur5IzDKqV.TizenYouTube")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(closeCmd, "-X DELETE") || !strings.Contains(closeCmd, "TizenYouTube") {
		t.Fatalf("close: %s", closeCmd)
	}
	if _, err := curlApp("not a host", "POST", "1"); err == nil {
		t.Fatal("bad host")
	}
	if _, err := curlApp(DefaultHost, "POST", "id;rm"); err == nil {
		t.Fatal("bad app id")
	}
}

func TestPyKeyAndText(t *testing.T) {
	cmd, err := pyKey(DefaultHost, DefaultTokenPath, DefaultClientName, "home")
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{"python3 -c", "samsungtvws", "192.168.68.102", "8002", DefaultTokenPath, "Gamma", "timeout=8", "KEY_HOME"} {
		if !strings.Contains(cmd, need) {
			t.Fatalf("missing %q in %s", need, cmd)
		}
	}
	textCmd, err := pyText(DefaultHost, DefaultTokenPath, DefaultClientName, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textCmd, "send_text") || !strings.Contains(textCmd, "hello") {
		t.Fatalf("text: %s", textCmd)
	}
	if _, err := pyText(DefaultHost, DefaultTokenPath, DefaultClientName, ""); err == nil {
		t.Fatal("empty text")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"home":     "KEY_HOME",
		"KEY_HOME": "KEY_HOME",
		"enter":    "KEY_ENTER",
		"back":     "KEY_RETURN",
		"volup":    "KEY_VOLUP",
		"power":    "KEY_POWER",
	}
	for in, want := range cases {
		got, err := NormalizeKey(in)
		if err != nil || got != want {
			t.Fatalf("%q -> %q %v want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeKey("KEY_HOME;reboot"); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := NormalizeKey(""); err == nil {
		t.Fatal("empty key")
	}
}

func TestValidHost(t *testing.T) {
	if err := validHost("192.168.68.102"); err != nil {
		t.Fatal(err)
	}
	if err := validHost("tv.local"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "192.168.68.102;id", "$(id)", "http://x", "1.2.3.4:8001", "fe80::1"} {
		if err := validHost(bad); err == nil {
			t.Fatalf("expected reject %q", bad)
		}
	}
}

func TestShellSingle(t *testing.T) {
	if got := shellSingle("abc"); got != "'abc'" {
		t.Fatal(got)
	}
	if got := shellSingle("a'b"); !strings.Contains(got, `'\''`) {
		t.Fatal(got)
	}
}
