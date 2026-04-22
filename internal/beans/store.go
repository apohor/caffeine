// Package beans is a tiny CRUD store for coffee beans. The user tags
// a shot with the bag of beans they pulled it with, so later the app
// can answer "how was my ninja natural last week" and the AI coach can
// factor origin/roast age into its suggestions.
package beans

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by Get/Update/Delete when the id doesn't exist.
var ErrNotFound = errors.New("bean not found")

// Bean is the full record as stored in SQLite.
type Bean struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Roaster       string  `json:"roaster,omitempty"`
	Origin        string  `json:"origin,omitempty"`      // country / farm / blend
	Process       string  `json:"process,omitempty"`     // washed / natural / honey / etc
	RoastLevel    string  `json:"roast_level,omitempty"` // light / medium / dark / etc
	RoastDate     string  `json:"roast_date,omitempty"`  // ISO yyyy-mm-dd
	Notes         string  `json:"notes,omitempty"`
	CreatedAtUnix float64 `json:"created_at_unix"`
	Archived      bool    `json:"archived,omitempty"`
	// Active marks the "bag currently in use". At most one bean row
	// has Active=true at a time; the shots store auto-tags new shots
	// with this bean id so users don't have to pick one per shot.
	Active bool `json:"active,omitempty"`
	// DefaultGrindSize is a free-form grinder label (e.g. "2.8", "12 clicks")
	// used as the starting point for new shots pulled with this bag.
	// Empty string = no default. The user can override per shot.
	DefaultGrindSize string `json:"default_grind_size,omitempty"`
	// DefaultGrindRPM is the variable-speed grinder RPM default. Nil
	// means "no default / not applicable to this grinder".
	DefaultGrindRPM *float64 `json:"default_grind_rpm,omitempty"`
}

// Input is the writable slice of a Bean (fields the user controls).
type Input struct {
	Name            string   `json:"name"`
	Roaster         string   `json:"roaster"`
	Origin          string   `json:"origin"`
	Process         string   `json:"process"`
	RoastLevel      string   `json:"roast_level"`
	RoastDate       string   `json:"roast_date"`
	Notes           string   `json:"notes"`
	Archived        *bool    `json:"archived,omitempty"`
	DefaultGrindSize    string   `json:"default_grind_size"`
	DefaultGrindRPM *float64 `json:"default_grind_rpm"`
}

// Store wraps the shared SQLite handle.
type Store struct{ db *sql.DB }

// New wires the beans table against an already-open *sql.DB (shared with
// shots etc.). Idempotent: safe to call on every boot.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS beans (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL,
		roaster         TEXT NOT NULL DEFAULT '',
		origin          TEXT NOT NULL DEFAULT '',
		process         TEXT NOT NULL DEFAULT '',
		roast_level     TEXT NOT NULL DEFAULT '',
		roast_date      TEXT NOT NULL DEFAULT '',
		notes           TEXT NOT NULL DEFAULT '',
		created_at_unix REAL NOT NULL,
		archived        INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return err
	}
	// Forward-compat migration: `active` was added after the initial
	// release. Ignore "duplicate column" on re-runs.
	for _, stmt := range []string{
		`ALTER TABLE beans ADD COLUMN active INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE beans ADD COLUMN default_grind_size TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE beans ADD COLUMN default_grind_rpm REAL`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("migrate beans: %w", err)
			}
		}
	}
	return nil
}

// List returns all beans, active first, then archived.
func (s *Store) List(ctx context.Context) ([]Bean, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, roaster, origin, process, roast_level, roast_date, notes, created_at_unix, archived, active, default_grind_size, default_grind_rpm FROM beans ORDER BY archived ASC, created_at_unix DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bean
	for rows.Next() {
		var b Bean
		var arch, active int
		var rpm sql.NullFloat64
		if err := rows.Scan(&b.ID, &b.Name, &b.Roaster, &b.Origin, &b.Process, &b.RoastLevel, &b.RoastDate, &b.Notes, &b.CreatedAtUnix, &arch, &active, &b.DefaultGrindSize, &rpm); err != nil {
			return nil, err
		}
		b.Archived = arch != 0
		b.Active = active != 0
		if rpm.Valid {
			v := rpm.Float64
			b.DefaultGrindRPM = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get fetches a single bean by id.
func (s *Store) Get(ctx context.Context, id string) (*Bean, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, roaster, origin, process, roast_level, roast_date, notes, created_at_unix, archived, active, default_grind_size, default_grind_rpm FROM beans WHERE id = ?`, id)
	var b Bean
	var arch, active int
	var rpm sql.NullFloat64
	if err := row.Scan(&b.ID, &b.Name, &b.Roaster, &b.Origin, &b.Process, &b.RoastLevel, &b.RoastDate, &b.Notes, &b.CreatedAtUnix, &arch, &active, &b.DefaultGrindSize, &rpm); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.Archived = arch != 0
	b.Active = active != 0
	if rpm.Valid {
		v := rpm.Float64
		b.DefaultGrindRPM = &v
	}
	return &b, nil
}

// Create inserts a new bean with a generated id and returns the full record.
func (s *Store) Create(ctx context.Context, in Input) (*Bean, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("bean: name is required")
	}
	id := newID()
	now := float64(time.Now().Unix())
	arch := 0
	if in.Archived != nil && *in.Archived {
		arch = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO beans (id, name, roaster, origin, process, roast_level, roast_date, notes, created_at_unix, archived, default_grind_size, default_grind_rpm)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Roaster, in.Origin, in.Process, in.RoastLevel, in.RoastDate, in.Notes, now, arch, in.DefaultGrindSize, nullableFloat(in.DefaultGrindRPM))
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Update rewrites the mutable fields of an existing bean.
func (s *Store) Update(ctx context.Context, id string, in Input) (*Bean, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("bean: name is required")
	}
	arch := 0
	if in.Archived != nil && *in.Archived {
		arch = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE beans
		   SET name = ?, roaster = ?, origin = ?, process = ?, roast_level = ?, roast_date = ?, notes = ?, archived = ?, default_grind_size = ?, default_grind_rpm = ?
		 WHERE id = ?`,
		in.Name, in.Roaster, in.Origin, in.Process, in.RoastLevel, in.RoastDate, in.Notes, arch, in.DefaultGrindSize, nullableFloat(in.DefaultGrindRPM), id)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Delete removes a bean by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM beans WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive marks one bean as the "bag currently in use", clearing the
// flag on every other row. Pass an empty id to just clear any active
// marker (no bag currently in use). Runs in a single transaction so
// readers never observe two active rows.
func (s *Store) SetActive(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE beans SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if id != "" {
		res, err := tx.ExecContext(ctx,
			`UPDATE beans SET active = 1, archived = 0 WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// ActiveID returns the id of the currently-active bean, or "" if none.
// Never returns an error in the happy path — a missing row is not a
// failure, the caller just skips auto-tagging. A SQL error (e.g. the
// db is closed) is still propagated.
func (s *Store) ActiveID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM beans WHERE active = 1 AND archived = 0 LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ActiveDefaults returns the currently-active bean's id and its default
// grind settings, so the shots store can seed new shots with both in a
// single query. Any value may be empty/nil: "" id means no active bag,
// empty grind means the user hasn't configured a default grind, and a
// nil rpm means no default RPM. A missing row is not an error.
func (s *Store) ActiveDefaults(ctx context.Context) (id, grind string, rpm *float64, err error) {
	var rpmNull sql.NullFloat64
	err = s.db.QueryRowContext(ctx,
		`SELECT id, default_grind_size, default_grind_rpm FROM beans WHERE active = 1 AND archived = 0 LIMIT 1`,
	).Scan(&id, &grind, &rpmNull)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, nil
	}
	if err != nil {
		return "", "", nil, err
	}
	if rpmNull.Valid {
		v := rpmNull.Float64
		rpm = &v
	}
	return id, grind, rpm, nil
}

// nullableFloat converts an optional *float64 into the interface that
// database/sql recognises as NULL when the pointer is nil.
func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// MarshalJSON lets Input distinguish between "empty string" and
// "field not sent" if we ever need to — for now a thin wrapper.
var _ = json.Marshaler(nil)

func newID() string {
	b := make([]byte, 8)
	_, _ = readRand(b)
	return fmt.Sprintf("bean_%x%d", b, time.Now().UnixNano()%1000)
}
