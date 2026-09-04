package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// LoginCodeLen is the number of digits in an emailed login code.
const LoginCodeLen = 6

// LoginCodeTTL is how long a code stays usable.
const LoginCodeTTL = 10 * time.Minute

// maxLoginCodeAttempts is how many wrong guesses a single code tolerates
// before it is burned (independent of the HTTP rate limit).
const maxLoginCodeAttempts = 5

// ErrLoginCode is returned for every failure of ConsumeLoginCode. The message
// is deliberately uniform so it cannot be used to probe code state.
var ErrLoginCode = fmt.Errorf("invalid or expired code")

func (s *Store) migrateLoginCodes() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS login_codes (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL COLLATE NOCASE,
  salt TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_login_codes_email ON login_codes(email, expires_at);
`)
	return err
}

// hashLoginCode salts the code so a database read cannot be reversed by
// precomputing the 10^6 possible values.
func hashLoginCode(salt, code string) string {
	sum := sha256.Sum256([]byte(salt + ":" + code))
	return hex.EncodeToString(sum[:])
}

// randomDigits returns a cryptographically random decimal string.
func randomDigits(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + d.Int64()))
	}
	return b.String(), nil
}

// IssueLoginCode generates a single-use login code for email and returns it in
// clear exactly once (only the salted hash is stored). Any previous unused code
// for that address is invalidated, so the newest email is always the valid one.
//
// The caller must never log the returned value.
func (s *Store) IssueLoginCode(ctx context.Context, email string, ttl time.Duration) (string, error) {
	email = normalizeEmail(email)
	if email == "" {
		return "", fmt.Errorf("email required")
	}
	if ttl <= 0 {
		ttl = LoginCodeTTL
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM login_codes WHERE email = ? AND used_at IS NULL`, email); err != nil {
		return "", err
	}
	code, err := randomDigits(LoginCodeLen)
	if err != nil {
		return "", err
	}
	salt, err := randomHex(16)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO login_codes (id, email, salt, code_hash, created_at, expires_at)
VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), email, salt, hashLoginCode(salt, code),
		now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339)); err != nil {
		return "", err
	}
	return code, nil
}

// ConsumeLoginCode validates a code and burns it. Every failure returns
// ErrLoginCode with no detail. A code is also burned after
// maxLoginCodeAttempts wrong guesses.
func (s *Store) ConsumeLoginCode(ctx context.Context, email, code string) error {
	email = normalizeEmail(email)
	code = strings.TrimSpace(code)
	if email == "" || code == "" {
		return ErrLoginCode
	}
	var id, salt, want string
	var attempts int
	var expires string
	err := s.db.QueryRowContext(ctx, `
SELECT id, salt, code_hash, attempts, expires_at FROM login_codes
WHERE email = ? AND used_at IS NULL
ORDER BY created_at DESC, id DESC LIMIT 1`, email).Scan(&id, &salt, &want, &attempts, &expires)
	if err != nil {
		return ErrLoginCode
	}
	exp, perr := time.Parse(time.RFC3339, expires)
	if perr != nil || time.Now().UTC().After(exp) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM login_codes WHERE id = ?`, id)
		return ErrLoginCode
	}
	got := hashLoginCode(salt, code)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		attempts++
		if attempts >= maxLoginCodeAttempts {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM login_codes WHERE id = ?`, id)
		} else {
			_, _ = s.db.ExecContext(ctx, `UPDATE login_codes SET attempts = ? WHERE id = ?`, attempts, id)
		}
		return ErrLoginCode
	}
	// Single use: only the row we just matched, and only if still unused.
	res, err := s.db.ExecContext(ctx,
		`UPDATE login_codes SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return ErrLoginCode
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLoginCode
	}
	return nil
}

// PurgeLoginCodes drops used and expired rows older than the given age.
func (s *Store) PurgeLoginCodes(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_codes WHERE expires_at < ? OR (used_at IS NOT NULL AND used_at < ?)`,
		cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OwnerEmail returns the operator's stored address, or "" when the instance is
// empty or still carries the placeholder from a password-era bootstrap.
func (s *Store) OwnerEmail(ctx context.Context) string {
	u, err := s.Owner(ctx)
	if err != nil || u == nil {
		return ""
	}
	if strings.EqualFold(u.Email, OperatorEmail) || !strings.Contains(u.Email, "@") {
		return ""
	}
	return u.Email
}

// SetOwnerEmail rewrites the operator's address (used when TAKAN_OWNER_EMAIL
// differs from what an older bootstrap stored).
func (s *Store) SetOwnerEmail(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	if email == "" {
		return fmt.Errorf("email required")
	}
	u, err := s.Owner(ctx)
	if err != nil || u == nil {
		return sql.ErrNoRows
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, email, u.ID)
	return err
}
