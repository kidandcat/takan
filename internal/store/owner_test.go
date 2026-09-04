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

	a, err := st.CreateUserOpts(ctx, "first@takan.test", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateUserOpts(ctx, "second@takan.test", "password2", CreateUserOpts{AllowOpen: true})
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
	if _, err := st.CreateUserOpts(ctx, OperatorEmail, "password1", CreateUserOpts{AllowOpen: true}); err != nil {
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
