package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCrossTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	a, err := st.CreateUserOpts(ctx, "a@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	// Second user needs invite when not AllowOpen — use open for test.
	b, err := st.CreateUserOpts(ctx, "b@example.com", "password2", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsAdmin || !a.InviteUnlimited {
		t.Fatalf("first user should be admin+unlimited: %+v", a)
	}
	if b.IsAdmin {
		t.Fatal("second user should not be admin")
	}

	// Memory isolation
	if err := st.SetMemory(ctx, a.ID, "secret-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMemory(ctx, b.ID, "secret-b"); err != nil {
		t.Fatal(err)
	}
	ca, _, _, _ := st.GetMemory(ctx, a.ID)
	cb, _, _, _ := st.GetMemory(ctx, b.ID)
	if ca != "secret-a" || cb != "secret-b" {
		t.Fatalf("memory leak: a=%q b=%q", ca, cb)
	}

	// People isolation
	pa, err := st.CreatePerson(ctx, Person{UserID: a.ID, Name: "Alice Friend"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.GetPerson(ctx, b.ID, pa.ID)
	if err == nil {
		t.Fatal("user B must not read user A person")
	}
	listB, _ := st.ListPeople(ctx, b.ID, "", 50)
	if len(listB) != 0 {
		t.Fatalf("user B people should be empty, got %d", len(listB))
	}

	// Machines isolation
	ma, _, err := st.CreateMachine(ctx, a.ID, "mac-a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.MachineByUserAndName(ctx, b.ID, "mac-a")
	if err == nil {
		t.Fatal("user B must not see machine mac-a")
	}
	msB, _ := st.ListMachines(ctx, b.ID)
	if len(msB) != 0 {
		t.Fatalf("B machines: %d", len(msB))
	}
	_ = ma

	// Mercadona tables exist in main DB
	if err := st.EnsureMercadonaSchema(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = st.DB().Exec(`INSERT INTO accounts (id, api_token_hash, access_token_enc, customer_id)
		VALUES (?, 'hash-a', 'enc', 'cust')`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(1) FROM accounts WHERE id = ?`, b.ID).Scan(&n)
	if n != 0 {
		t.Fatal("B should not have mercadona account row")
	}
}

func TestInviteQuotaAndUnlimited(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	admin, err := st.CreateUserOpts(ctx, "admin@takan.test", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	// Limited user
	u, err := st.CreateUserOpts(ctx, "u@takan.test", "password2", CreateUserOpts{
		AllowOpen: true, DefaultQuota: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reload to get quota from DB (create sets it)
	u, _ = st.UserByID(ctx, u.ID)
	if u.InviteQuota != 2 {
		t.Fatalf("quota want 2 got %d", u.InviteQuota)
	}

	for i := 0; i < 2; i++ {
		if _, err := st.CreateInvite(ctx, u.ID, "n", 24*time.Hour); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	if _, err := st.CreateInvite(ctx, u.ID, "over", 24*time.Hour); err == nil {
		t.Fatal("expected quota exhausted")
	}

	// Admin grants unlimited
	if err := st.SetUserInvitePolicy(ctx, u.ID, 2, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInvite(ctx, u.ID, "ok", 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	// Consume invite for registration
	inv, err := st.CreateInvite(ctx, admin.ID, "for-c", 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateUserOpts(ctx, "c@takan.test", "password3", CreateUserOpts{
		InviteCode: inv.RawCode, AllowOpen: false, RequireInvite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "c@takan.test" {
		t.Fatal(c.Email)
	}
	// Reuse should fail
	if _, err := st.CreateUserOpts(ctx, "d@takan.test", "password4", CreateUserOpts{
		InviteCode: inv.RawCode, AllowOpen: false,
	}); err == nil {
		t.Fatal("expected used invite rejection")
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
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "o@takan.test", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.IssueRefreshToken(ctx, u.ID, "takan", "mcp", time.Hour)
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
	// Old must be invalid
	if _, _, _, err := st.ConsumeRefreshToken(ctx, raw); err == nil {
		t.Fatal("old refresh should be gone")
	}
}
