package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// --- SIP module (phone gateways → Grok Voice via central proxy) ---

type SIPSettings struct {
	UserID         string
	XAIAPIKeyEnc   string
	Voice          string
	Instructions   string
	AutoAnswer     bool
	AudioRate      int
	BridgeMode     string // realtime | sip (sip UAC planned)
	UpdatedAt      time.Time
	HasKey         bool // true when encrypted key is non-empty
}

type SIPDevice struct {
	ID        string
	UserID    string
	Name      string // slug shown as device_id to phones
	SimE164   string
	LastSeen  *time.Time
	CreatedAt time.Time
}

func (s *Store) migrateSIP() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sip_settings (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  xai_api_key_enc TEXT NOT NULL DEFAULT '',
  voice TEXT NOT NULL DEFAULT 'eve',
  instructions TEXT NOT NULL DEFAULT '',
  auto_answer INTEGER NOT NULL DEFAULT 1,
  audio_rate INTEGER NOT NULL DEFAULT 16000,
  bridge_mode TEXT NOT NULL DEFAULT 'realtime',
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sip_devices (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  sim_e164 TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL UNIQUE,
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_sip_devices_user ON sip_devices(user_id);
`)
	return err
}

func (s *Store) GetSIPSettings(ctx context.Context, userID string) (SIPSettings, bool, error) {
	var st SIPSettings
	var auto int
	var updated string
	err := s.db.QueryRowContext(ctx, `
SELECT user_id, xai_api_key_enc, voice, instructions, auto_answer, audio_rate, bridge_mode, updated_at
FROM sip_settings WHERE user_id = ?`, userID).
		Scan(&st.UserID, &st.XAIAPIKeyEnc, &st.Voice, &st.Instructions, &auto, &st.AudioRate, &st.BridgeMode, &updated)
	if err == sql.ErrNoRows {
		return SIPSettings{}, false, nil
	}
	if err != nil {
		return SIPSettings{}, false, err
	}
	st.AutoAnswer = auto != 0
	st.HasKey = st.XAIAPIKeyEnc != ""
	st.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	if st.Voice == "" {
		st.Voice = "eve"
	}
	if st.AudioRate == 0 {
		st.AudioRate = 16000
	}
	if st.BridgeMode == "" {
		st.BridgeMode = "realtime"
	}
	return st, true, nil
}

// SaveSIPSettings upserts settings. Pass empty xaiAPIKeyEnc to keep previous key when row exists
// (caller should load+merge). Empty enc on create is allowed (configure devices first).
func (s *Store) SaveSIPSettings(ctx context.Context, userID, xaiAPIKeyEnc, voice, instructions, bridgeMode string, autoAnswer bool, audioRate int) error {
	if voice == "" {
		voice = "eve"
	}
	if bridgeMode == "" {
		bridgeMode = "realtime"
	}
	if audioRate != 8000 && audioRate != 16000 && audioRate != 24000 {
		audioRate = 16000
	}
	auto := 0
	if autoAnswer {
		auto = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sip_settings (user_id, xai_api_key_enc, voice, instructions, auto_answer, audio_rate, bridge_mode, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(user_id) DO UPDATE SET
  xai_api_key_enc = CASE WHEN excluded.xai_api_key_enc = '' THEN sip_settings.xai_api_key_enc ELSE excluded.xai_api_key_enc END,
  voice = excluded.voice,
  instructions = excluded.instructions,
  auto_answer = excluded.auto_answer,
  audio_rate = excluded.audio_rate,
  bridge_mode = excluded.bridge_mode,
  updated_at = excluded.updated_at`,
		userID, xaiAPIKeyEnc, voice, instructions, auto, audioRate, bridgeMode, now)
	return err
}

func (s *Store) ClearSIPSettings(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sip_settings WHERE user_id = ?`, userID)
	return err
}

// CreateSIPDevice returns (device, rawToken, error). Token shown once.
func (s *Store) CreateSIPDevice(ctx context.Context, userID, name, simE164 string) (*SIPDevice, string, error) {
	name = normalizeName(name)
	if name == "" {
		return nil, "", fmt.Errorf("name required")
	}
	raw, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO sip_devices (id, user_id, name, sim_e164, token_hash, created_at) VALUES (?,?,?,?,?,?)`,
		id, userID, name, trimSpace(simE164), hashToken(raw), now.Format(time.RFC3339))
	if err != nil {
		return nil, "", fmt.Errorf("create sip device: %w", err)
	}
	return &SIPDevice{ID: id, UserID: userID, Name: name, SimE164: trimSpace(simE164), CreatedAt: now}, raw, nil
}

func (s *Store) SIPDeviceByToken(ctx context.Context, raw string) (*SIPDevice, error) {
	if raw == "" {
		return nil, sql.ErrNoRows
	}
	var d SIPDevice
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, name, sim_e164, last_seen_at, created_at FROM sip_devices WHERE token_hash = ?`,
		hashToken(raw)).Scan(&d.ID, &d.UserID, &d.Name, &d.SimE164, &last, &created)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339, last.String)
		d.LastSeen = &t
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &d, nil
}

func (s *Store) TouchSIPDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sip_devices SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) UpdateSIPDevice(ctx context.Context, userID, id, name, simE164 string) error {
	name = normalizeName(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE sip_devices SET name = ?, sim_e164 = ? WHERE id = ? AND user_id = ?`,
		name, trimSpace(simE164), id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteSIPDevice(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sip_devices WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

func (s *Store) ListSIPDevices(ctx context.Context, userID string) ([]SIPDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, name, sim_e164, last_seen_at, created_at FROM sip_devices WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SIPDevice
	for rows.Next() {
		var d SIPDevice
		var last, created sql.NullString
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.SimE164, &last, &created); err != nil {
			return nil, err
		}
		if last.Valid {
			t, _ := time.Parse(time.RFC3339, last.String)
			d.LastSeen = &t
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) CountSIPDevices(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sip_devices WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func (s *Store) GetSIPDevice(ctx context.Context, userID, id string) (*SIPDevice, error) {
	var d SIPDevice
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, name, sim_e164, last_seen_at, created_at FROM sip_devices WHERE id = ? AND user_id = ?`,
		id, userID).Scan(&d.ID, &d.UserID, &d.Name, &d.SimE164, &last, &created)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339, last.String)
		d.LastSeen = &t
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &d, nil
}
