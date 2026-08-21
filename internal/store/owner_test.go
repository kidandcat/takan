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

func TestBootstrapOwnerOnce(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.BootstrapOwner(ctx, "short"); err == nil {
		t.Fatal("expected min password length")
	}
	u, err := st.BootstrapOwner(ctx, "instance-secret")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != OperatorEmail {
		t.Fatalf("email: %s", u.Email)
	}
	if _, err := st.BootstrapOwner(ctx, "instance-secret2"); err == nil {
		t.Fatal("expected already initialized")
	}
	got, err := st.AuthenticatePassword(ctx, "instance-secret")
	if err != nil || got.ID != u.ID {
		t.Fatalf("auth: %+v err=%v", got, err)
	}
}

func TestAuthenticatePasswordOwnerOnly(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	owner, err := st.CreateUserOpts(ctx, "kidandcat@example.com", "owner-pass-1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	extra, err := st.CreateUserOpts(ctx, "guest@example.com", "guest-pass-1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.AuthenticatePassword(ctx, "owner-pass-1")
	if err != nil || got.ID != owner.ID {
		t.Fatalf("owner password: %+v err=%v", got, err)
	}
	if _, err := st.AuthenticatePassword(ctx, "guest-pass-1"); err == nil {
		t.Fatal("extra user password must not unlock the instance")
	}
	// email+password helper also ignores email and only accepts the owner secret
	got, err = st.Authenticate(ctx, extra.Email, "owner-pass-1")
	if err != nil || got.ID != owner.ID {
		t.Fatalf("Authenticate should ignore email: %+v err=%v", got, err)
	}
	if _, err := st.Authenticate(ctx, extra.Email, "guest-pass-1"); err == nil {
		t.Fatal("Authenticate must not accept extra user password")
	}

	tok, exp, err := st.IssueAccessToken(ctx, extra.ID, "takan", "mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if exp.IsZero() {
		t.Fatal("expected expiry")
	}
	u, err := st.UserByAccessToken(ctx, tok)
	if err != nil || u.ID != extra.ID {
		t.Fatalf("extra user OAuth token must still resolve: %+v err=%v", u, err)
	}
}

func TestSetOwnerPasswordDropsWebSessionsOnly(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	owner, err := st.BootstrapOwner(ctx, "old-password")
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

	if err := st.SetOwnerPassword(ctx, "old-password", "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByWebSession(ctx, sess); err == nil {
		t.Fatal("panel session should be gone")
	}
	if _, err := st.UserByAccessToken(ctx, access); err != nil {
		t.Fatalf("OAuth access must survive password change: %v", err)
	}
	if _, err := st.AuthenticatePassword(ctx, "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticatePassword(ctx, "old-password"); err == nil {
		t.Fatal("old password must fail")
	}
}
