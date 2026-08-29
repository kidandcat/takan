package tv

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

func TestParseConfigDefaults(t *testing.T) {
	for _, raw := range []string{"", "{}", "not-json", `{"other":1}`} {
		c := ParseConfig(raw)
		if c.Machine != DefaultMachine || c.Host != DefaultHost {
			t.Fatalf("ParseConfig(%q) machine/host = %s %s", raw, c.Machine, c.Host)
		}
		if c.TokenPath != DefaultTokenPath || c.ClientName != DefaultClientName {
			t.Fatalf("token/client: %s %s", c.TokenPath, c.ClientName)
		}
		if c.Apps["netflix"] != "3201907018807" || c.Apps["youtube"] != "9Ur5IzDKqV.TizenYouTube" {
			t.Fatalf("builtin apps: %+v", c.Apps)
		}
	}
}

func TestParseConfigOverrides(t *testing.T) {
	c := ParseConfig(`{"machine":"office","host":"10.0.0.8","token_path":"/tmp/tv.tok","client_name":"Takan","apps":{"plex":"3201512006785"}}`)
	if c.Machine != "office" || c.Host != "10.0.0.8" {
		t.Fatalf("override: %+v", c)
	}
	if c.Apps["plex"] != "3201512006785" {
		t.Fatal("custom alias missing")
	}
	if c.Apps["netflix"] != "3201907018807" {
		t.Fatal("builtins should merge")
	}
}

func TestParseConfigBlankFieldsKeepDefaults(t *testing.T) {
	c := ParseConfig(`{"machine":"  ","host":""}`)
	if c.Machine != DefaultMachine || c.Host != DefaultHost {
		t.Fatalf("blank should default: %+v", c)
	}
}

func TestResolveApp(t *testing.T) {
	c := DefaultConfig()
	id, err := c.ResolveApp("Netflix")
	if err != nil || id != "3201907018807" {
		t.Fatalf("netflix: %s %v", id, err)
	}
	id, err = c.ResolveApp("hbo max")
	if err != nil || id != "3201601007230" {
		t.Fatalf("hbo: %s %v", id, err)
	}
	id, err = c.ResolveApp("9Ur5IzDKqV.TizenYouTube")
	if err != nil || id != "9Ur5IzDKqV.TizenYouTube" {
		t.Fatalf("raw id: %s %v", id, err)
	}
	id, err = c.ResolveApp("3201907018807")
	if err != nil || id != "3201907018807" {
		t.Fatalf("numeric id: %s %v", id, err)
	}
	if _, err := c.ResolveApp("disney"); err == nil {
		t.Fatal("expected unknown app")
	}
}

func TestParseAppsText(t *testing.T) {
	m, err := ParseAppsText("Plex|3201512006785\n# skip\nSpotify = 3201601007230\n")
	if err != nil {
		t.Fatal(err)
	}
	if m["plex"] != "3201512006785" || m["spotify"] != "3201601007230" {
		t.Fatalf("%+v", m)
	}
	if _, err := ParseAppsText("bad line"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	c := DefaultConfig()
	c.Host = "192.168.68.102; rm -rf /"
	if err := c.Validate(); err == nil {
		t.Fatal("expected bad host")
	}
	c = DefaultConfig()
	c.Machine = "mac;id"
	if err := c.Validate(); err == nil {
		t.Fatal("expected bad machine")
	}
	c = DefaultConfig()
	c.TokenPath = "/tmp/x;evil"
	if err := c.Validate(); err == nil {
		t.Fatal("expected bad token path")
	}
}

func TestLoadSaveConfig(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "tv-cfg@example.com", "password1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(ctx, st, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machine != "mac" {
		t.Fatalf("default machine %q", cfg.Machine)
	}
	cfg.Host = "10.1.2.3"
	if err := SaveConfig(ctx, st, u.ID, cfg); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(ctx, st, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Host != "10.1.2.3" || cfg2.Machine != "mac" {
		t.Fatalf("roundtrip: %+v", cfg2)
	}
	if !strings.Contains(cfg2.AppsText(), "Netflix|3201907018807") {
		t.Fatalf("apps text: %s", cfg2.AppsText())
	}
	mods, err := st.ListModules(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mods {
		if m.ModuleID == "tv" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("tv must be in defaultModuleIDs / ListModules")
	}
}
