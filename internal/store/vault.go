package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// --- Vault module (password manager + agent grant broker) ---

// VaultItem is a login credential (secrets stored encrypted).
type VaultItem struct {
	ID          string
	UserID      string
	Name        string
	Username    string
	PasswordEnc string
	TOTPEnc     string
	NotesEnc    string
	URLs        []string
	Folder      string
	Tags        []string
	Favorite    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// VaultGrant is an agent request to read secret fields of an item.
type VaultGrant struct {
	ID         string
	UserID     string
	ItemID     string
	MatchURL   string
	MatchQuery string
	Fields     []string // username | password | otp | totp | notes
	Purpose    string
	Status     string // pending | approved | denied | expired | consumed
	Mode       string // once | session
	TTLSeconds int
	ApprovedAt *time.Time
	ExpiresAt  *time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
	// Joined for panel (not always set)
	ItemName string
	ItemURL  string
}

func (s *Store) migrateVault() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS vault_items (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '',
  password_enc TEXT NOT NULL DEFAULT '',
  totp_enc TEXT NOT NULL DEFAULT '',
  notes_enc TEXT NOT NULL DEFAULT '',
  urls_json TEXT NOT NULL DEFAULT '[]',
  folder TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]',
  favorite INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vault_items_user ON vault_items(user_id);
CREATE INDEX IF NOT EXISTS idx_vault_items_user_updated ON vault_items(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS vault_grants (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id TEXT REFERENCES vault_items(id) ON DELETE SET NULL,
  match_url TEXT NOT NULL DEFAULT '',
  match_query TEXT NOT NULL DEFAULT '',
  fields_json TEXT NOT NULL DEFAULT '[]',
  purpose TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  mode TEXT NOT NULL DEFAULT 'once',
  ttl_seconds INTEGER NOT NULL DEFAULT 120,
  approved_at TEXT,
  expires_at TEXT,
  consumed_at TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vault_grants_user ON vault_grants(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_vault_grants_status ON vault_grants(user_id, status);

CREATE TABLE IF NOT EXISTS vault_devices (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  platform TEXT NOT NULL DEFAULT '',
  push_endpoint_enc TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_vault_devices_user ON vault_devices(user_id);

CREATE TABLE IF NOT EXISTS vault_audit (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  action TEXT NOT NULL,
  item_id TEXT NOT NULL DEFAULT '',
  grant_id TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_vault_audit_user ON vault_audit(user_id, at DESC);
`)
	return err
}

func encodeStringSlice(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func decodeStringSlice(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func scanVaultItem(scanner interface {
	Scan(dest ...any) error
}) (VaultItem, error) {
	var it VaultItem
	var urlsJSON, tagsJSON, created, updated string
	var fav int
	err := scanner.Scan(
		&it.ID, &it.UserID, &it.Name, &it.Username,
		&it.PasswordEnc, &it.TOTPEnc, &it.NotesEnc,
		&urlsJSON, &it.Folder, &tagsJSON, &fav, &created, &updated,
	)
	if err != nil {
		return it, err
	}
	it.URLs = decodeStringSlice(urlsJSON)
	it.Tags = decodeStringSlice(tagsJSON)
	it.Favorite = fav != 0
	it.CreatedAt, _ = time.Parse(time.RFC3339, created)
	it.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return it, nil
}

const vaultItemCols = `id, user_id, name, username, password_enc, totp_enc, notes_enc,
  urls_json, folder, tags_json, favorite, created_at, updated_at`

// CreateVaultItem inserts a new vault login.
func (s *Store) CreateVaultItem(ctx context.Context, it VaultItem) (VaultItem, error) {
	if it.ID == "" {
		it.ID = uuid.NewString()
	}
	if it.UserID == "" {
		return it, fmt.Errorf("user_id required")
	}
	if strings.TrimSpace(it.Name) == "" {
		if len(it.URLs) > 0 {
			it.Name = hostFromURL(it.URLs[0])
		}
		if it.Name == "" {
			it.Name = "Login"
		}
	}
	now := time.Now().UTC()
	it.CreatedAt = now
	it.UpdatedAt = now
	fav := 0
	if it.Favorite {
		fav = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO vault_items (id, user_id, name, username, password_enc, totp_enc, notes_enc,
  urls_json, folder, tags_json, favorite, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.UserID, it.Name, it.Username, it.PasswordEnc, it.TOTPEnc, it.NotesEnc,
		encodeStringSlice(it.URLs), it.Folder, encodeStringSlice(it.Tags), fav,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return it, err
	}
	_ = s.AddVaultAudit(ctx, it.UserID, "item_create", it.ID, "", "system", it.Name)
	return it, nil
}

// UpdateVaultItem patches a vault item. When keepSecrets is true, empty passwordEnc/totpEnc/notesEnc keep the previous ciphertext.
func (s *Store) UpdateVaultItem(ctx context.Context, userID, id string, name, username, passwordEnc, totpEnc, notesEnc, folder string, urls, tags []string, favorite *bool, setURLs, setTags bool, keepSecrets bool) (VaultItem, error) {
	cur, err := s.GetVaultItem(ctx, userID, id)
	if err != nil {
		return VaultItem{}, err
	}
	if name != "" {
		cur.Name = name
	}
	cur.Username = username
	cur.Folder = folder
	if keepSecrets {
		if passwordEnc != "" {
			cur.PasswordEnc = passwordEnc
		}
		if totpEnc != "" {
			cur.TOTPEnc = totpEnc
		}
		if notesEnc != "" {
			cur.NotesEnc = notesEnc
		}
	} else {
		cur.PasswordEnc = passwordEnc
		cur.TOTPEnc = totpEnc
		cur.NotesEnc = notesEnc
	}
	if setURLs {
		cur.URLs = urls
	}
	if setTags {
		cur.Tags = tags
	}
	if favorite != nil {
		cur.Favorite = *favorite
	}
	now := time.Now().UTC()
	cur.UpdatedAt = now
	fav := 0
	if cur.Favorite {
		fav = 1
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE vault_items SET name=?, username=?, password_enc=?, totp_enc=?, notes_enc=?,
  urls_json=?, folder=?, tags_json=?, favorite=?, updated_at=?
WHERE id=? AND user_id=?`,
		cur.Name, cur.Username, cur.PasswordEnc, cur.TOTPEnc, cur.NotesEnc,
		encodeStringSlice(cur.URLs), cur.Folder, encodeStringSlice(cur.Tags), fav,
		now.Format(time.RFC3339), id, userID,
	)
	if err != nil {
		return VaultItem{}, err
	}
	_ = s.AddVaultAudit(ctx, userID, "item_update", id, "", "system", cur.Name)
	return cur, nil
}

// UpsertVaultItemByURL updates matching URL host or creates a new item (agent store path).
func (s *Store) UpsertVaultItem(ctx context.Context, it VaultItem, keepSecrets bool) (VaultItem, bool, error) {
	if it.ID != "" {
		out, err := s.UpdateVaultItem(ctx, it.UserID, it.ID, it.Name, it.Username, it.PasswordEnc, it.TOTPEnc, it.NotesEnc, it.Folder, it.URLs, it.Tags, &it.Favorite, true, true, keepSecrets)
		return out, false, err
	}
	// If id empty but urls match existing, update first match
	if len(it.URLs) > 0 {
		found, err := s.FindVaultItemsByURL(ctx, it.UserID, it.URLs[0], 1)
		if err != nil {
			return VaultItem{}, false, err
		}
		if len(found) > 0 {
			out, err := s.UpdateVaultItem(ctx, it.UserID, found[0].ID, it.Name, it.Username, it.PasswordEnc, it.TOTPEnc, it.NotesEnc, it.Folder, it.URLs, it.Tags, &it.Favorite, len(it.URLs) > 0, len(it.Tags) > 0, keepSecrets)
			return out, false, err
		}
	}
	out, err := s.CreateVaultItem(ctx, it)
	return out, true, err
}

func (s *Store) GetVaultItem(ctx context.Context, userID, id string) (VaultItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+vaultItemCols+` FROM vault_items WHERE id=? AND user_id=?`, id, userID)
	it, err := scanVaultItem(row)
	if err == sql.ErrNoRows {
		return VaultItem{}, sql.ErrNoRows
	}
	return it, err
}

func (s *Store) DeleteVaultItem(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM vault_items WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	_ = s.AddVaultAudit(ctx, userID, "item_delete", id, "", "system", "")
	return nil
}

func (s *Store) CountVaultItems(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vault_items WHERE user_id=?`, userID).Scan(&n)
	return n, err
}

// SearchVaultItems filters by query (name/username/url/folder/tags) and optional url host match. Metadata only.
func (s *Store) SearchVaultItems(ctx context.Context, userID, query, urlMatch string, limit int) ([]VaultItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+vaultItemCols+` FROM vault_items WHERE user_id=? ORDER BY favorite DESC, updated_at DESC LIMIT 500`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	q := strings.ToLower(strings.TrimSpace(query))
	hostWant := hostFromURL(urlMatch)
	var out []VaultItem
	for rows.Next() {
		it, err := scanVaultItem(rows)
		if err != nil {
			return nil, err
		}
		if hostWant != "" && !itemMatchesHost(it, hostWant) {
			continue
		}
		if q != "" && !itemMatchesQuery(it, q) {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func (s *Store) FindVaultItemsByURL(ctx context.Context, userID, rawURL string, limit int) ([]VaultItem, error) {
	return s.SearchVaultItems(ctx, userID, "", rawURL, limit)
}

func (s *Store) ListVaultItems(ctx context.Context, userID string, limit int) ([]VaultItem, error) {
	return s.SearchVaultItems(ctx, userID, "", "", limit)
}

func itemMatchesQuery(it VaultItem, q string) bool {
	if strings.Contains(strings.ToLower(it.Name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(it.Username), q) {
		return true
	}
	if strings.Contains(strings.ToLower(it.Folder), q) {
		return true
	}
	for _, t := range it.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	for _, u := range it.URLs {
		if strings.Contains(strings.ToLower(u), q) {
			return true
		}
		if strings.Contains(hostFromURL(u), q) {
			return true
		}
	}
	return false
}

func itemMatchesHost(it VaultItem, host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return true
	}
	for _, u := range it.URLs {
		h := hostFromURL(u)
		if h == host || strings.HasSuffix(h, "."+host) || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	h := strings.ToLower(u.Hostname())
	h = strings.TrimPrefix(h, "www.")
	return h
}

// --- Grants ---

func scanVaultGrant(scanner interface {
	Scan(dest ...any) error
}) (VaultGrant, error) {
	var g VaultGrant
	var fieldsJSON, created string
	var itemID, approved, expires, consumed sql.NullString
	err := scanner.Scan(
		&g.ID, &g.UserID, &itemID, &g.MatchURL, &g.MatchQuery, &fieldsJSON,
		&g.Purpose, &g.Status, &g.Mode, &g.TTLSeconds,
		&approved, &expires, &consumed, &created,
	)
	if err != nil {
		return g, err
	}
	if itemID.Valid {
		g.ItemID = itemID.String
	}
	g.Fields = decodeStringSlice(fieldsJSON)
	if approved.Valid && approved.String != "" {
		t, _ := time.Parse(time.RFC3339, approved.String)
		g.ApprovedAt = &t
	}
	if expires.Valid && expires.String != "" {
		t, _ := time.Parse(time.RFC3339, expires.String)
		g.ExpiresAt = &t
	}
	if consumed.Valid && consumed.String != "" {
		t, _ := time.Parse(time.RFC3339, consumed.String)
		g.ConsumedAt = &t
	}
	g.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return g, nil
}

const vaultGrantCols = `id, user_id, item_id, match_url, match_query, fields_json, purpose, status, mode,
  ttl_seconds, approved_at, expires_at, consumed_at, created_at`

// CreateVaultGrant creates a pending grant. Resolves item_id from match if empty.
func (s *Store) CreateVaultGrant(ctx context.Context, g VaultGrant) (VaultGrant, error) {
	if g.UserID == "" {
		return g, fmt.Errorf("user_id required")
	}
	if g.ID == "" {
		g.ID = uuid.NewString()
	}
	if g.Mode != "session" {
		g.Mode = "once"
	}
	if g.TTLSeconds <= 0 {
		g.TTLSeconds = 120
	}
	if g.TTLSeconds > 3600 {
		g.TTLSeconds = 3600
	}
	if len(g.Fields) == 0 {
		g.Fields = []string{"username", "password"}
	}
	// Normalize fields
	var fields []string
	for _, f := range g.Fields {
		f = strings.ToLower(strings.TrimSpace(f))
		switch f {
		case "username", "password", "otp", "totp", "notes":
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return g, fmt.Errorf("fields must include username, password, otp, totp, and/or notes")
	}
	g.Fields = fields

	if g.ItemID == "" {
		if g.MatchURL != "" {
			found, err := s.FindVaultItemsByURL(ctx, g.UserID, g.MatchURL, 1)
			if err != nil {
				return g, err
			}
			if len(found) > 0 {
				g.ItemID = found[0].ID
			}
		}
		if g.ItemID == "" && g.MatchQuery != "" {
			found, err := s.SearchVaultItems(ctx, g.UserID, g.MatchQuery, "", 1)
			if err != nil {
				return g, err
			}
			if len(found) > 0 {
				g.ItemID = found[0].ID
			}
		}
	}
	if g.ItemID != "" {
		if _, err := s.GetVaultItem(ctx, g.UserID, g.ItemID); err != nil {
			return g, fmt.Errorf("item not found")
		}
	}

	g.Status = "pending"
	now := time.Now().UTC()
	g.CreatedAt = now
	var itemID any
	if g.ItemID != "" {
		itemID = g.ItemID
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO vault_grants (id, user_id, item_id, match_url, match_query, fields_json, purpose, status, mode,
  ttl_seconds, approved_at, expires_at, consumed_at, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL,NULL,NULL,?)`,
		g.ID, g.UserID, itemID, g.MatchURL, g.MatchQuery, encodeStringSlice(g.Fields),
		g.Purpose, g.Status, g.Mode, g.TTLSeconds, now.Format(time.RFC3339),
	)
	if err != nil {
		return g, err
	}
	_ = s.AddVaultAudit(ctx, g.UserID, "grant_create", g.ItemID, g.ID, "agent", g.Purpose)
	return g, nil
}

func (s *Store) GetVaultGrant(ctx context.Context, userID, id string) (VaultGrant, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+vaultGrantCols+` FROM vault_grants WHERE id=? AND user_id=?`, id, userID)
	g, err := scanVaultGrant(row)
	if err != nil {
		return g, err
	}
	// Expire pending/approved if past expires
	g = s.refreshGrantExpiry(ctx, g)
	return g, nil
}

func (s *Store) refreshGrantExpiry(ctx context.Context, g VaultGrant) VaultGrant {
	now := time.Now().UTC()
	if g.Status == "pending" {
		// Pending requests expire after ttl from creation (same window)
		deadline := g.CreatedAt.Add(time.Duration(g.TTLSeconds) * time.Second)
		// Give pending longer: 15 min min or ttl*5
		wait := time.Duration(g.TTLSeconds*5) * time.Second
		if wait < 15*time.Minute {
			wait = 15 * time.Minute
		}
		deadline = g.CreatedAt.Add(wait)
		if now.After(deadline) {
			_ = s.setGrantStatus(ctx, g.UserID, g.ID, "expired", nil, nil, nil)
			g.Status = "expired"
		}
		return g
	}
	if (g.Status == "approved") && g.ExpiresAt != nil && now.After(*g.ExpiresAt) {
		_ = s.setGrantStatus(ctx, g.UserID, g.ID, "expired", g.ApprovedAt, g.ExpiresAt, nil)
		g.Status = "expired"
	}
	return g
}

func (s *Store) setGrantStatus(ctx context.Context, userID, id, status string, approved, expires, consumed *time.Time) error {
	var a, e, c any
	if approved != nil {
		a = approved.UTC().Format(time.RFC3339)
	}
	if expires != nil {
		e = expires.UTC().Format(time.RFC3339)
	}
	if consumed != nil {
		c = consumed.UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE vault_grants SET status=?, approved_at=COALESCE(?, approved_at), expires_at=COALESCE(?, expires_at),
  consumed_at=COALESCE(?, consumed_at) WHERE id=? AND user_id=?`,
		status, a, e, c, id, userID)
	return err
}

// DecideVaultGrant approves or denies a pending grant. On approve, sets expires_at from now+ttl.
func (s *Store) DecideVaultGrant(ctx context.Context, userID, id string, approve bool, itemID string) (VaultGrant, error) {
	g, err := s.GetVaultGrant(ctx, userID, id)
	if err != nil {
		return g, err
	}
	if g.Status != "pending" {
		return g, fmt.Errorf("grant is %s, not pending", g.Status)
	}
	if !approve {
		now := time.Now().UTC()
		if err := s.setGrantStatus(ctx, userID, id, "denied", &now, nil, nil); err != nil {
			return g, err
		}
		_ = s.AddVaultAudit(ctx, userID, "grant_deny", g.ItemID, id, "user", "")
		g.Status = "denied"
		return g, nil
	}
	// Resolve item
	if itemID != "" {
		g.ItemID = itemID
	}
	if g.ItemID == "" {
		return g, fmt.Errorf("approve requires item_id (grant has no matched item)")
	}
	if _, err := s.GetVaultItem(ctx, userID, g.ItemID); err != nil {
		return g, fmt.Errorf("item not found")
	}
	now := time.Now().UTC()
	exp := now.Add(time.Duration(g.TTLSeconds) * time.Second)
	_, err = s.db.ExecContext(ctx, `
UPDATE vault_grants SET status='approved', item_id=?, approved_at=?, expires_at=? WHERE id=? AND user_id=? AND status='pending'`,
		g.ItemID, now.Format(time.RFC3339), exp.Format(time.RFC3339), id, userID)
	if err != nil {
		return g, err
	}
	_ = s.AddVaultAudit(ctx, userID, "grant_approve", g.ItemID, id, "user", g.Purpose)
	g.Status = "approved"
	g.ApprovedAt = &now
	g.ExpiresAt = &exp
	return g, nil
}

// ConsumeVaultGrant marks a once-mode approved grant as consumed. Returns error if not consumable.
func (s *Store) ConsumeVaultGrant(ctx context.Context, userID, id string) (VaultGrant, error) {
	g, err := s.GetVaultGrant(ctx, userID, id)
	if err != nil {
		return g, err
	}
	g = s.refreshGrantExpiry(ctx, g)
	if g.Status == "consumed" {
		return g, nil
	}
	if g.Status != "approved" {
		return g, fmt.Errorf("grant is %s", g.Status)
	}
	if g.Mode == "session" {
		// Session grants stay approved until expiry; no consume.
		return g, nil
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
UPDATE vault_grants SET status='consumed', consumed_at=? WHERE id=? AND user_id=? AND status='approved'`,
		now.Format(time.RFC3339), id, userID)
	if err != nil {
		return g, err
	}
	g.Status = "consumed"
	g.ConsumedAt = &now
	_ = s.AddVaultAudit(ctx, userID, "grant_consume", g.ItemID, id, "agent", "")
	return g, nil
}

// ListVaultGrants lists recent grants; if status filter empty, all; pending first useful for panel.
func (s *Store) ListVaultGrants(ctx context.Context, userID, status string, limit int) ([]VaultGrant, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+vaultGrantCols+` FROM vault_grants WHERE user_id=? AND status=? ORDER BY created_at DESC LIMIT ?`, userID, status, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+vaultGrantCols+` FROM vault_grants WHERE user_id=? ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?`, userID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VaultGrant
	for rows.Next() {
		g, err := scanVaultGrant(rows)
		if err != nil {
			return nil, err
		}
		g = s.refreshGrantExpiry(ctx, g)
		if g.ItemID != "" {
			if it, err := s.GetVaultItem(ctx, userID, g.ItemID); err == nil {
				g.ItemName = it.Name
				if len(it.URLs) > 0 {
					g.ItemURL = it.URLs[0]
				}
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) CountVaultGrantsPending(ctx context.Context, userID string) (int, error) {
	// Refresh pending first is expensive; count pending only
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vault_grants WHERE user_id=? AND status='pending'`, userID).Scan(&n)
	return n, err
}

// AddVaultAudit appends an audit row.
func (s *Store) AddVaultAudit(ctx context.Context, userID, action, itemID, grantID, actor, detail string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO vault_audit (id, user_id, at, action, item_id, grant_id, actor, detail)
VALUES (?,?,?,?,?,?,?,?)`,
		uuid.NewString(), userID, time.Now().UTC().Format(time.RFC3339), action, itemID, grantID, actor, detail)
	return err
}
