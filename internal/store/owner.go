package store

import (
	"context"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// OperatorEmail is the sentinel stored in users.email when the instance is
// bootstrapped with no prior account. Existing databases keep the owner's
// real address. It is a storage detail, not a login identifier.
const OperatorEmail = "operator@local"

const minPasswordLen = 8

// Owner returns the single operator row: earliest admin, else earliest user.
func (s *Store) Owner(ctx context.Context) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, userSelect+`
WHERE id = (
  SELECT id FROM users
  ORDER BY is_admin DESC, created_at ASC, id ASC
  LIMIT 1
)`))
}

// IsOwner reports whether userID is the instance operator.
func (s *Store) IsOwner(ctx context.Context, userID string) bool {
	if userID == "" {
		return false
	}
	o, err := s.Owner(ctx)
	return err == nil && o != nil && o.ID == userID
}

// BootstrapOwner creates the operator on an empty database.
func (s *Store) BootstrapOwner(ctx context.Context, password string) (*User, error) {
	if len(password) < minPasswordLen {
		return nil, fmt.Errorf("password min %d chars", minPasswordLen)
	}
	n, err := s.UserCount(ctx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("instance already initialized")
	}
	return s.CreateUserOpts(ctx, OperatorEmail, password, CreateUserOpts{
		AllowOpen:    true,
		DefaultQuota: 0,
	})
}

// AuthenticatePassword checks the instance password against the owner hash.
func (s *Store) AuthenticatePassword(ctx context.Context, password string) (*User, error) {
	if password == "" {
		return nil, fmt.Errorf("invalid credentials")
	}
	u, err := s.Owner(ctx)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	return u, nil
}

// SetOwnerPassword updates the instance password after verifying current.
// Panel cookies for the owner are dropped; OAuth tokens are left intact.
func (s *Store) SetOwnerPassword(ctx context.Context, current, next string) error {
	if len(next) < minPasswordLen {
		return fmt.Errorf("password min %d chars", minPasswordLen)
	}
	u, err := s.AuthenticatePassword(ctx, current)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), u.ID); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE user_id = ?`, u.ID)
	return nil
}
