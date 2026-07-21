package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// --- health schema (profile + daily log + issues/injuries) ---

// HealthProfile is the user's baseline body metrics and freeform notes.
type HealthProfile struct {
	UserID    string
	HeightCM  *float64 // centimeters; nil if unset
	WeightKG  *float64 // latest known weight; also mirrored from log upserts when set
	Notes     string
	UpdatedAt time.Time
}

// HealthLogEntry is one calendar day of health diary.
type HealthLogEntry struct {
	ID         string
	UserID     string
	Day        string // YYYY-MM-DD
	WeightKG   *float64
	Sleep      string
	Training   string
	Symptoms   string
	Pain       string
	Medication string
	Notes      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// HealthIssue is an injury, condition, or medical event (historial).
type HealthIssue struct {
	ID         string
	UserID     string
	Title      string
	Status     string // active | recovering | resolved | chronic
	StartedOn  string // YYYY-MM-DD
	EndedOn    string
	BodyPart   string
	Diagnosis  string
	Treatment  string
	Notes      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (s *Store) migrateHealth() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS health_profile (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  height_cm REAL,
  weight_kg REAL,
  notes TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS health_log (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  day TEXT NOT NULL,
  weight_kg REAL,
  sleep TEXT NOT NULL DEFAULT '',
  training TEXT NOT NULL DEFAULT '',
  symptoms TEXT NOT NULL DEFAULT '',
  pain TEXT NOT NULL DEFAULT '',
  medication TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_health_log_user_day ON health_log(user_id, day DESC);

CREATE TABLE IF NOT EXISTS health_issues (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  started_on TEXT NOT NULL DEFAULT '',
  ended_on TEXT NOT NULL DEFAULT '',
  body_part TEXT NOT NULL DEFAULT '',
  diagnosis TEXT NOT NULL DEFAULT '',
  treatment TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_health_issues_user ON health_issues(user_id);
CREATE INDEX IF NOT EXISTS idx_health_issues_status ON health_issues(user_id, status);
`)
	return err
}

// --- profile ---

func (s *Store) GetHealthProfile(ctx context.Context, userID string) (*HealthProfile, bool, error) {
	var height, weight sql.NullFloat64
	var notes, updated string
	err := s.db.QueryRowContext(ctx, `
SELECT height_cm, weight_kg, notes, updated_at FROM health_profile WHERE user_id = ?`, userID).
		Scan(&height, &weight, &notes, &updated)
	if err == sql.ErrNoRows {
		return &HealthProfile{UserID: userID}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p := &HealthProfile{UserID: userID, Notes: notes}
	if height.Valid {
		v := height.Float64
		p.HeightCM = &v
	}
	if weight.Valid {
		v := weight.Float64
		p.WeightKG = &v
	}
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return p, true, nil
}

// UpsertHealthProfile merges fields. nil pointers leave existing values; empty notes string clears notes when setNotes is true.
func (s *Store) UpsertHealthProfile(ctx context.Context, userID string, heightCM, weightKG *float64, notes *string) (*HealthProfile, error) {
	cur, _, err := s.GetHealthProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if heightCM != nil {
		if *heightCM <= 0 {
			cur.HeightCM = nil
		} else {
			v := *heightCM
			cur.HeightCM = &v
		}
	}
	if weightKG != nil {
		if *weightKG <= 0 {
			cur.WeightKG = nil
		} else {
			v := *weightKG
			cur.WeightKG = &v
		}
	}
	if notes != nil {
		cur.Notes = *notes
	}
	now := time.Now().UTC()
	cur.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
INSERT INTO health_profile (user_id, height_cm, weight_kg, notes, updated_at) VALUES (?,?,?,?,?)
ON CONFLICT(user_id) DO UPDATE SET
  height_cm = excluded.height_cm,
  weight_kg = excluded.weight_kg,
  notes = excluded.notes,
  updated_at = excluded.updated_at`,
		userID, nullF(cur.HeightCM), nullF(cur.WeightKG), cur.Notes, now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return cur, nil
}

// --- daily log ---

func madridNow() time.Time {
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		return time.Now().UTC()
	}
	return time.Now().In(loc)
}

func normalizeDay(day string) (string, error) {
	day = strings.TrimSpace(day)
	if day == "" {
		return madridNow().Format("2006-01-02"), nil
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", fmt.Errorf("day must be YYYY-MM-DD")
	}
	return day, nil
}

// UpsertHealthLog merges provided fields for a calendar day (YYYY-MM-DD; empty day = today Europe/Madrid).
// Pointer fields: nil = leave unchanged; set weight <= 0 to clear weight.
// appendNote appends a line to notes without wiping other fields.
// When weight is set (>0), also updates profile.weight_kg.
func (s *Store) UpsertHealthLog(ctx context.Context, userID, day string, weightKG *float64, sleep, training, symptoms, pain, medication, notes *string, appendNote string) (*HealthLogEntry, error) {
	day, err := normalizeDay(day)
	if err != nil {
		return nil, err
	}
	cur, err := s.GetHealthLog(ctx, userID, day)
	now := time.Now().UTC()
	if err == sql.ErrNoRows {
		e := HealthLogEntry{
			ID: uuid.NewString(), UserID: userID, Day: day,
			CreatedAt: now, UpdatedAt: now,
		}
		if weightKG != nil && *weightKG > 0 {
			v := *weightKG
			e.WeightKG = &v
		}
		if sleep != nil {
			e.Sleep = *sleep
		}
		if training != nil {
			e.Training = *training
		}
		if symptoms != nil {
			e.Symptoms = *symptoms
		}
		if pain != nil {
			e.Pain = *pain
		}
		if medication != nil {
			e.Medication = *medication
		}
		if notes != nil {
			e.Notes = *notes
		}
		if strings.TrimSpace(appendNote) != "" {
			if e.Notes == "" {
				e.Notes = strings.TrimSpace(appendNote)
			} else {
				e.Notes = e.Notes + "\n" + strings.TrimSpace(appendNote)
			}
		}
		_, err = s.db.ExecContext(ctx, `
INSERT INTO health_log (id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, userID, day, nullF(e.WeightKG), e.Sleep, e.Training, e.Symptoms, e.Pain, e.Medication, e.Notes,
			now.Format(time.RFC3339), now.Format(time.RFC3339))
		if err != nil {
			return nil, err
		}
		if e.WeightKG != nil {
			_, _ = s.UpsertHealthProfile(ctx, userID, nil, e.WeightKG, nil)
		}
		return &e, nil
	}
	if err != nil {
		return nil, err
	}
	if weightKG != nil {
		if *weightKG <= 0 {
			cur.WeightKG = nil
		} else {
			v := *weightKG
			cur.WeightKG = &v
		}
	}
	if sleep != nil {
		cur.Sleep = *sleep
	}
	if training != nil {
		cur.Training = *training
	}
	if symptoms != nil {
		cur.Symptoms = *symptoms
	}
	if pain != nil {
		cur.Pain = *pain
	}
	if medication != nil {
		cur.Medication = *medication
	}
	if notes != nil {
		cur.Notes = *notes
	}
	if strings.TrimSpace(appendNote) != "" {
		if strings.TrimSpace(cur.Notes) == "" {
			cur.Notes = strings.TrimSpace(appendNote)
		} else {
			cur.Notes = strings.TrimSpace(cur.Notes) + "\n" + strings.TrimSpace(appendNote)
		}
	}
	cur.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
UPDATE health_log SET weight_kg=?, sleep=?, training=?, symptoms=?, pain=?, medication=?, notes=?, updated_at=?
WHERE user_id=? AND day=?`,
		nullF(cur.WeightKG), cur.Sleep, cur.Training, cur.Symptoms, cur.Pain, cur.Medication, cur.Notes,
		now.Format(time.RFC3339), userID, day)
	if err != nil {
		return nil, err
	}
	if cur.WeightKG != nil && *cur.WeightKG > 0 {
		_, _ = s.UpsertHealthProfile(ctx, userID, nil, cur.WeightKG, nil)
	}
	return cur, nil
}

func (s *Store) GetHealthLog(ctx context.Context, userID, day string) (*HealthLogEntry, error) {
	day, err := normalizeDay(day)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at
FROM health_log WHERE user_id = ? AND day = ?`, userID, day)
	return scanHealthLog(row)
}

func (s *Store) ListHealthLog(ctx context.Context, userID, from, to string, limit int) ([]HealthLogEntry, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 365 {
		limit = 365
	}
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	var rows *sql.Rows
	var err error
	switch {
	case from != "" && to != "":
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at
FROM health_log WHERE user_id = ? AND day >= ? AND day <= ? ORDER BY day DESC LIMIT ?`,
			userID, from, to, limit)
	case from != "":
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at
FROM health_log WHERE user_id = ? AND day >= ? ORDER BY day DESC LIMIT ?`,
			userID, from, limit)
	case to != "":
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at
FROM health_log WHERE user_id = ? AND day <= ? ORDER BY day DESC LIMIT ?`,
			userID, to, limit)
	default:
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, day, weight_kg, sleep, training, symptoms, pain, medication, notes, created_at, updated_at
FROM health_log WHERE user_id = ? ORDER BY day DESC LIMIT ?`,
			userID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthLogEntry
	for rows.Next() {
		e, err := scanHealthLogRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteHealthLog(ctx context.Context, userID, day string) error {
	day, err := normalizeDay(day)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM health_log WHERE user_id = ? AND day = ?`, userID, day)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CountHealthLog(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM health_log WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// --- issues ---

func normalizeIssueStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "active":
		return "active"
	case "recovering", "resolved", "chronic":
		return s
	default:
		return s // allow freeform but prefer known values
	}
}

func (s *Store) CreateHealthIssue(ctx context.Context, iss HealthIssue) (*HealthIssue, error) {
	iss.Title = strings.TrimSpace(iss.Title)
	if iss.Title == "" {
		return nil, fmt.Errorf("title required")
	}
	if iss.ID == "" {
		iss.ID = uuid.NewString()
	}
	iss.Status = normalizeIssueStatus(iss.Status)
	now := time.Now().UTC()
	iss.CreatedAt = now
	iss.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO health_issues (id, user_id, title, status, started_on, ended_on, body_part, diagnosis, treatment, notes, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		iss.ID, iss.UserID, iss.Title, iss.Status, strings.TrimSpace(iss.StartedOn), strings.TrimSpace(iss.EndedOn),
		strings.TrimSpace(iss.BodyPart), iss.Diagnosis, iss.Treatment, iss.Notes,
		now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return &iss, nil
}

func (s *Store) UpdateHealthIssue(ctx context.Context, userID, id string, fields map[string]string) (*HealthIssue, error) {
	cur, err := s.GetHealthIssue(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if v, ok := fields["title"]; ok && strings.TrimSpace(v) != "" {
		cur.Title = strings.TrimSpace(v)
	}
	if v, ok := fields["status"]; ok {
		cur.Status = normalizeIssueStatus(v)
	}
	if v, ok := fields["started_on"]; ok {
		cur.StartedOn = strings.TrimSpace(v)
	}
	if v, ok := fields["ended_on"]; ok {
		cur.EndedOn = strings.TrimSpace(v)
	}
	if v, ok := fields["body_part"]; ok {
		cur.BodyPart = strings.TrimSpace(v)
	}
	if v, ok := fields["diagnosis"]; ok {
		cur.Diagnosis = v
	}
	if v, ok := fields["treatment"]; ok {
		cur.Treatment = v
	}
	if v, ok := fields["notes"]; ok {
		cur.Notes = v
	}
	if v, ok := fields["append_notes"]; ok && strings.TrimSpace(v) != "" {
		if strings.TrimSpace(cur.Notes) == "" {
			cur.Notes = strings.TrimSpace(v)
		} else {
			cur.Notes = strings.TrimSpace(cur.Notes) + "\n" + strings.TrimSpace(v)
		}
	}
	cur.UpdatedAt = time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
UPDATE health_issues SET title=?, status=?, started_on=?, ended_on=?, body_part=?, diagnosis=?, treatment=?, notes=?, updated_at=?
WHERE id=? AND user_id=?`,
		cur.Title, cur.Status, cur.StartedOn, cur.EndedOn, cur.BodyPart, cur.Diagnosis, cur.Treatment, cur.Notes,
		cur.UpdatedAt.Format(time.RFC3339), id, userID)
	if err != nil {
		return nil, err
	}
	return cur, nil
}

func (s *Store) GetHealthIssue(ctx context.Context, userID, id string) (*HealthIssue, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, user_id, title, status, started_on, ended_on, body_part, diagnosis, treatment, notes, created_at, updated_at
FROM health_issues WHERE id = ? AND user_id = ?`, id, userID)
	return scanHealthIssue(row)
}

func (s *Store) ListHealthIssues(ctx context.Context, userID, status string, limit int) ([]HealthIssue, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	status = strings.TrimSpace(status)
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, title, status, started_on, ended_on, body_part, diagnosis, treatment, notes, created_at, updated_at
FROM health_issues WHERE user_id = ?
ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'recovering' THEN 1 WHEN 'chronic' THEN 2 ELSE 3 END, started_on DESC
LIMIT ?`, userID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, user_id, title, status, started_on, ended_on, body_part, diagnosis, treatment, notes, created_at, updated_at
FROM health_issues WHERE user_id = ? AND lower(status) = lower(?)
ORDER BY started_on DESC LIMIT ?`, userID, status, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthIssue
	for rows.Next() {
		iss, err := scanHealthIssueRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *iss)
	}
	return out, rows.Err()
}

func (s *Store) DeleteHealthIssue(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM health_issues WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CountHealthIssues(ctx context.Context, userID, status string) (int, error) {
	var n int
	var err error
	if strings.TrimSpace(status) == "" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM health_issues WHERE user_id = ?`, userID).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM health_issues WHERE user_id = ? AND lower(status) = lower(?)`, userID, status).Scan(&n)
	}
	return n, err
}

// --- helpers ---

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

type scanner interface {
	Scan(dest ...any) error
}

func scanHealthLog(row scanner) (*HealthLogEntry, error) {
	var e HealthLogEntry
	var weight sql.NullFloat64
	var created, updated string
	err := row.Scan(&e.ID, &e.UserID, &e.Day, &weight, &e.Sleep, &e.Training, &e.Symptoms, &e.Pain, &e.Medication, &e.Notes, &created, &updated)
	if err != nil {
		return nil, err
	}
	if weight.Valid {
		v := weight.Float64
		e.WeightKG = &v
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339, created)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &e, nil
}

func scanHealthLogRows(rows *sql.Rows) (*HealthLogEntry, error) {
	return scanHealthLog(rows)
}

func scanHealthIssue(row scanner) (*HealthIssue, error) {
	var iss HealthIssue
	var created, updated string
	err := row.Scan(&iss.ID, &iss.UserID, &iss.Title, &iss.Status, &iss.StartedOn, &iss.EndedOn,
		&iss.BodyPart, &iss.Diagnosis, &iss.Treatment, &iss.Notes, &created, &updated)
	if err != nil {
		return nil, err
	}
	iss.CreatedAt, _ = time.Parse(time.RFC3339, created)
	iss.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &iss, nil
}

func scanHealthIssueRows(rows *sql.Rows) (*HealthIssue, error) {
	return scanHealthIssue(rows)
}
