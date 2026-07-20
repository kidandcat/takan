// Package mercadona is the Mercadona cart module for Takan (API client, store, tools).
package mercadona

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/modules/mercadona/accounts"
	"github.com/kidandcat/takan/modules/mercadona/client"
	"github.com/kidandcat/takan/modules/mercadona/cryptox"
	"github.com/kidandcat/takan/modules/mercadona/service"
	"github.com/kidandcat/takan/modules/mercadona/store"
)

// OpenDB opens/migrates the Mercadona SQLite database.
func OpenDB(path string) (*sql.DB, error) {
	return store.Open(path)
}

// Box is AES-GCM encryption for tokens at rest.
type Box = cryptox.Box

// NewBox derives a key from secret.
func NewBox(secret string) (*Box, error) {
	return cryptox.New(secret)
}

// AccountStore persists encrypted Mercadona sessions.
type AccountStore struct {
	inner *accounts.Store
}

// NewAccountStore creates a multi-tenant token store.
func NewAccountStore(db *sql.DB, box *Box, baseURL string) *AccountStore {
	return &AccountStore{inner: accounts.New(db, box, baseURL)}
}

// Service is a cart façade scoped to one account id.
type Service = service.Service

// NewService returns a Service for accountID (e.g. host user id).
func NewService(db *sql.DB, accountID string, as *AccountStore) *Service {
	return service.New(db, accountID).WithAccountStore(as.inner)
}

// LinkAccount logs into Mercadona and upserts encrypted tokens under accountID.
func LinkAccount(ctx context.Context, db *sql.DB, box *Box, accountID, email, password, postal string) error {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	postal = strings.TrimSpace(postal)
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || email == "" || password == "" || postal == "" {
		return fmt.Errorf("account, email, password and postal code required")
	}
	mc := client.New()
	sess, err := mc.Login(ctx, email, password)
	if err != nil {
		return fmt.Errorf("mercadona login: %w", err)
	}
	pc, err := mc.ChangePostalCode(ctx, sess, postal)
	if err != nil {
		return fmt.Errorf("set postal code: %w", err)
	}
	accessEnc, err := box.Seal(sess.AccessToken)
	if err != nil {
		return err
	}
	refreshEnc, err := box.Seal(sess.RefreshToken)
	if err != nil {
		return err
	}
	emailHint := maskEmail(email)
	tokenHash := "sdk:" + accountID
	_, err = db.ExecContext(ctx, `
INSERT INTO accounts (
  id, api_token_hash, email_hint, postal_code, warehouse,
  access_token_enc, refresh_token_enc, customer_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  api_token_hash = excluded.api_token_hash,
  email_hint = excluded.email_hint,
  postal_code = excluded.postal_code,
  warehouse = excluded.warehouse,
  access_token_enc = excluded.access_token_enc,
  refresh_token_enc = excluded.refresh_token_enc,
  customer_id = excluded.customer_id,
  last_used_at = CURRENT_TIMESTAMP
`, accountID, tokenHash, emailHint, pc.PostalCode, pc.Warehouse,
		accessEnc, refreshEnc, sess.CustomerID)
	if err != nil {
		return fmt.Errorf("save account: %w", err)
	}
	return nil
}

// UnlinkAccount removes session + grocery state for accountID.
func UnlinkAccount(ctx context.Context, db *sql.DB, accountID string) error {
	_, _ = db.ExecContext(ctx, `DELETE FROM grocery_aliases WHERE account_id = ?`, accountID)
	_, _ = db.ExecContext(ctx, `DELETE FROM grocery_preferred WHERE account_id = ?`, accountID)
	_, _ = db.ExecContext(ctx, `DELETE FROM grocery_pending WHERE account_id = ?`, accountID)
	_, err := db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, accountID)
	return err
}

// HasSession reports whether tokens exist for accountID.
func HasSession(ctx context.Context, db *sql.DB, accountID string) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(1) FROM accounts WHERE id = ?`, accountID).Scan(&n)
	return n > 0
}

func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 1 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// Re-export common result types for hosts that prefer not to import internal/.
type (
	AddResult = service.AddResult
	SearchHit = service.SearchHit
	Alias     = service.Alias
	Preferred = service.Preferred
	Cart      = client.Cart
	Product   = client.Product
)
