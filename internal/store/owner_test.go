package store

import (
	"context"
	"testing"
	"time"
)

func TestOwnerPicksEarliestAdmin(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	a, err := st.CreateUser(ctx, "first@takan.test", "password1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateUser(ctx, "second@takan.test", "password2")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := st.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ID != a.ID {
		t.Fatalf("owner want %s got %s", a.ID, owner.ID)
	}
	if !st.IsOwner(ctx, a.ID) || st.IsOwner(ctx, b.ID) {
		t.Fatalf("IsOwner a=%v b=%v", st.IsOwner(ctx, a.ID), st.IsOwner(ctx, b.ID))
	}
}

func TestBootstrapOwnerUsesEmail(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.BootstrapOwner(ctx, "not-an-email"); err == nil {
		t.Fatal("expected an email address to be required")
	}
	if _, err := st.BootstrapOwner(ctx, ""); err == nil {
		t.Fatal("expected empty email to be rejected")
	}
	u, err := st.BootstrapOwner(ctx, "Owner@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "owner@example.com" {
		t.Fatalf("email should be normalised: %s", u.Email)
	}
	if !u.IsAdmin {
		t.Fatal("the first user must be the admin/owner")
	}
	if _, err := st.BootstrapOwner(ctx, "second@example.com"); err == nil {
		t.Fatal("expected already initialized")
	}
	if got := st.OwnerEmail(ctx); got != "owner@example.com" {
		t.Fatalf("OwnerEmail: %q", got)
	}
}

func TestOwnerEmailIgnoresLegacyPlaceholder(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// A database bootstrapped in the password era stores the sentinel address;
	// it must not be treated as a destination for login codes.
	if _, err := st.CreateUser(ctx, OperatorEmail, "password1"); err != nil {
		t.Fatal(err)
	}
	if got := st.OwnerEmail(ctx); got != "" {
		t.Fatalf("placeholder must not be usable: %q", got)
	}
	if err := st.SetOwnerEmail(ctx, "Real@Example.com"); err != nil {
		t.Fatal(err)
	}
	if got := st.OwnerEmail(ctx); got != "real@example.com" {
		t.Fatalf("OwnerEmail after adoption: %q", got)
	}
}

func TestOwnerSessionsAndTokensCoexist(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateWebSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	access, _, err := st.IssueAccessToken(ctx, owner.ID, "takan", "mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Existing sessions stay valid across the move to code login.
	if u, err := st.UserByWebSession(ctx, sess); err != nil || u.ID != owner.ID {
		t.Fatalf("web session: %v", err)
	}
	if u, err := st.UserByAccessToken(ctx, access); err != nil || u.ID != owner.ID {
		t.Fatalf("access token: %v", err)
	}
}

// TestOwnerHintWinsOverCreationOrder covers an instance that accumulated more
// than one admin: the account receiving login codes is the operator.
func TestOwnerHintWinsOverCreationOrder(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	stale, err := st.CreateUser(ctx, "stale-admin@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	real, err := st.CreateUser(ctx, "kidandcat@example.com", "password2")
	if err != nil {
		t.Fatal(err)
	}
	// created_at has second granularity, so pin the order explicitly.
	if _, err := st.db.ExecContext(ctx, `UPDATE users SET is_admin = 1, created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339), real.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE users SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), stale.ID); err != nil {
		t.Fatal(err)
	}
	// Without a hint, creation order wins and picks the stale row.
	if owner, _ := st.Owner(ctx); owner.ID != stale.ID {
		t.Fatalf("unhinted owner: %s", owner.ID)
	}
	st.SetOwnerHint("KidAndCat@Example.com")
	owner, err := st.Owner(ctx)
	if err != nil || owner.ID != real.ID {
		t.Fatalf("hinted owner: %+v err=%v", owner, err)
	}
	if !st.IsOwner(ctx, real.ID) || st.IsOwner(ctx, stale.ID) {
		t.Fatal("IsOwner must follow the hint")
	}
	// A hint that matches nobody falls back instead of locking everyone out.
	st.SetOwnerHint("nobody@example.com")
	if owner, _ := st.Owner(ctx); owner.ID != stale.ID {
		t.Fatalf("unmatched hint should fall back: %+v", owner)
	}
}
