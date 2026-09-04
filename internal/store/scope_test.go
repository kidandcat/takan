package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func newOwner(t *testing.T) (*Store, *User, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return st, owner, ctx
}

// TestOwnerScopedData is the single-operator replacement for the old two-tenant
// isolation suite: every row is keyed by the owner, and a stranger id sees
// nothing.
func TestOwnerScopedData(t *testing.T) {
	st, owner, ctx := newOwner(t)
	const stranger = "not-a-user"

	if !owner.IsAdmin {
		t.Fatalf("the bootstrapped owner must be the admin: %+v", owner)
	}

	p, err := st.CreatePerson(ctx, Person{UserID: owner.ID, Name: "Alice Friend"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPerson(ctx, stranger, p.ID); err == nil {
		t.Fatal("people are owner-scoped")
	}

	h, w := 179.0, 87.0
	if _, err := st.UpsertHealthProfile(ctx, owner.ID, &h, &w, strPtr("private")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertHealthLog(ctx, owner.ID, "2026-07-17", &w, nil, nil, nil, nil, nil, strPtr("hurt calf"), ""); err != nil {
		t.Fatal(err)
	}
	logs, _ := st.ListHealthLog(ctx, stranger, "", "", 10)
	if len(logs) != 0 {
		t.Fatalf("health log is owner-scoped, got %d rows", len(logs))
	}
	logA, err := st.GetHealthLog(ctx, owner.ID, "2026-07-17")
	if err != nil || logA.Notes != "hurt calf" {
		t.Fatalf("owner log: %+v err=%v", logA, err)
	}

	mac, _, err := st.CreateMachine(ctx, owner.ID, "mac")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MachineByUserAndName(ctx, stranger, "mac"); err == nil {
		t.Fatal("machines are owner-scoped")
	}
	d, err := st.CreateDisplay(ctx, owner.ID, "oficina", mac.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.DefaultDisplay(ctx, owner.ID)
	if err != nil || got.ID != d.ID {
		t.Fatalf("default display: %+v err=%v", got, err)
	}
}

func TestDroppedTablesAreGone(t *testing.T) {
	st, _, _ := newOwner(t)
	for _, table := range []string{
		"bots", "bot_chats", "bot_deliveries", "bot_jobs", "bot_provision_tickets",
		"telegram_channels", "telegram_channel_chats", "telegram_attachments",
		"telegram_settings", "runtime_bundles", "invites", "mcp_tokens",
	} {
		var n int
		err := st.DB().QueryRow(
			`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("table %s should have been dropped", table)
		}
	}
}

func TestDefaultModulesMatchTheCatalog(t *testing.T) {
	st, owner, ctx := newOwner(t)
	mods, err := st.ListModules(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range mods {
		seen[m.ModuleID] = true
	}
	for _, want := range []string{"assistant", "machine", "vault"} {
		if !seen[want] {
			t.Fatalf("module %s missing from %v", want, seen)
		}
	}
	for _, gone := range []string{"bots", "telegram"} {
		if seen[gone] {
			t.Fatalf("module %s should be retired", gone)
		}
	}
}

func TestForeignKeysOn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Writer must reject orphan people rows when FKs are on.
	_, err = st.DB().Exec(`
INSERT INTO people (id, user_id, name, aliases, relationship, context, notes, tags, birthday, email, phone, contact, photo, created_at, updated_at)
VALUES ('p1', 'no-such-user', 'X', '[]', '', '', '', '[]', '', '', '', '{}', '', '2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected FK violation for people.user_id")
	}
}

func TestOAuthRefreshRotation(t *testing.T) {
	st, owner, ctx := newOwner(t)
	raw, err := st.IssueRefreshToken(ctx, owner.ID, "takan", "mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, newRaw, err := st.RotateRefreshToken(ctx, raw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newRaw == "" || newRaw == raw {
		t.Fatal("expected new refresh token")
	}
	if _, _, _, err := st.ConsumeRefreshToken(ctx, raw); err == nil {
		t.Fatal("old refresh should be gone")
	}
}
