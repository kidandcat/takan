package store

import (
	"context"
	"fmt"
	"strings"
)

// OperatorEmail is the sentinel stored in users.email when the instance is
// bootstrapped with no prior account. Existing databases keep the owner's
// real address. It is a storage detail, not a login identifier.
const OperatorEmail = "operator@local"

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

// BootstrapOwner creates the operator on an empty database, owned by email.
// Login is by emailed one-time code, so no password is ever chosen: the row
// gets an unusable random hash purely to satisfy the users schema.
func (s *Store) BootstrapOwner(ctx context.Context, email string) (*User, error) {
	email = normalizeEmail(email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("owner email required")
	}
	n, err := s.UserCount(ctx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("instance already initialized")
	}
	unusable, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	return s.CreateUserOpts(ctx, email, unusable, CreateUserOpts{
		AllowOpen:    true,
		DefaultQuota: 0,
	})
}
