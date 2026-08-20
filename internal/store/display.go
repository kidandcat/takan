package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Display is a named kiosk screen backed by a takan-agent machine.
type Display struct {
	ID          string
	UserID      string
	Name        string
	MachineID   string
	MachineName string
	IsDefault   bool
	LastShown   *time.Time
	CreatedAt   time.Time
}

func (s *Store) migrateDisplay() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS displays (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  machine_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
  is_default INTEGER NOT NULL DEFAULT 0,
  last_shown_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_displays_user ON displays(user_id);
CREATE INDEX IF NOT EXISTS idx_displays_machine ON displays(machine_id);
`)
	return err
}

func scanDisplay(id, userID, name, machineID, machineName string, def int, last, created sql.NullString) Display {
	d := Display{ID: id, UserID: userID, Name: name, MachineID: machineID, MachineName: machineName, IsDefault: def != 0}
	if last.Valid && last.String != "" {
		t, _ := time.Parse(time.RFC3339, last.String)
		d.LastShown = &t
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return d
}

func (s *Store) CreateDisplay(ctx context.Context, userID, name, machineID string, isDefault bool) (*Display, error) {
	name = normalizeName(name)
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	mac, err := s.MachineByID(ctx, userID, machineID)
	if err != nil {
		return nil, fmt.Errorf("unknown machine")
	}
	n, err := s.CountDisplays(ctx, userID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		isDefault = true
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	if isDefault {
		if _, err := s.db.ExecContext(ctx, `UPDATE displays SET is_default = 0 WHERE user_id = ?`, userID); err != nil {
			return nil, err
		}
	}
	def := 0
	if isDefault {
		def = 1
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO displays (id, user_id, name, machine_id, is_default, created_at) VALUES (?,?,?,?,?,?)`,
		id, userID, name, mac.ID, def, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("create display: %w", err)
	}
	return &Display{
		ID: id, UserID: userID, Name: name, MachineID: mac.ID, MachineName: mac.Name,
		IsDefault: isDefault, CreatedAt: now,
	}, nil
}

func (s *Store) CountDisplays(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM displays WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func (s *Store) ListDisplays(ctx context.Context, userID string) ([]Display, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.user_id, d.name, d.machine_id, m.name, d.is_default, d.last_shown_at, d.created_at
FROM displays d
JOIN machines m ON m.id = d.machine_id
WHERE d.user_id = ?
ORDER BY d.is_default DESC, d.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Display
	for rows.Next() {
		var id, uid, name, mid, mname string
		var def int
		var last, created sql.NullString
		if err := rows.Scan(&id, &uid, &name, &mid, &mname, &def, &last, &created); err != nil {
			return nil, err
		}
		out = append(out, scanDisplay(id, uid, name, mid, mname, def, last, created))
	}
	return out, rows.Err()
}

func (s *Store) DisplayByUserAndName(ctx context.Context, userID, name string) (*Display, error) {
	name = normalizeName(name)
	var id, uid, dname, mid, mname string
	var def int
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT d.id, d.user_id, d.name, d.machine_id, m.name, d.is_default, d.last_shown_at, d.created_at
FROM displays d
JOIN machines m ON m.id = d.machine_id
WHERE d.user_id = ? AND d.name = ?`, userID, name).
		Scan(&id, &uid, &dname, &mid, &mname, &def, &last, &created)
	if err != nil {
		return nil, err
	}
	d := scanDisplay(id, uid, dname, mid, mname, def, last, created)
	return &d, nil
}

func (s *Store) DisplayByID(ctx context.Context, userID, id string) (*Display, error) {
	var did, uid, dname, mid, mname string
	var def int
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT d.id, d.user_id, d.name, d.machine_id, m.name, d.is_default, d.last_shown_at, d.created_at
FROM displays d
JOIN machines m ON m.id = d.machine_id
WHERE d.user_id = ? AND d.id = ?`, userID, id).
		Scan(&did, &uid, &dname, &mid, &mname, &def, &last, &created)
	if err != nil {
		return nil, err
	}
	d := scanDisplay(did, uid, dname, mid, mname, def, last, created)
	return &d, nil
}

func (s *Store) DefaultDisplay(ctx context.Context, userID string) (*Display, error) {
	var id, uid, dname, mid, mname string
	var def int
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT d.id, d.user_id, d.name, d.machine_id, m.name, d.is_default, d.last_shown_at, d.created_at
FROM displays d
JOIN machines m ON m.id = d.machine_id
WHERE d.user_id = ? AND d.is_default = 1
LIMIT 1`, userID).
		Scan(&id, &uid, &dname, &mid, &mname, &def, &last, &created)
	if err != nil {
		return nil, err
	}
	d := scanDisplay(id, uid, dname, mid, mname, def, last, created)
	return &d, nil
}

func (s *Store) SetDisplayDefault(ctx context.Context, userID, id string) error {
	if _, err := s.DisplayByID(ctx, userID, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE displays SET is_default = 0 WHERE user_id = ?`, userID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE displays SET is_default = 1 WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteDisplay(ctx context.Context, userID, id string) error {
	wasDefault := false
	d, err := s.DisplayByID(ctx, userID, id)
	if err == nil {
		wasDefault = d.IsDefault
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM displays WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if wasDefault {
		var next sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT id FROM displays WHERE user_id = ? ORDER BY name LIMIT 1`, userID).Scan(&next)
		if next.Valid {
			_, _ = s.db.ExecContext(ctx, `UPDATE displays SET is_default = 1 WHERE id = ?`, next.String)
		}
	}
	return nil
}

func (s *Store) TouchDisplayShown(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE displays SET last_shown_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) MachineByID(ctx context.Context, userID, id string) (*Machine, error) {
	var m Machine
	var last, created sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, name, last_seen_at, created_at FROM machines WHERE id = ? AND user_id = ?`,
		id, userID).Scan(&m.ID, &m.UserID, &m.Name, &last, &created)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339, last.String)
		m.LastSeen = &t
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created.String)
	return &m, nil
}
