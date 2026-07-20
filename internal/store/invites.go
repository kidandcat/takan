package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultInviteQuota is used when CreateUser does not set a quota.
const DefaultInviteQuota = 5

// Invite is a one-time registration code issued by a user.
type Invite struct {
	ID        string
	CreatedBy string
	Note      string
	ExpiresAt *time.Time
	UsedBy    string
	UsedAt    *time.Time
	CreatedAt time.Time
	// RawCode is only set when the invite is first created (never stored).
	RawCode string
}

// InviteQuotaInfo is the panel-facing invite budget for a user.
type InviteQuotaInfo struct {
	Quota     int
	Unlimited bool
	Created   int // total invites ever issued
	Remaining int // -1 when unlimited
}

// CreateInvite issues a new invite for creatorID if under quota.
// Returns the invite with RawCode set once.
func (s *Store) CreateInvite(ctx context.Context, creatorID, note string, ttl time.Duration) (*Invite, error) {
	u, err := s.UserByID(ctx, creatorID)
	if err != nil {
		return nil, err
	}
	info, err := s.InviteQuota(ctx, creatorID)
	if err != nil {
		return nil, err
	}
	if !info.Unlimited && info.Remaining <= 0 {
		return nil, fmt.Errorf("invite quota exhausted (%d/%d used)", info.Created, info.Quota)
	}
	raw, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	// Human-friendly: no hex confusion — use raw hex as code (32 chars).
	code := raw
	id := uuid.NewString()
	now := time.Now().UTC()
	var exp any
	var expT *time.Time
	if ttl > 0 {
		t := now.Add(ttl)
		expT = &t
		exp = t.Format(time.RFC3339)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO invites (id, code_hash, created_by, note, expires_at, created_at)
VALUES (?,?,?,?,?,?)`,
		id, hashToken(code), creatorID, strings.TrimSpace(note), exp, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	_ = u // loaded for existence
	return &Invite{
		ID: id, CreatedBy: creatorID, Note: strings.TrimSpace(note),
		ExpiresAt: expT, CreatedAt: now, RawCode: code,
	}, nil
}

// InviteQuota returns budget stats for userID.
func (s *Store) InviteQuota(ctx context.Context, userID string) (*InviteQuotaInfo, error) {
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM invites WHERE created_by = ?`, userID).Scan(&n)
	if err != nil {
		return nil, err
	}
	info := &InviteQuotaInfo{
		Quota:     u.InviteQuota,
		Unlimited: u.InviteUnlimited,
		Created:   n,
	}
	if u.InviteUnlimited {
		info.Remaining = -1
	} else {
		info.Remaining = u.InviteQuota - n
		if info.Remaining < 0 {
			info.Remaining = 0
		}
	}
	return info, nil
}

// ListInvites returns invites created by userID (newest first).
func (s *Store) ListInvites(ctx context.Context, userID string) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, created_by, COALESCE(note,''), expires_at, COALESCE(used_by,''), used_at, created_at
FROM invites WHERE created_by = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var exp, usedAt sql.NullString
		var created string
		if err := rows.Scan(&inv.ID, &inv.CreatedBy, &inv.Note, &exp, &inv.UsedBy, &usedAt, &created); err != nil {
			return nil, err
		}
		if exp.Valid && exp.String != "" {
			t, _ := time.Parse(time.RFC3339, exp.String)
			inv.ExpiresAt = &t
		}
		if usedAt.Valid && usedAt.String != "" {
			t, _ := time.Parse(time.RFC3339, usedAt.String)
			inv.UsedAt = &t
		}
		inv.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ConsumeInvite validates a raw invite code and marks it used by newUserID.
// Must be called inside the same logical registration (after user insert).
func (s *Store) ConsumeInvite(ctx context.Context, rawCode, newUserID string) error {
	rawCode = strings.TrimSpace(rawCode)
	if rawCode == "" {
		return fmt.Errorf("invite code required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	var exp sql.NullString
	var usedBy sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT id, expires_at, used_by FROM invites WHERE code_hash = ?`, hashToken(rawCode)).
		Scan(&id, &exp, &usedBy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("invalid invite code")
	}
	if err != nil {
		return err
	}
	if usedBy.Valid && usedBy.String != "" {
		return fmt.Errorf("invite already used")
	}
	if exp.Valid && exp.String != "" {
		t, _ := time.Parse(time.RFC3339, exp.String)
		if time.Now().UTC().After(t) {
			return fmt.Errorf("invite expired")
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
UPDATE invites SET used_by = ?, used_at = ? WHERE id = ? AND (used_by IS NULL OR used_by = '')`,
		newUserID, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("invite already used")
	}
	return tx.Commit()
}

// PeekInvite validates a code without consuming (for pre-check).
func (s *Store) PeekInvite(ctx context.Context, rawCode string) error {
	rawCode = strings.TrimSpace(rawCode)
	if rawCode == "" {
		return fmt.Errorf("invite code required")
	}
	var exp sql.NullString
	var usedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT expires_at, used_by FROM invites WHERE code_hash = ?`, hashToken(rawCode)).
		Scan(&exp, &usedBy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("invalid invite code")
	}
	if err != nil {
		return err
	}
	if usedBy.Valid && usedBy.String != "" {
		return fmt.Errorf("invite already used")
	}
	if exp.Valid && exp.String != "" {
		t, _ := time.Parse(time.RFC3339, exp.String)
		if time.Now().UTC().After(t) {
			return fmt.Errorf("invite expired")
		}
	}
	return nil
}

// SetUserInvitePolicy updates quota / unlimited / admin for a user (admin only at call site).
func (s *Store) SetUserInvitePolicy(ctx context.Context, userID string, quota int, unlimited, isAdmin bool) error {
	if quota < 0 {
		quota = 0
	}
	un, ad := 0, 0
	if unlimited {
		un = 1
	}
	if isAdmin {
		ad = 1
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE users SET invite_quota = ?, invite_unlimited = ?, is_admin = ? WHERE id = ?`,
		quota, un, ad, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListUsersForAdmin returns basic user rows for the admin invites panel.
func (s *Store) ListUsersForAdmin(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, email, password_hash, created_at, invite_quota, invite_unlimited, is_admin
FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created string
		var un, ad int
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &created, &u.InviteQuota, &un, &ad); err != nil {
			return nil, err
		}
		u.InviteUnlimited = un != 0
		u.IsAdmin = ad != 0
		u.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserCount returns total registered users.
func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&n)
	return n, err
}

// RevokeUnusedInvite deletes an unused invite owned by userID.
func (s *Store) RevokeUnusedInvite(ctx context.Context, userID, inviteID string) error {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM invites WHERE id = ? AND created_by = ? AND (used_by IS NULL OR used_by = '')`,
		inviteID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
