package tv

import (
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/agenthub"
)

func TestParseSOAPVolumeMute(t *testing.T) {
	volXML := `<s:Envelope><s:Body><u:GetVolumeResponse><CurrentVolume>10</CurrentVolume></u:GetVolumeResponse></s:Body></s:Envelope>`
	n, ok := parseVolume(volXML)
	if !ok || n != 10 {
		t.Fatalf("volume: %d %v", n, ok)
	}
	muteXML := `<CurrentMute>1</CurrentMute>`
	m, ok := parseMute(muteXML)
	if !ok || !m {
		t.Fatalf("mute: %v %v", m, ok)
	}
	off, ok := parseMute(`<ns:CurrentMute>0</ns:CurrentMute>`)
	if !ok || off {
		t.Fatalf("unmute: %v %v", off, ok)
	}
	if _, ok := parseVolume("nope"); ok {
		t.Fatal("expected no volume")
	}
}

func TestParsePowerAndMAC(t *testing.T) {
	raw := `{"device":{"PowerState":"on","wifiMac":"04:b9:e3:86:cd:c0"}}`
	p, mac := parsePowerAndMAC(raw)
	if p != "on" || mac != "04:B9:E3:86:CD:C0" {
		t.Fatalf("got %q %q", p, mac)
	}
	p, mac = parsePowerAndMAC(`{"PowerState":"Standby"}`)
	if p != "standby" || mac != "" {
		t.Fatalf("standby: %q %q", p, mac)
	}
	p, mac = parsePowerAndMAC("")
	if p != "" || mac != "" {
		t.Fatal("empty")
	}
	if powerOn("on") != true || powerOn("ON") != true || powerOn("off") || powerOn("standby") {
		t.Fatal("powerOn")
	}
}

func TestParseAppStatus(t *testing.T) {
	run, vis, name := parseAppStatus(`{"running":true,"visible":true,"name":"Netflix"}`)
	if !run || !vis || name != "Netflix" {
		t.Fatalf("%v %v %q", run, vis, name)
	}
	run, vis, _ = parseAppStatus(`{"running":"false","visible":0}`)
	if run || vis {
		t.Fatal("expected not running")
	}
}

func TestIntArgAndMuteArgs(t *testing.T) {
	n, ok := intArg(map[string]any{"level": float64(10)}, "level")
	if !ok || n != 10 {
		t.Fatalf("json number: %d %v", n, ok)
	}
	n, ok = intArg(map[string]any{"level": "25"}, "level")
	if !ok || n != 25 {
		t.Fatalf("string: %d %v", n, ok)
	}
	if _, ok := intArg(map[string]any{}, "level"); ok {
		t.Fatal("omit")
	}

	mode, muted, err := parseMuteArgs(map[string]any{})
	if err != nil || mode != "get" {
		t.Fatalf("get: %s %v", mode, err)
	}
	mode, muted, err = parseMuteArgs(map[string]any{"mute": true})
	if err != nil || mode != "set" || !muted {
		t.Fatalf("set true: %s %v %v", mode, muted, err)
	}
	mode, muted, err = parseMuteArgs(map[string]any{"mute": "toggle"})
	if err != nil || mode != "toggle" {
		t.Fatalf("toggle: %s %v", mode, err)
	}
	if _, _, err := parseMuteArgs(map[string]any{"mute": "maybe"}); err == nil {
		t.Fatal("bad mute")
	}
}

func TestNowCommandsAreREST(t *testing.T) {
	status, err := curlStatus(DefaultHost)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status, "app_list") || strings.Contains(status, "samsungtvws") {
		t.Fatal(status)
	}
	for _, ref := range DefaultConfig().UniqueApps() {
		cmd, err := curlApp(DefaultHost, "GET", ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "app_list") || strings.Contains(cmd, "samsungtvws") {
			t.Fatalf("now probe must be REST GET: %s", cmd)
		}
		if !strings.Contains(cmd, "/api/v2/applications/"+ref.ID) {
			t.Fatalf("GET app: %s", cmd)
		}
	}
}

func TestFormatVolumeMute(t *testing.T) {
	got := formatVolume(DefaultConfig(), &agenthub.Result{
		ExitCode: 0,
		Stdout:   `<CurrentVolume>10</CurrentVolume>`,
	}, true, 10)
	if !strings.Contains(got, `"volume": 10`) || !strings.Contains(got, `"requested": 10`) {
		t.Fatal(got)
	}
	mute := formatMute(DefaultConfig(), &agenthub.Result{
		Stdout: `<CurrentMute>1</CurrentMute>`,
	}, "set")
	if !strings.Contains(mute, `"muted": true`) {
		t.Fatal(mute)
	}
}
