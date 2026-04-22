// Package preheat persists user-defined preheat schedules and runs the
// background scheduler that fires them.
//
// "Preheat" on the Meticulous machine means "warm the boiler / group head so
// the next shot pulls at the configured temperature". The machine itself
// owns a boolean `auto_preheat` setting — when set true the firmware enters
// its preheat cycle. We don't model the cycle ourselves; we just toggle that
// setting on the schedules the operator defines.
//
// Schedules are stored in the same SQLite database the rest of the app uses
// (see internal/shots and internal/settings — there is one DB file).
package preheat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a schedule lookup misses.
var ErrNotFound = errors.New("schedule not found")

const schema = `
CREATE TABLE IF NOT EXISTS preheat_schedules (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    time_of_day   TEXT NOT NULL,    -- "HH:MM" in machine local time
    weekday_mask  INTEGER NOT NULL, -- bitmask, Sun=bit0 ... Sat=bit6
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
`

// Schedule is a single preheat trigger rule.
//
// WeekdayMask is a 7-bit mask. Sunday=bit0, Monday=bit1 … Saturday=bit6.
// 0b0111110 = 62 = weekdays Mon–Fri.
type Schedule struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	TimeOfDay   string    `json:"time_of_day"`
	WeekdayMask int       `json:"weekday_mask"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Validate checks the user-supplied fields. ID/timestamps are filled by the
// store on Create.
func (s *Schedule) Validate() error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	if _, err := time.Parse("15:04", s.TimeOfDay); err != nil {
		return fmt.Errorf("time_of_day must be HH:MM (24h): %w", err)
	}
	if s.WeekdayMask <= 0 || s.WeekdayMask > 0b1111111 {
		return errors.New("weekday_mask must be 1..127")
	}
	return nil
}

// Store wraps a *sql.DB with preheat-schedule CRUD.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) a SQLite database at path. Safe to point at
// the same file used by other stores in this app.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply preheat schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// List returns every schedule, oldest first.
func (s *Store) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, enabled, time_of_day, weekday_mask, created_at, updated_at
		 FROM preheat_schedules ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var (
			sch              Schedule
			enabled          int
			created, updated int64
		)
		if err := rows.Scan(&sch.ID, &sch.Name, &enabled, &sch.TimeOfDay,
			&sch.WeekdayMask, &created, &updated); err != nil {
			return nil, err
		}
		sch.Enabled = enabled != 0
		sch.CreatedAt = time.Unix(created, 0).UTC()
		sch.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, sch)
	}
	return out, rows.Err()
}

// Get fetches a single schedule.
func (s *Store) Get(ctx context.Context, id string) (Schedule, error) {
	var (
		sch              Schedule
		enabled          int
		created, updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, enabled, time_of_day, weekday_mask, created_at, updated_at
		 FROM preheat_schedules WHERE id = ?`, id).Scan(
		&sch.ID, &sch.Name, &enabled, &sch.TimeOfDay,
		&sch.WeekdayMask, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	if err != nil {
		return Schedule{}, err
	}
	sch.Enabled = enabled != 0
	sch.CreatedAt = time.Unix(created, 0).UTC()
	sch.UpdatedAt = time.Unix(updated, 0).UTC()
	return sch, nil
}

// Create inserts a new schedule. ID and timestamps are assigned here.
func (s *Store) Create(ctx context.Context, sch *Schedule) error {
	if err := sch.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	sch.CreatedAt = now
	sch.UpdatedAt = now
	enabled := 0
	if sch.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preheat_schedules
		 (id, name, enabled, time_of_day, weekday_mask, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sch.ID, sch.Name, enabled, sch.TimeOfDay, sch.WeekdayMask,
		now.Unix(), now.Unix())
	return err
}

// Update overwrites the editable fields of an existing schedule.
func (s *Store) Update(ctx context.Context, sch *Schedule) error {
	if err := sch.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	sch.UpdatedAt = now
	enabled := 0
	if sch.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE preheat_schedules
		 SET name = ?, enabled = ?, time_of_day = ?, weekday_mask = ?, updated_at = ?
		 WHERE id = ?`,
		sch.Name, enabled, sch.TimeOfDay, sch.WeekdayMask, now.Unix(), sch.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a schedule. Missing IDs are not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM preheat_schedules WHERE id = ?`, id)
	return err
}
