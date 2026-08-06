package vault

import (
	"context"
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

func TestParseConfigDefaults(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"{}", true},
		{`{"other":1}`, true},
		{`{"require_approval":true}`, true},
		{`{"require_approval":false}`, false},
		{"not-json", true},
	}
	for _, tc := range cases {
		got := ParseConfig(tc.raw)
		if got.RequireApproval != tc.want {
			t.Errorf("ParseConfig(%q).RequireApproval=%v want %v", tc.raw, got.RequireApproval, tc.want)
		}
	}
}

func TestLoadSaveConfig(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "vault-cfg@example.com", "password1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(ctx, st, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireApproval {
		t.Fatal("default should require approval")
	}

	cfg.RequireApproval = false
	if err := SaveConfig(ctx, st, u.ID, cfg); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(ctx, st, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.RequireApproval {
		t.Fatal("expected require_approval=false after save")
	}
}
