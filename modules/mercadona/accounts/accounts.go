// Package accounts manages hosted multi-tenant Mercadona connections.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/kidandcat/takan/modules/mercadona/client"
	"github.com/kidandcat/takan/modules/mercadona/cryptox"
)

// Account is a hosted connection (one Mercadona login + postal code + MCP token).
type Account struct {
	ID         string
	EmailHint  string
	PostalCode string
	Warehouse  string
	CustomerID string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// ConnectResult is returned when a Mercadona account is linked (OAuth authorize).
type ConnectResult struct {
	AccountID  string `json:"account_id"`
	PostalCode string `json:"postal_code"`
	Warehouse  string `json:"warehouse,omitempty"`
	EmailHint  string `json:"email_hint,omitempty"`
}

// Store persists accounts with encrypted Mercadona tokens.
type Store struct {
	db   *sql.DB
	box  *cryptox.Box
	base string // public base URL, e.g. https://mercadona.example.com
}

// New creates an account store. baseURL has no trailing slash.
func New(db *sql.DB, box *cryptox.Box, baseURL string) *Store {
	return &Store{db: db, box: box, base: strings.TrimRight(baseURL, "/")}
}

// Connect logs into Mercadona, sets postal code, stores encrypted tokens, returns a one-time API token.
func (s *Store) Connect(ctx context.Context, email, password, postalCode string) (*ConnectResult, error) {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	postalCode = strings.TrimSpace(postalCode)
	if email == "" || password == "" {
		return nil, fmt.Errorf("email and password required")
	}
	if postalCode == "" {
		return nil, fmt.Errorf("postal_code required")
	}
	if len(postalCode) < 4 || len(postalCode) > 10 {
		return nil, fmt.Errorf("invalid postal_code")
	}

	mc := client.New()
	sess, err := mc.Login(ctx, email, password)
	if err != nil {
		return nil, fmt.Errorf("mercadona login failed: %w", err)
	}
	pc, err := mc.ChangePostalCode(ctx, sess, postalCode)
	if err != nil {
		return nil, fmt.Errorf("set postal code: %w", err)
	}

	accessEnc, err := s.box.Seal(sess.AccessToken)
	if err != nil {
		return nil, err
	}
	refreshEnc, err := s.box.Seal(sess.RefreshToken)
	if err != nil {
		return nil, err
	}

	accountID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	// Placeholder hash — access is via OAuth tokens only (api_token_hash kept for schema).
	tokenHash := hashToken(accountID + ":" + sess.CustomerID)

	emailHint := maskEmail(email)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO accounts (
			id, api_token_hash, email_hint, postal_code, warehouse,
			access_token_enc, refresh_token_enc, customer_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, accountID, tokenHash, emailHint, pc.PostalCode, pc.Warehouse,
		accessEnc, refreshEnc, sess.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("save account: %w", err)
	}

	return &ConnectResult{
		AccountID:  accountID,
		PostalCode: pc.PostalCode,
		Warehouse:  pc.Warehouse,
		EmailHint:  emailHint,
	}, nil
}

// LookupByToken resolves a Bearer API token to an account id.
func (s *Store) LookupByToken(ctx context.Context, apiToken string) (string, error) {
	apiToken = strings.TrimSpace(apiToken)
	if apiToken == "" {
		return "", fmt.Errorf("missing token")
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM accounts WHERE api_token_hash = ?
	`, hashToken(apiToken)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("invalid token")
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// Delete removes an account (and cascade aliases manually).
func (s *Store) Delete(ctx context.Context, accountID string) error {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM grocery_aliases WHERE account_id = ?`, accountID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM grocery_preferred WHERE account_id = ?`, accountID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM grocery_pending WHERE account_id = ?`, accountID)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM grocery_pending WHERE account_id = ?`, accountID)
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

// Get returns public account metadata.
func (s *Store) Get(ctx context.Context, accountID string) (*Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(email_hint,''), COALESCE(postal_code,''), COALESCE(warehouse,''),
		       customer_id, created_at, last_used_at
		FROM accounts WHERE id = ?
	`, accountID).Scan(&a.ID, &a.EmailHint, &a.PostalCode, &a.Warehouse, &a.CustomerID, &a.CreatedAt, &a.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// --- service.AccountStore implementation ---

// LoadSession decrypts Mercadona tokens for the account.
func (s *Store) LoadSession(ctx context.Context, accountID string) (*client.Session, error) {
	var accessEnc, refreshEnc, customerID string
	err := s.db.QueryRowContext(ctx, `
		SELECT access_token_enc, COALESCE(refresh_token_enc,''), customer_id
		FROM accounts WHERE id = ?
	`, accountID).Scan(&accessEnc, &refreshEnc, &customerID)
	if err != nil {
		return nil, err
	}
	access, err := s.box.Open(accessEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt access: %w", err)
	}
	refresh, err := s.box.Open(refreshEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh: %w", err)
	}
	return &client.Session{
		AccessToken:  access,
		RefreshToken: refresh,
		CustomerID:   customerID,
	}, nil
}

// SaveSession encrypts and stores updated Mercadona tokens.
func (s *Store) SaveSession(ctx context.Context, accountID string, sess *client.Session) error {
	accessEnc, err := s.box.Seal(sess.AccessToken)
	if err != nil {
		return err
	}
	refreshEnc, err := s.box.Seal(sess.RefreshToken)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE accounts SET
			access_token_enc = ?,
			refresh_token_enc = ?,
			customer_id = COALESCE(NULLIF(?, ''), customer_id),
			last_used_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, accessEnc, refreshEnc, sess.CustomerID, accountID)
	return err
}

// Touch updates last_used_at.
func (s *Store) Touch(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?`, accountID)
	return err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 1 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}
