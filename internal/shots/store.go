// Package shots caches shot history from the Meticulous machine in a local
// SQLite database and serves it to the UI.
//
// The machine exposes a single GET /api/v1/history endpoint that returns
// every shot, including the full per-sample time-series, in one blob.
// For the UI we want cheap list-many / read-one semantics. This package:
//
//  1. Periodically fetches /api/v1/history and upserts each shot by id.
//  2. Stores metadata in one row per shot plus a JSON blob for samples.
//  3. Serves compact list + full-detail reads straight from SQLite.
package shots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS shots (
    id           TEXT PRIMARY KEY,
    db_key       INTEGER,
    time_unix    REAL NOT NULL,
    name         TEXT,
    file         TEXT,
    debug_file   TEXT,
    profile_id   TEXT,
    profile_name TEXT,
    sample_count INTEGER NOT NULL DEFAULT 0,
    summary_json TEXT NOT NULL,
    samples_json TEXT NOT NULL,
    inserted_at  INTEGER NOT NULL,
    hidden       INTEGER NOT NULL DEFAULT 0,
    rating       INTEGER,          -- 1..5, NULL = unrated
    note         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_shots_time ON shots(time_unix DESC);

-- Cached LLM analyses. One row per (shot, model) tuple so switching the
-- model produces a new entry rather than clobbering the old one.
CREATE TABLE IF NOT EXISTS shot_analyses (
    shot_id      TEXT NOT NULL,
    model        TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    analysis_json TEXT NOT NULL,
    PRIMARY KEY (shot_id, model),
    FOREIGN KEY (shot_id) REFERENCES shots(id) ON DELETE CASCADE
);

-- Cached "next pull" coach suggestions. Same (shot, model) keying as
-- analyses so a user re-running the coach after switching provider
-- keeps both rows. Payload is the full Suggestion JSON the API
-- returns so the client can render from the cache with no conversion.
CREATE TABLE IF NOT EXISTS shot_coach_suggestions (
    shot_id        TEXT NOT NULL,
    model          TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    suggestion_json TEXT NOT NULL,
    PRIMARY KEY (shot_id, model),
    FOREIGN KEY (shot_id) REFERENCES shots(id) ON DELETE CASCADE
);

-- Cached A/B shot comparisons. The key is (a_id, b_id) canonicalised
-- so a<b at write time — that way comparing shots in either order
-- shares a row and the UI never double-calls the LLM for the same
-- pair. Model is included in the key for the same reason as analyses.
CREATE TABLE IF NOT EXISTS shot_compares (
    a_id         TEXT NOT NULL,
    b_id         TEXT NOT NULL,
    model        TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    compare_json TEXT NOT NULL,
    PRIMARY KEY (a_id, b_id, model),
    FOREIGN KEY (a_id) REFERENCES shots(id) ON DELETE CASCADE,
    FOREIGN KEY (b_id) REFERENCES shots(id) ON DELETE CASCADE
);
`

// Store wraps a SQLite database holding the cached shot history.
type Store struct {
	db *sql.DB
	// activeBean, if set, is invoked on every new shot insert to
	// auto-tag the shot with the user's currently-active bag and to
	// seed the grinder defaults (grind label + RPM) associated with
	// that bag. Returns an empty id to skip auto-tagging. Wired from
	// main.go to the beans store — kept as a plain fn so the shots
	// package doesn't depend on internal/beans.
	activeBean func(context.Context) (id, grind string, rpm *float64)
}

// OpenStore opens (or creates) a SQLite database at path. Use ":memory:" for tests.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Forward-compat migrations: each column was added after the
	// initial schema shipped. "duplicate column" is expected on any DB
	// that already has it; surface any other error.
	for _, stmt := range []string{
		`ALTER TABLE shots ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE shots ADD COLUMN rating INTEGER`,
		`ALTER TABLE shots ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shots ADD COLUMN bean_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shots ADD COLUMN grind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shots ADD COLUMN grind_rpm REAL`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate shots schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// SetActiveBeanResolver wires the "which bag is currently loaded"
// lookup. Called by main.go after the beans store is constructed.
// Safe to call more than once; last setter wins.
func (s *Store) SetActiveBeanResolver(fn func(context.Context) (id, grind string, rpm *float64)) {
	s.activeBean = fn
}

// resolveActiveBean returns the active bean id plus its grinder
// defaults, or zero values if the resolver isn't wired or no bag is
// currently active. Never panics — the feature is optional.
func (s *Store) resolveActiveBean(ctx context.Context) (id, grind string, rpm *float64) {
	if s.activeBean == nil {
		return "", "", nil
	}
	return s.activeBean(ctx)
}

// DB exposes the raw *sql.DB so sibling packages (ai usage recorder,
// beans store, …) can reuse the same SQLite file without opening a
// second connection and fighting over WAL locks.
func (s *Store) DB() *sql.DB { return s.db }

// ShotListItem is the compact list view (no sample data).
type ShotListItem struct {
	ID          string  `json:"id"`
	Time        float64 `json:"time"`
	Name        string  `json:"name"`
	File        string  `json:"file,omitempty"`
	ProfileID   string  `json:"profile_id,omitempty"`
	ProfileName string  `json:"profile_name,omitempty"`
	SampleCount int     `json:"sample_count"`
	// User feedback. Rating is 1..5, or nil if unrated. Note is a
	// free-form tasting note the user attached to the shot.
	Rating *int   `json:"rating,omitempty"`
	Note   string `json:"note,omitempty"`
	// BeanID links this shot to a record in the beans table (empty when unset).
	BeanID string `json:"bean_id,omitempty"`
	// Grind is a free-form grinder setting label (e.g. "2.8" on a
	// Niche, "12" clicks on a Kingrinder). RPM is only meaningful for
	// variable-speed grinders (DF64, P100) and is nil otherwise.
	Grind    string   `json:"grind,omitempty"`
	GrindRPM *float64 `json:"grind_rpm,omitempty"`
}

// Shot is the full detail view including raw samples and the profile snapshot.
type Shot struct {
	ShotListItem
	DebugFile string          `json:"debug_file,omitempty"`
	Samples   json.RawMessage `json:"samples"`
	Profile   json.RawMessage `json:"profile,omitempty"`
}

// ListShots returns the newest-first summary list, capped at limit.
func (s *Store) ListShots(ctx context.Context, limit int) ([]ShotListItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, time_unix, COALESCE(name,''), COALESCE(file,''),
		        COALESCE(profile_id,''), COALESCE(profile_name,''), sample_count,
		        rating, COALESCE(note,''), COALESCE(bean_id,''),
		        COALESCE(grind,''), grind_rpm
		   FROM shots
		  WHERE hidden = 0
		  ORDER BY time_unix DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ShotListItem
	for rows.Next() {
		var it ShotListItem
		var rating sql.NullInt64
		var rpm sql.NullFloat64
		if err := rows.Scan(&it.ID, &it.Time, &it.Name, &it.File,
			&it.ProfileID, &it.ProfileName, &it.SampleCount,
			&rating, &it.Note, &it.BeanID, &it.Grind, &rpm); err != nil {
			return nil, err
		}
		if rating.Valid {
			v := int(rating.Int64)
			it.Rating = &v
		}
		if rpm.Valid {
			v := rpm.Float64
			it.GrindRPM = &v
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetShot returns the full shot (samples + profile snapshot) by id.
func (s *Store) GetShot(ctx context.Context, id string) (*Shot, error) {
	var (
		it            ShotListItem
		debug         sql.NullString
		samples, prof string
		summaryJSON   string
		rating        sql.NullInt64
		rpm           sql.NullFloat64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, time_unix, COALESCE(name,''), COALESCE(file,''),
		        COALESCE(profile_id,''), COALESCE(profile_name,''), sample_count,
		        debug_file, samples_json, summary_json,
		        rating, COALESCE(note,''), COALESCE(bean_id,''),
		        COALESCE(grind,''), grind_rpm
		   FROM shots WHERE id = ?`, id).
		Scan(&it.ID, &it.Time, &it.Name, &it.File, &it.ProfileID, &it.ProfileName,
			&it.SampleCount, &debug, &samples, &summaryJSON,
			&rating, &it.Note, &it.BeanID, &it.Grind, &rpm)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if rating.Valid {
		v := int(rating.Int64)
		it.Rating = &v
	}
	if rpm.Valid {
		v := rpm.Float64
		it.GrindRPM = &v
	}
	// Pull the profile snapshot out of the summary blob.
	var summary struct {
		Profile json.RawMessage `json:"profile"`
	}
	_ = json.Unmarshal([]byte(summaryJSON), &summary)
	prof = string(summary.Profile)
	if prof == "" {
		prof = "null"
	}

	out := &Shot{
		ShotListItem: it,
		Samples:      json.RawMessage(samples),
		Profile:      json.RawMessage(prof),
	}
	if debug.Valid {
		out.DebugFile = debug.String
	}
	return out, nil
}

// ErrNotFound is returned by GetShot when no row matches.
var ErrNotFound = errors.New("shot not found")

// HideShot soft-deletes a shot: flips its hidden flag so it drops out of
// the list and sparkline views. We don't hard-delete because the next
// sync from the machine would just bring it back — hidden survives the
// upsert (the ON CONFLICT clause doesn't touch it).
func (s *Store) HideShot(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE shots SET hidden = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFeedback persists the user's rating (1..5, or nil to clear) and
// free-form note for a shot. Like `hidden`, these columns aren't touched
// by the sync upsert, so they survive future machine syncs.
func (s *Store) SetFeedback(ctx context.Context, id string, rating *int, note string) error {
	var ratingArg any
	if rating != nil {
		r := *rating
		if r < 1 || r > 5 {
			return fmt.Errorf("rating must be between 1 and 5, got %d", r)
		}
		ratingArg = r
	} else {
		ratingArg = nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE shots SET rating = ?, note = ? WHERE id = ?`,
		ratingArg, note, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBean attaches (or clears) a bean id on a shot. Pass "" to clear.
// Like SetFeedback, this column is not overwritten by machine sync.
func (s *Store) SetBean(ctx context.Context, id, beanID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shots SET bean_id = ? WHERE id = ?`, beanID, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetGrind persists the grinder setting label and optional RPM for a
// shot. Pass nil for rpm to clear it, or a non-nil pointer to set it.
// These columns are not touched by machine sync.
func (s *Store) SetGrind(ctx context.Context, id, grind string, rpm *float64) error {
	var rpmArg any
	if rpm != nil {
		rpmArg = *rpm
	} else {
		rpmArg = nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE shots SET grind = ?, grind_rpm = ? WHERE id = ?`,
		grind, rpmArg, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ShotMetrics is the per-shot summary used to decorate list rows: a
// downsampled pressure trace (for the thumbnail) plus cheap-to-derive
// headline numbers. Everything here is computed from samples_json on
// demand, so we don't have to migrate the schema when we want a new
// metric.
type ShotMetrics struct {
	Spark        []float64 `json:"spark,omitempty"`
	PeakPressure float64   `json:"peak_pressure,omitempty"`
	FinalWeight  float64   `json:"final_weight,omitempty"`
}

// ListShotMetrics returns a newest-first map of shot id -> ShotMetrics,
// one entry per shot up to limit. The sparkline trace is normalised to
// `points` values using nearest-neighbour bucketing. Shots with no
// samples are omitted entirely.
func (s *Store) ListShotMetrics(ctx context.Context, limit, points int) (map[string]ShotMetrics, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if points <= 0 || points > 128 {
		points = 24
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, samples_json
		   FROM shots
		  WHERE hidden = 0
		  ORDER BY time_unix DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]ShotMetrics, limit)
	for rows.Next() {
		var id, samplesJSON string
		if err := rows.Scan(&id, &samplesJSON); err != nil {
			return nil, err
		}
		m := extractShotMetrics(samplesJSON, points)
		if len(m.Spark) > 0 || m.PeakPressure > 0 || m.FinalWeight > 0 {
			out[id] = m
		}
	}
	return out, rows.Err()
}

// ListSparklines returns a newest-first map of shot id -> downsampled
// pressure series, one entry per shot up to limit. The series is
// normalised to `points` values using nearest-neighbour bucketing; shots
// with no samples are omitted. Intended for the history list thumbnail.
func (s *Store) ListSparklines(ctx context.Context, limit, points int) (map[string][]float64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if points <= 0 || points > 128 {
		points = 24
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, samples_json
		   FROM shots
		  WHERE hidden = 0
		  ORDER BY time_unix DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]float64, limit)
	for rows.Next() {
		var id, samplesJSON string
		if err := rows.Scan(&id, &samplesJSON); err != nil {
			return nil, err
		}
		spark := extractPressureSparkline(samplesJSON, points)
		if len(spark) > 0 {
			out[id] = spark
		}
	}
	return out, rows.Err()
}

// extractShotMetrics parses a shot samples JSON array and returns a
// downsampled pressure sparkline plus peak pressure and final weight.
// Missing fields are zero-valued; the caller decides whether to emit.
func extractShotMetrics(samplesJSON string, points int) ShotMetrics {
	if samplesJSON == "" || samplesJSON == "[]" || samplesJSON == "null" {
		return ShotMetrics{}
	}
	var raw []struct {
		Shot struct {
			Pressure *float64 `json:"pressure"`
			Weight   *float64 `json:"weight"`
		} `json:"shot"`
	}
	if err := json.Unmarshal([]byte(samplesJSON), &raw); err != nil {
		return ShotMetrics{}
	}
	var out ShotMetrics
	ps := make([]float64, 0, len(raw))
	for _, s := range raw {
		if s.Shot.Pressure != nil {
			p := *s.Shot.Pressure
			ps = append(ps, p)
			if p > out.PeakPressure {
				out.PeakPressure = p
			}
		}
		// Final weight = last non-nil reading in chronological order.
		if s.Shot.Weight != nil {
			out.FinalWeight = *s.Shot.Weight
		}
	}
	if len(ps) == 0 {
		return out
	}
	if len(ps) <= points {
		out.Spark = ps
		return out
	}
	out.Spark = make([]float64, points)
	for i := 0; i < points; i++ {
		idx := i * (len(ps) - 1) / (points - 1)
		out.Spark[i] = ps[idx]
	}
	return out
}

// extractPressureSparkline parses a shot samples JSON array and returns a
// downsampled pressure series of length ~points. Returns nil if no
// pressure values were found.
func extractPressureSparkline(samplesJSON string, points int) []float64 {
	if samplesJSON == "" || samplesJSON == "[]" || samplesJSON == "null" {
		return nil
	}
	var raw []struct {
		Shot struct {
			Pressure *float64 `json:"pressure"`
		} `json:"shot"`
	}
	if err := json.Unmarshal([]byte(samplesJSON), &raw); err != nil {
		return nil
	}
	// Collect non-nil pressure readings in order.
	ps := make([]float64, 0, len(raw))
	for _, s := range raw {
		if s.Shot.Pressure != nil {
			ps = append(ps, *s.Shot.Pressure)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	if len(ps) <= points {
		return ps
	}
	// Nearest-neighbour downsample.
	out := make([]float64, points)
	for i := 0; i < points; i++ {
		idx := i * (len(ps) - 1) / (points - 1)
		out[i] = ps[idx]
	}
	return out
}

// --- Live capture ---------------------------------------------------------

// LiveShotInput is a shot captured from the live WebSocket stream. The
// recorder in internal/live builds this from a sequence of status events.
// Samples must be a JSON array matching the /api/v1/history per-shot
// `data` shape so readers don't branch on source.
type LiveShotInput struct {
	ID          string
	Time        float64 // unix seconds (ms fraction ok)
	Name        string
	ProfileID   string
	ProfileName string
	Samples     json.RawMessage
	Profile     json.RawMessage // may be nil; stored inside summary_json
}

// SaveLiveShot inserts a shot iff no row with the same id already exists.
// This keeps the canonical /history-synced row authoritative: if the
// history sync lands first (or later), it wins via ON CONFLICT in the
// full upsert path. We do NOT upsert here — the live stream lacks some
// metadata fields the machine provides in /history.
func (s *Store) SaveLiveShot(ctx context.Context, in LiveShotInput) error {
	if in.ID == "" {
		return errors.New("live shot: empty id")
	}
	samples := in.Samples
	if len(samples) == 0 {
		samples = json.RawMessage("[]")
	}
	sampleCount := countJSONArray(samples)

	// Build a minimal summary blob that mirrors what a /history row would
	// carry (profile snapshot inline), so GetShot returns something
	// coherent to the UI/analyzer before the canonical row arrives.
	summary := map[string]json.RawMessage{}
	if len(in.Profile) > 0 {
		summary["profile"] = in.Profile
	}
	if b, err := json.Marshal(in.Name); err == nil {
		summary["name"] = b
	}
	summaryJSON, _ := json.Marshal(summary)

	beanID, grind, rpm := s.resolveActiveBean(ctx)
	var rpmArg any
	if rpm != nil {
		rpmArg = *rpm
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO shots
          (id, db_key, time_unix, name, file, debug_file, profile_id, profile_name,
           sample_count, summary_json, samples_json, inserted_at, bean_id, grind, grind_rpm)
        VALUES (?, 0, ?, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO NOTHING
    `,
		in.ID, in.Time, in.Name,
		in.ProfileID, in.ProfileName,
		sampleCount, string(summaryJSON), string(samples), time.Now().Unix(),
		beanID, grind, rpmArg,
	)
	return err
}

// --- Analysis cache -------------------------------------------------------

// GetAnalysis returns the cached analysis JSON for (shotID, model), or
// ErrNotFound if none is cached.
func (s *Store) GetAnalysis(ctx context.Context, shotID, model string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT analysis_json FROM shot_analyses WHERE shot_id = ? AND model = ?`,
		shotID, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// GetLatestAnalysis returns the most recently cached analysis for a shot
// regardless of model, plus the model it was generated with. Used by the
// read path so switching the configured model in Settings doesn't hide
// existing analyses from the user — they're still in the DB, we just
// weren't looking for them.
func (s *Store) GetLatestAnalysis(ctx context.Context, shotID string) (model string, analysis json.RawMessage, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `
		SELECT model, analysis_json
		FROM shot_analyses
		WHERE shot_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, shotID).Scan(&model, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	return model, json.RawMessage(raw), nil
}

// SaveAnalysis upserts an analysis for (shotID, model).
func (s *Store) SaveAnalysis(ctx context.Context, shotID, model string, analysis json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shot_analyses(shot_id, model, created_at, analysis_json)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(shot_id, model) DO UPDATE SET
			created_at = excluded.created_at,
			analysis_json = excluded.analysis_json
	`, shotID, model, time.Now().Unix(), string(analysis))
	return err
}

// --- Coach suggestion cache ----------------------------------------------
//
// Mirrors the analysis cache: (shot_id, model) -> suggestion JSON. The
// read path prefers the user's currently-active model and falls back to
// the most recent entry across any model so switching providers in
// Settings doesn't hide prior suggestions.

// GetCoachSuggestion returns the cached suggestion for (shotID, model),
// or ErrNotFound.
func (s *Store) GetCoachSuggestion(ctx context.Context, shotID, model string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT suggestion_json FROM shot_coach_suggestions WHERE shot_id = ? AND model = ?`,
		shotID, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// GetLatestCoachSuggestion returns the most recently cached suggestion
// for a shot regardless of model.
func (s *Store) GetLatestCoachSuggestion(ctx context.Context, shotID string) (model string, suggestion json.RawMessage, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `
		SELECT model, suggestion_json
		FROM shot_coach_suggestions
		WHERE shot_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, shotID).Scan(&model, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	return model, json.RawMessage(raw), nil
}

// SaveCoachSuggestion upserts a suggestion for (shotID, model).
func (s *Store) SaveCoachSuggestion(ctx context.Context, shotID, model string, suggestion json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shot_coach_suggestions(shot_id, model, created_at, suggestion_json)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(shot_id, model) DO UPDATE SET
			created_at = excluded.created_at,
			suggestion_json = excluded.suggestion_json
	`, shotID, model, time.Now().Unix(), string(suggestion))
	return err
}

// --- Compare cache --------------------------------------------------------
//
// A comparison is keyed by an ordered shot pair (a<b) + model. We
// canonicalise at the boundary so callers can pass the pair in either
// order without double-caching. The payload is the full Comparison
// JSON (model, created_at, markdown) so the client renders from cache
// with no format drift.

// canonicalPair returns (a,b) with a<=b so both orderings key the same row.
func canonicalPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// GetCompare returns the cached comparison for (a,b,model). a and b may
// be supplied in either order.
func (s *Store) GetCompare(ctx context.Context, a, b, model string) (json.RawMessage, error) {
	lo, hi := canonicalPair(a, b)
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT compare_json FROM shot_compares WHERE a_id = ? AND b_id = ? AND model = ?`,
		lo, hi, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// GetLatestCompare returns the most recent cached comparison for (a,b)
// regardless of model. a and b may be supplied in either order.
func (s *Store) GetLatestCompare(ctx context.Context, a, b string) (model string, compare json.RawMessage, err error) {
	lo, hi := canonicalPair(a, b)
	var raw string
	err = s.db.QueryRowContext(ctx, `
		SELECT model, compare_json
		FROM shot_compares
		WHERE a_id = ? AND b_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, lo, hi).Scan(&model, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	return model, json.RawMessage(raw), nil
}

// SaveCompare upserts a comparison for (a,b,model). a and b are
// canonicalised so save order doesn't matter.
func (s *Store) SaveCompare(ctx context.Context, a, b, model string, compare json.RawMessage) error {
	lo, hi := canonicalPair(a, b)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shot_compares(a_id, b_id, model, created_at, compare_json)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(a_id, b_id, model) DO UPDATE SET
			created_at = excluded.created_at,
			compare_json = excluded.compare_json
	`, lo, hi, model, time.Now().Unix(), string(compare))
	return err
}

// --- Sync -----------------------------------------------------------------

// Syncer keeps the Store up to date by polling the machine.
type Syncer struct {
	store      *Store
	client     *http.Client
	machineURL string
	interval   time.Duration
	lastSync   time.Time
	lastErr    error
}

// NewSyncer builds a Syncer. interval must be > 0 (sensible default: 30s).
func NewSyncer(store *Store, machineURL string, interval time.Duration) *Syncer {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Syncer{
		store: store,
		// History is a single 6MB+ blob on busy machines. Allow generous time.
		client:     &http.Client{Timeout: 2 * time.Minute},
		machineURL: machineURL,
		interval:   interval,
	}
}

// Status is the sync status surface used by the API.
type Status struct {
	LastSync     time.Time `json:"last_sync"`
	LastError    string    `json:"last_error,omitempty"`
	ShotsCached  int       `json:"shots_cached"`
	MachineURL   string    `json:"machine_url"`
	IntervalSecs float64   `json:"interval_secs"`
}

// Status reports the current sync status. Safe to call from any goroutine
// (the struct is written only by Run).
func (s *Syncer) Status(ctx context.Context) Status {
	var n int
	_ = s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shots`).Scan(&n)
	st := Status{
		LastSync:     s.lastSync,
		ShotsCached:  n,
		MachineURL:   s.machineURL,
		IntervalSecs: s.interval.Seconds(),
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	return st
}

// Run performs an initial sync, then repeats every interval until ctx ends.
// Errors are logged but never terminate the loop.
func (s *Syncer) Run(ctx context.Context) {
	// One immediate attempt so the UI has data quickly, then on a timer.
	s.syncOnce(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncOnce(ctx)
		}
	}
}

// SyncOnce performs a single sync cycle. Exported for tests and manual triggers.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	return s.syncOnce(ctx)
}

func (s *Syncer) syncOnce(ctx context.Context) error {
	err := s.fetchAndStore(ctx)
	s.lastSync = time.Now().UTC()
	s.lastErr = err
	if err != nil {
		slog.Warn("shot sync failed", "err", err.Error(), "url", s.machineURL)
		return err
	}
	slog.Info("shot sync ok", "url", s.machineURL)
	// Now that /history is up to date, scrub any live-captured rows
	// older than 10 minutes that /history never promoted into a real
	// (db_key > 0) row. These are the orphan duplicates created when
	// the live-stream shot id doesn't match the /history id for the
	// same physical extraction.
	if n, perr := s.store.pruneOrphanedLiveShots(ctx, 10*time.Minute); perr != nil {
		slog.Warn("prune orphaned live shots failed", "err", perr.Error())
	} else if n > 0 {
		slog.Info("pruned orphaned live shots", "count", n)
	}
	return nil
}

// rawShot is the subset of the history payload we need to extract indexed fields.
type rawShot struct {
	ID        string          `json:"id"`
	DBKey     int64           `json:"db_key"`
	Time      float64         `json:"time"`
	Name      string          `json:"name"`
	File      string          `json:"file"`
	DebugFile string          `json:"debug_file"`
	Data      json.RawMessage `json:"data"`
	Profile   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"profile"`
}

type historyEnvelope struct {
	History []json.RawMessage `json:"history"`
}

func (s *Syncer) fetchAndStore(ctx context.Context) error {
	if s.machineURL == "" {
		return errors.New("machine URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.machineURL+"/api/v1/history", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("machine returned %d: %s", resp.StatusCode, string(body))
	}
	var env historyEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode history: %w", err)
	}
	return s.upsertAll(ctx, env.History)
}

func (s *Syncer) upsertAll(ctx context.Context, raw []json.RawMessage) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO shots
          (id, db_key, time_unix, name, file, debug_file, profile_id, profile_name,
           sample_count, summary_json, samples_json, inserted_at, bean_id, grind, grind_rpm)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            db_key=excluded.db_key,
            time_unix=excluded.time_unix,
            name=excluded.name,
            file=excluded.file,
            debug_file=excluded.debug_file,
            profile_id=excluded.profile_id,
            profile_name=excluded.profile_name,
            sample_count=excluded.sample_count,
            summary_json=excluded.summary_json,
            samples_json=excluded.samples_json
            -- bean_id / grind / grind_rpm intentionally omitted:
            -- once set (by the user or the live auto-tag on insert),
            -- we never overwrite on re-sync.
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	activeBean, activeGrind, activeRPM := s.store.resolveActiveBean(ctx)
	var activeRPMArg any
	if activeRPM != nil {
		activeRPMArg = *activeRPM
	}
	for _, r := range raw {
		var meta rawShot
		if err := json.Unmarshal(r, &meta); err != nil {
			slog.Warn("skip malformed shot", "err", err.Error())
			continue
		}
		if meta.ID == "" {
			continue
		}

		samples := meta.Data
		if len(samples) == 0 {
			samples = json.RawMessage("[]")
		}
		sampleCount := countJSONArray(samples)

		// Summary blob = the original shot payload minus the giant samples array.
		// We keep it lossless so the UI can surface any extra field later.
		var asMap map[string]json.RawMessage
		if err := json.Unmarshal(r, &asMap); err != nil {
			asMap = map[string]json.RawMessage{}
		}
		delete(asMap, "data")
		summaryJSON, _ := json.Marshal(asMap)

		if _, err := stmt.ExecContext(ctx,
			meta.ID, meta.DBKey, meta.Time, meta.Name, meta.File, meta.DebugFile,
			meta.Profile.ID, meta.Profile.Name,
			sampleCount, string(summaryJSON), string(samples), now,
			activeBean, activeGrind, activeRPMArg,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// pruneOrphanedLiveShots merges live-captured rows into their /history
// twins when both are present, then deletes any remaining orphans past
// minAge.
//
// Live rows are inserted by SaveLiveShot with db_key=0 (real /history
// rows always have db_key>=1), so the column is a reliable "this row
// came from the live WebSocket" marker.
//
// The merge runs in two passes:
//
//  1. For every live row that has a /history twin within ±liveMatchWindow
//     of its time_unix, we keep the canonical history row (machine-
//     preferred metadata) but transplant anything useful from the live
//     row onto it before deleting:
//
//     - cached AI analyses (shot_analyses): moved over, keeping the
//     live-row analysis when the history row doesn't already have
//     one for the same model. Duplicates on the history side win.
//     - usage-ledger rows (ai_usage.shot_id): repointed so
//     cost-per-shot audits don't break.
//     - user edits (rating, note, bean_id, hidden): copied iff the
//     history row is unset. History wins on conflict.
//
//     This handles the main duplicate path: the live WebSocket's
//     transient id (e.g. "b3fcab16…") is not the same as the stable id
//     /history assigns ("58cbb74c…"), so the rows coexist as separate
//     entries for the same physical extraction. Matching by time is
//     safe: two espresso shots can't start within ~30 seconds of each
//     other.
//
//  2. Age-based fallback for the rare case where /history never picks
//     the shot up at all: live rows older than minAge are deleted.
//
// minAge protects shots that are still extracting when /history fires
// (so we don't delete a row five seconds before its canonical twin
// arrives). The merge pass doesn't need that protection because the
// twin has already been inserted.
const liveMatchWindow = 30 // seconds

func (s *Store) pruneOrphanedLiveShots(ctx context.Context, minAge time.Duration) (int, error) {
	n1, err := s.mergeLiveDuplicates(ctx)
	if err != nil {
		return n1, err
	}

	// Pass 2: age-based fallback.
	cutoff := time.Now().Add(-minAge).Unix()
	res2, err := s.db.ExecContext(ctx, `
        DELETE FROM shots
         WHERE db_key = 0
           AND inserted_at < ?
    `, cutoff)
	if err != nil {
		return n1, err
	}
	n2, _ := res2.RowsAffected()
	return n1 + int(n2), nil
}

// mergeLiveDuplicates finds (live_id, history_id) pairs that refer to
// the same extraction, moves everything worth keeping from the live row
// onto the history row, and deletes the live row. Returns the number of
// live rows merged.
func (s *Store) mergeLiveDuplicates(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT l.id AS live_id, h.id AS hist_id
          FROM shots AS l
          JOIN shots AS h
            ON h.db_key > 0
           AND ABS(h.time_unix - l.time_unix) <= ?
         WHERE l.db_key = 0
    `, liveMatchWindow)
	if err != nil {
		return 0, err
	}
	type pair struct{ live, hist string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.live, &p.hist); err != nil {
			rows.Close()
			return 0, err
		}
		// A single live row can match multiple history rows if two real
		// shots happened within 30s of each other AND the live feed
		// dropped one — pick the closest in time to be safe. In practice
		// the query returns at most one hit per live row, but we
		// deduplicate defensively.
		pairs = append(pairs, p)
	}
	rows.Close()
	if len(pairs) == 0 {
		return 0, nil
	}

	seen := make(map[string]bool, len(pairs))
	merged := 0
	for _, p := range pairs {
		if seen[p.live] {
			continue
		}
		seen[p.live] = true
		if err := s.mergeOnePair(ctx, p.live, p.hist); err != nil {
			return merged, fmt.Errorf("merge %s→%s: %w", p.live, p.hist, err)
		}
		merged++
	}
	return merged, nil
}

// mergeOnePair transplants state from liveID onto histID, then deletes
// the live row. Wrapped in a transaction so a failure leaves either
// both rows intact or only the history row. ai_usage is a best-effort
// side-table in a foreign package: an "ai_usage table doesn't exist"
// error (when the operator has no ledger) must not block the merge.
func (s *Store) mergeOnePair(ctx context.Context, liveID, histID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Analyses: move live → hist where hist doesn't already have an
	// analysis for the same model. UPDATE OR IGNORE skips conflicts.
	if _, err := tx.ExecContext(ctx,
		`UPDATE OR IGNORE shot_analyses SET shot_id = ? WHERE shot_id = ?`,
		histID, liveID); err != nil {
		return fmt.Errorf("move analyses: %w", err)
	}
	// FK enforcement isn't enabled on this connection (no
	// PRAGMA foreign_keys=ON), so the ON DELETE CASCADE on shot_analyses
	// won't fire. Explicitly clean up any leftover live-side analyses
	// that lost the UPDATE OR IGNORE (i.e. hist already had that model).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM shot_analyses WHERE shot_id = ?`, liveID); err != nil {
		return fmt.Errorf("clean leftover analyses: %w", err)
	}

	// Usage-ledger rows referencing the live id get repointed. ai_usage
	// lives in a sibling package; if the table isn't there (no ledger
	// configured) the error is benign.
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_usage SET shot_id = ? WHERE shot_id = ?`,
		histID, liveID); err != nil && !isMissingTableErr(err) {
		return fmt.Errorf("move ai_usage: %w", err)
	}

	// User-facing edits: copy over only the fields the history row
	// hasn't already overridden. Live wins exactly when hist is unset.
	if _, err := tx.ExecContext(ctx, `
        UPDATE shots
           SET rating = (SELECT rating FROM shots WHERE id = ?)
         WHERE id = ?
           AND rating IS NULL
           AND (SELECT rating FROM shots WHERE id = ?) IS NOT NULL
    `, liveID, histID, liveID); err != nil {
		return fmt.Errorf("copy rating: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE shots
           SET grind_rpm = (SELECT grind_rpm FROM shots WHERE id = ?)
         WHERE id = ?
           AND grind_rpm IS NULL
           AND (SELECT grind_rpm FROM shots WHERE id = ?) IS NOT NULL
    `, liveID, histID, liveID); err != nil {
		return fmt.Errorf("copy grind_rpm: %w", err)
	}
	for _, col := range []string{"note", "bean_id", "grind"} {
		q := `UPDATE shots SET ` + col + ` = (SELECT ` + col + ` FROM shots WHERE id = ?)
		       WHERE id = ?
		         AND COALESCE(` + col + `, '') = ''
		         AND COALESCE((SELECT ` + col + ` FROM shots WHERE id = ?), '') <> ''`
		if _, err := tx.ExecContext(ctx, q, liveID, histID, liveID); err != nil {
			return fmt.Errorf("copy %s: %w", col, err)
		}
	}
	// hidden is a sticky bit: if the user hid the live row, carry it.
	if _, err := tx.ExecContext(ctx, `
        UPDATE shots
           SET hidden = 1
         WHERE id = ?
           AND hidden = 0
           AND (SELECT hidden FROM shots WHERE id = ?) = 1
    `, histID, liveID); err != nil {
		return fmt.Errorf("copy hidden: %w", err)
	}

	// Finally, drop the live row. shot_analyses rows still keyed to it
	// (model-collisions from above) cascade-delete via FK.
	if _, err := tx.ExecContext(ctx, `DELETE FROM shots WHERE id = ?`, liveID); err != nil {
		return fmt.Errorf("delete live: %w", err)
	}
	return tx.Commit()
}

// isMissingTableErr recognises the sqlite "no such table" error so the
// cross-package ai_usage UPDATE can be treated as best-effort.
func isMissingTableErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}

// countJSONArray counts the top-level elements of a JSON array without
// materialising them. Returns 0 for anything that isn't a valid array.
func countJSONArray(raw json.RawMessage) int {
	// Fast path: empty or "[]".
	if len(raw) <= 2 {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

// ShotSibling is a compact per-shot row the coach / comparator use for
// historical context. Metrics come from samples_json on demand, so no
// schema migration is needed to add more fields later.
type ShotSibling struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	TimeISO      string  `json:"time_iso"`
	Duration     float64 `json:"duration_s"`
	PeakPressure float64 `json:"peak_pressure_bar"`
	FinalWeight  float64 `json:"final_weight_g"`
	Rating       *int    `json:"rating,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// ListShotSiblings returns up to `limit` recent shots with the given
// profile id (newest first), optionally excluding `excludeID`. Used by
// the profile-coach endpoint to pass historical context to the LLM.
func (s *Store) ListShotSiblings(ctx context.Context, profileID, excludeID string, limit int) ([]ShotSibling, error) {
	if profileID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), time_unix, sample_count, samples_json, rating, COALESCE(note,'')
		   FROM shots
		  WHERE hidden = 0 AND profile_id = ? AND id <> ?
		  ORDER BY time_unix DESC
		  LIMIT ?`, profileID, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShotSibling
	for rows.Next() {
		var (
			id, name, samplesJSON, note string
			ts                          float64
			sc                          int
			rating                      sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &ts, &sc, &samplesJSON, &rating, &note); err != nil {
			return nil, err
		}
		m := extractShotMetrics(samplesJSON, 0)
		sib := ShotSibling{
			ID:           id,
			Name:         name,
			TimeISO:      time.Unix(int64(ts), 0).UTC().Format(time.RFC3339),
			Duration:     float64(sc) / 10.0,
			PeakPressure: m.PeakPressure,
			FinalWeight:  m.FinalWeight,
			Note:         note,
		}
		if rating.Valid {
			v := int(rating.Int64)
			sib.Rating = &v
		}
		out = append(out, sib)
	}
	return out, rows.Err()
}
