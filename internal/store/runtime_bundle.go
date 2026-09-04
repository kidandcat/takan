package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RuntimeBundle is the sealed "brain" a freshly provisioned bot needs: the grok
// CLI credentials and config, the Groq API key and the daemon's base config plus
// workspace guide. One bundle per account.
//
// The whole payload is sealed with the vault's cryptox.Box exactly like a
// channel credential, so nothing readable is at rest and nothing is ever echoed
// back to the panel. Only the non-secret metadata below is queryable.
type RuntimeBundle struct {
	ID     string
	UserID string
	// GrokVersion pins the CLI version the source machine runs, so a target
	// gets the same one instead of whatever is latest today.
	GrokVersion string
	// SourceName / SourceSlug / SourceDataDir are the instance identity the
	// bundle was templated against; they are recorded for display only.
	SourceName    string
	SourceSlug    string
	SourceDataDir string
	// PayloadEnc is the sealed JSON blob of the components.
	PayloadEnc string
	Components []BundleComponent
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// BundleComponent names one file inside the bundle. Only the label and the
// plaintext size are stored in the clear; the bytes live in PayloadEnc.
type BundleComponent struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

func (s *Store) migrateRuntimeBundles() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS runtime_bundles (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  grok_version TEXT NOT NULL DEFAULT '',
  source_name TEXT NOT NULL DEFAULT '',
  source_slug TEXT NOT NULL DEFAULT '',
  source_data_dir TEXT NOT NULL DEFAULT '',
  payload_enc TEXT NOT NULL,
  components TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id)
);`)
	return err
}

// SaveRuntimeBundle stores (or replaces) the account's bundle. The caller has
// already sealed PayloadEnc; this layer never sees plaintext.
func (s *Store) SaveRuntimeBundle(ctx context.Context, b *RuntimeBundle) error {
	comps, err := json.Marshal(b.Components)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	existing, err := s.RuntimeBundle(ctx, b.UserID)
	if err != nil {
		return err
	}
	created := now
	id := uuid.NewString()
	if existing != nil {
		created = existing.CreatedAt
		id = existing.ID
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO runtime_bundles (id, user_id, grok_version, source_name, source_slug, source_data_dir,
                             payload_enc, components, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(user_id) DO UPDATE SET
  grok_version = excluded.grok_version,
  source_name = excluded.source_name,
  source_slug = excluded.source_slug,
  source_data_dir = excluded.source_data_dir,
  payload_enc = excluded.payload_enc,
  components = excluded.components,
  updated_at = excluded.updated_at`,
		id, b.UserID, strings.TrimSpace(b.GrokVersion), b.SourceName, b.SourceSlug, b.SourceDataDir,
		b.PayloadEnc, string(comps), created.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return err
	}
	b.ID, b.CreatedAt, b.UpdatedAt = id, created, now
	return nil
}

// RuntimeBundle returns the account's bundle, or (nil, nil) when none exists.
func (s *Store) RuntimeBundle(ctx context.Context, userID string) (*RuntimeBundle, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, user_id, grok_version, source_name, source_slug, source_data_dir,
       payload_enc, components, created_at, updated_at
FROM runtime_bundles WHERE user_id = ?`, userID)
	var b RuntimeBundle
	var comps, created, updated string
	if err := row.Scan(&b.ID, &b.UserID, &b.GrokVersion, &b.SourceName, &b.SourceSlug,
		&b.SourceDataDir, &b.PayloadEnc, &comps, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(comps), &b.Components); err != nil {
		b.Components = nil // safe-ignore: metadata only; a corrupt list must not hide the bundle
	}
	b.CreatedAt, _ = time.Parse(time.RFC3339, created) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
	b.UpdatedAt, _ = time.Parse(time.RFC3339, updated) // safe-ignore: stored RFC3339; a malformed value degrades to the zero time
	return &b, nil
}

// DeleteRuntimeBundle removes the account's bundle.
func (s *Store) DeleteRuntimeBundle(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_bundles WHERE user_id = ?`, userID)
	return err
}
