package store

import (
	"context"
	"testing"
	"time"
)

func newCodeStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, context.Background()
}

func TestLoginCodeHappyPathIsSingleUse(t *testing.T) {
	st, ctx := newCodeStore(t)

	code, err := st.IssueLoginCode(ctx, "Owner@Example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != LoginCodeLen {
		t.Fatalf("code %q should be %d digits", code, LoginCodeLen)
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Fatalf("code must be digits only: %q", code)
		}
	}
	// The clear code is never persisted.
	var stored string
	if err := st.db.QueryRowContext(ctx, `SELECT code_hash FROM login_codes`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == code {
		t.Fatal("code stored in clear")
	}

	// Address matching is case-insensitive.
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", code); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", code); err == nil {
		t.Fatal("a code must not be reusable")
	}
}

func TestLoginCodeRejectsWrongInputs(t *testing.T) {
	st, ctx := newCodeStore(t)
	code, err := st.IssueLoginCode(ctx, "owner@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, email, code string }{
		{"wrong code", "owner@example.com", "000000"},
		{"empty code", "owner@example.com", ""},
		{"wrong address", "someone@example.com", code},
		{"empty address", "", code},
	} {
		if err := st.ConsumeLoginCode(ctx, tc.email, tc.code); err == nil {
			t.Fatalf("%s should fail", tc.name)
		}
	}
	// The real code still works after those failures (below the attempt cap).
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", code); err != nil {
		t.Fatalf("valid code after failures: %v", err)
	}
}

func TestLoginCodeExpires(t *testing.T) {
	st, ctx := newCodeStore(t)
	code, err := st.IssueLoginCode(ctx, "owner@example.com", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// A negative TTL falls back to the default, so expire the row explicitly.
	if _, err := st.db.ExecContext(ctx, `UPDATE login_codes SET expires_at = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", code); err == nil {
		t.Fatal("an expired code must not sign in")
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM login_codes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expired code should be dropped on use, %d rows left", n)
	}
}

func TestIssuingANewCodeInvalidatesThePrevious(t *testing.T) {
	st, ctx := newCodeStore(t)
	first, err := st.IssueLoginCode(ctx, "owner@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.IssueLoginCode(ctx, "owner@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", first); err == nil {
		t.Fatal("superseded code must not work")
	}
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", second); err != nil {
		t.Fatalf("newest code: %v", err)
	}
}

func TestLoginCodeBurnsAfterTooManyAttempts(t *testing.T) {
	st, ctx := newCodeStore(t)
	code, err := st.IssueLoginCode(ctx, "owner@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < maxLoginCodeAttempts; i++ {
		if err := st.ConsumeLoginCode(ctx, "owner@example.com", wrong); err == nil {
			t.Fatalf("guess %d should fail", i)
		}
	}
	if err := st.ConsumeLoginCode(ctx, "owner@example.com", code); err == nil {
		t.Fatal("the code should be burned after the attempt cap")
	}
}

func TestPurgeLoginCodes(t *testing.T) {
	st, ctx := newCodeStore(t)
	if _, err := st.IssueLoginCode(ctx, "owner@example.com", LoginCodeTTL); err != nil {
		t.Fatal(err)
	}
	// Fresh rows survive.
	if n, err := st.PurgeLoginCodes(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("purge fresh: %d %v", n, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE login_codes SET expires_at = ?`,
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PurgeLoginCodes(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("purge stale: %d %v", n, err)
	}
}

func TestMaskEmailNotInStore(t *testing.T) {
	// Sanity: two issues for different addresses do not collide.
	st, ctx := newCodeStore(t)
	a, err := st.IssueLoginCode(ctx, "a@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.IssueLoginCode(ctx, "b@example.com", LoginCodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeLoginCode(ctx, "a@example.com", b); err == nil {
		t.Fatal("codes must not be interchangeable between addresses")
	}
	if err := st.ConsumeLoginCode(ctx, "a@example.com", a); err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeLoginCode(ctx, "b@example.com", b); err != nil {
		t.Fatal(err)
	}
}
