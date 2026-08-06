package store

import (
	"context"
	"testing"
)

func TestVaultIsolationAndGrantOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	a, err := st.CreateUserOpts(ctx, "vault-a@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateUserOpts(ctx, "vault-b@example.com", "password2", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}

	item, err := st.CreateVaultItem(ctx, VaultItem{
		UserID: a.ID, Name: "BankSync", Username: "jairo",
		PasswordEnc: "enc-secret", URLs: []string{"https://banksync.example/login"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// B cannot read A's item
	if _, err := st.GetVaultItem(ctx, b.ID, item.ID); err == nil {
		t.Fatal("cross-tenant read should fail")
	}
	listB, _ := st.ListVaultItems(ctx, b.ID, 50)
	if len(listB) != 0 {
		t.Fatalf("B vault should be empty, got %d", len(listB))
	}

	// Search by URL host
	found, err := st.SearchVaultItems(ctx, a.ID, "", "https://www.banksync.example/x", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("search by url: %v n=%d", err, len(found))
	}
	if found[0].PasswordEnc != "enc-secret" {
		t.Fatal("store keeps enc field; tools must not expose it in search")
	}

	// Grant request → approve → consume once
	g, err := st.CreateVaultGrant(ctx, VaultGrant{
		UserID: a.ID, ItemID: item.ID, Fields: []string{"username", "password"},
		Purpose: "login test", Mode: "once", TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "pending" {
		t.Fatalf("status=%s", g.Status)
	}

	g, err = st.DecideVaultGrant(ctx, a.ID, g.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "approved" || g.ExpiresAt == nil {
		t.Fatalf("approve: %+v", g)
	}

	g, err = st.ConsumeVaultGrant(ctx, a.ID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "consumed" {
		t.Fatalf("want consumed, got %s", g.Status)
	}
	// Second consume stays consumed
	g2, err := st.ConsumeVaultGrant(ctx, a.ID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != "consumed" {
		t.Fatalf("second consume status=%s", g2.Status)
	}

	// Auto-approve actor is recorded separately
	gAuto, err := st.CreateVaultGrant(ctx, VaultGrant{
		UserID: a.ID, ItemID: item.ID, Fields: []string{"password"},
		Purpose: "auto path", Mode: "once", TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	gAuto, err = st.DecideVaultGrantAs(ctx, a.ID, gAuto.ID, true, "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if gAuto.Status != "approved" {
		t.Fatalf("auto approve status=%s", gAuto.Status)
	}

	// Session mode does not consume
	gS, err := st.CreateVaultGrant(ctx, VaultGrant{
		UserID: a.ID, ItemID: item.ID, Fields: []string{"password"},
		Mode: "session", TTLSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	gS, err = st.DecideVaultGrant(ctx, a.ID, gS.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	gS, err = st.ConsumeVaultGrant(ctx, a.ID, gS.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gS.Status != "approved" {
		t.Fatalf("session should stay approved, got %s", gS.Status)
	}

	// Deny path
	gD, err := st.CreateVaultGrant(ctx, VaultGrant{UserID: a.ID, ItemID: item.ID, Fields: []string{"password"}})
	if err != nil {
		t.Fatal(err)
	}
	gD, err = st.DecideVaultGrant(ctx, a.ID, gD.ID, false, "")
	if err != nil || gD.Status != "denied" {
		t.Fatalf("deny: %+v err=%v", gD, err)
	}

	// Match by URL when item_id empty
	gU, err := st.CreateVaultGrant(ctx, VaultGrant{
		UserID: a.ID, MatchURL: "https://banksync.example", Fields: []string{"password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gU.ItemID != item.ID {
		t.Fatalf("expected auto-match item, got %q", gU.ItemID)
	}
}
