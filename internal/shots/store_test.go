// Package shots tests.
package shots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStoreAndSyncRoundtrip(t *testing.T) {
	// A fake machine serving a minimal /api/v1/history payload.
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "aaaa",
				"db_key":  1,
				"time":    1_700_000_000.5,
				"name":    "Test Shot",
				"file":    "2026-04-17/10:17:54.shot.json.zst",
				"profile": map[string]any{"id": "p1", "name": "Espresso"},
				"data": []any{
					map[string]any{"time": 0, "shot": map[string]any{"pressure": 0.0}},
					map[string]any{"time": 1, "shot": map[string]any{"pressure": 6.2}},
				},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/history" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	sync := NewSyncer(store, upstream.URL, 1*time.Second)
	if err := sync.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	list, err := store.ListShots(context.Background(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "aaaa" || list[0].ProfileName != "Espresso" || list[0].SampleCount != 2 {
		t.Fatalf("unexpected list: %+v", list)
	}

	full, err := store.GetShot(context.Background(), "aaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if full.SampleCount != 2 {
		t.Fatalf("want 2 samples, got %d", full.SampleCount)
	}
	// Second sync should be idempotent (upsert, not duplicate).
	if err := sync.SyncOnce(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	list, _ = store.ListShots(context.Background(), 10)
	if len(list) != 1 {
		t.Fatalf("upsert produced dup: %+v", list)
	}
}

func TestSyncHandlesNon2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "boom")
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	s := NewSyncer(store, upstream.URL, time.Second)
	err = s.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("want error on non-2xx")
	}
}

// TestSaveLiveShot_DoesNotClobberHistoryRow verifies that once the
// canonical /history sync has stored a rich row, a subsequent live
// capture for the same id leaves the history-sourced fields (name,
// file, profile_id/name, sample_count) untouched.
func TestSaveLiveShot_DoesNotClobberHistoryRow(t *testing.T) {
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "same-id",
				"db_key":  7,
				"time":    1_700_000_001.0,
				"name":    "Canonical",
				"file":    "2026/10.shot.json.zst",
				"profile": map[string]any{"id": "p1", "name": "FromHistory"},
				"data": []any{
					map[string]any{"time": 0, "shot": map[string]any{"pressure": 1}},
					map[string]any{"time": 1, "shot": map[string]any{"pressure": 5}},
					map[string]any{"time": 2, "shot": map[string]any{"pressure": 9}},
				},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Now try to save a live shot with the same id — should no-op.
	err = store.SaveLiveShot(context.Background(), LiveShotInput{
		ID:          "same-id",
		Time:        1_700_000_002.0,
		Name:        "FromLive",
		ProfileName: "FromLive",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":7}}]`),
	})
	if err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}

	got, err := store.GetShot(context.Background(), "same-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Canonical" || got.ProfileName != "FromHistory" || got.SampleCount != 3 {
		t.Fatalf("history row was overwritten: %+v", got.ShotListItem)
	}
}

// TestSaveLiveShot_InsertsNewRow verifies the happy path: when no
// history row exists yet, SaveLiveShot persists the shot so the UI
// has something to show immediately.
func TestSaveLiveShot_InsertsNewRow(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	err = store.SaveLiveShot(context.Background(), LiveShotInput{
		ID:          "live-only",
		Time:        1_700_000_099.0,
		Name:        "Live",
		ProfileName: "Live",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":3}},{"time":1,"shot":{"pressure":8}}]`),
	})
	if err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}
	got, err := store.GetShot(context.Background(), "live-only")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SampleCount != 2 || got.Name != "Live" {
		t.Fatalf("unexpected row: %+v", got.ShotListItem)
	}
}

// TestSyncPrunesLiveDuplicateByTime verifies that when the live
// recorder and /history end up with two different ids for the same
// physical extraction, the next sync eagerly prunes the live row
// based on a matching time_unix — without waiting the 10-minute
// age fallback.
func TestSyncPrunesLiveDuplicateByTime(t *testing.T) {
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "canonical-id",
				"db_key":  42,
				"time":    1_700_000_500.0,
				"name":    "Canonical",
				"file":    "2026/shot.json.zst",
				"profile": map[string]any{"id": "p1", "name": "Espresso"},
				"data": []any{
					map[string]any{"time": 0, "shot": map[string]any{"pressure": 6}},
				},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Live recorder inserts with a different id but the SAME time_unix
	// (±5s). This simulates the ws-shot-id vs /history-shot-id mismatch.
	if err := store.SaveLiveShot(context.Background(), LiveShotInput{
		ID:          "transient-live-id",
		Time:        1_700_000_503.0, // 3s off canonical
		Name:        "LiveName",
		ProfileName: "Espresso",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":5}}]`),
	}); err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}

	list, _ := store.ListShots(context.Background(), 10)
	if len(list) != 1 {
		t.Fatalf("pre-sync: want 1 row, got %d", len(list))
	}

	// Sync brings in the canonical row and should immediately prune
	// the live twin — even though inserted_at is well under 10 minutes.
	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	list, _ = store.ListShots(context.Background(), 10)
	if len(list) != 1 {
		t.Fatalf("post-sync: want 1 row (dedupe), got %d: %+v", len(list), list)
	}
	if list[0].ID != "canonical-id" {
		t.Fatalf("wrong survivor: %+v", list[0])
	}
}

// TestSyncKeepsUnrelatedLiveShot verifies the time-match prune doesn't
// nuke a genuinely live-only row whose time_unix is far from any
// /history row.
func TestSyncKeepsUnrelatedLiveShot(t *testing.T) {
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "history-only",
				"db_key":  1,
				"time":    1_700_000_000.0,
				"name":    "HistOnly",
				"file":    "f",
				"profile": map[string]any{"id": "p1", "name": "X"},
				"data":    []any{map[string]any{"time": 0, "shot": map[string]any{"pressure": 6}}},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Live shot 10 minutes away from the history row — not a duplicate.
	if err := store.SaveLiveShot(context.Background(), LiveShotInput{
		ID:          "live-standalone",
		Time:        1_700_000_600.0,
		Name:        "Live",
		ProfileName: "X",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":5}}]`),
	}); err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}
	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	list, _ := store.ListShots(context.Background(), 10)
	if len(list) != 2 {
		t.Fatalf("want both rows kept, got %d: %+v", len(list), list)
	}
}

// TestSyncMergesLiveAnalysisIntoHistory verifies that when a live row
// has a cached AI analysis and is about to be pruned as a duplicate of
// a /history twin, the analysis is transplanted onto the surviving
// history id so the Shots detail view doesn't lose it.
func TestSyncMergesLiveAnalysisIntoHistory(t *testing.T) {
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "canonical-id",
				"db_key":  7,
				"time":    1_700_000_500.0,
				"name":    "Canonical",
				"file":    "f",
				"profile": map[string]any{"id": "p1", "name": "Espresso"},
				"data":    []any{map[string]any{"time": 0, "shot": map[string]any{"pressure": 6}}},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.SaveLiveShot(ctx, LiveShotInput{
		ID:          "transient-live-id",
		Time:        1_700_000_501.0,
		Name:        "Live",
		ProfileName: "Espresso",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":5}}]`),
	}); err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}

	// Simulate auto-analyze firing against the live id before /history
	// landed — this is the state that used to orphan analyses.
	want := json.RawMessage(`{"summary":"ok","score":7}`)
	if err := store.SaveAnalysis(ctx, "transient-live-id", "gemini:test", want); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}

	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Live row gone, history row survives, analysis now keyed to it.
	list, _ := store.ListShots(ctx, 10)
	if len(list) != 1 || list[0].ID != "canonical-id" {
		t.Fatalf("post-sync rows: %+v", list)
	}
	got, err := store.GetAnalysis(ctx, "canonical-id", "gemini:test")
	if err != nil {
		t.Fatalf("GetAnalysis on survivor: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("analysis lost in merge: got %s want %s", got, want)
	}
	// And the orphan row is gone.
	orphan, err := store.GetAnalysis(ctx, "transient-live-id", "gemini:test")
	if err == nil && len(orphan) > 0 {
		t.Fatalf("live-side analysis should be gone, got %s", orphan)
	}
}

// TestSyncMergePrefersHistoryAnalysisOnConflict verifies that when both
// the live and history rows already have an analysis for the same
// model, the history side wins (machine/canonical row takes precedence
// on the shot metadata; analysis attached to the canonical id is
// presumed newer/authoritative).
func TestSyncMergePrefersHistoryAnalysisOnConflict(t *testing.T) {
	body := map[string]any{
		"history": []any{
			map[string]any{
				"id":      "canonical-id",
				"db_key":  7,
				"time":    1_700_000_500.0,
				"name":    "Canonical",
				"file":    "f",
				"profile": map[string]any{"id": "p1", "name": "Espresso"},
				"data":    []any{map[string]any{"time": 0, "shot": map[string]any{"pressure": 6}}},
			},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	store, _ := OpenStore(":memory:")
	defer store.Close()
	ctx := context.Background()

	if err := store.SaveLiveShot(ctx, LiveShotInput{
		ID: "transient", Time: 1_700_000_501.0, Name: "L",
		ProfileName: "Espresso",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":5}}]`),
	}); err != nil {
		t.Fatalf("SaveLiveShot: %v", err)
	}
	_ = store.SaveAnalysis(ctx, "transient", "m", json.RawMessage(`{"from":"live"}`))

	// Pre-seed the history-side analysis by running sync once to insert
	// the canonical row (which also prunes the live row, but we want to
	// test the conflict path, so we re-add the live row after).
	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(ctx); err != nil {
		t.Fatalf("sync1: %v", err)
	}
	_ = store.SaveAnalysis(ctx, "canonical-id", "m", json.RawMessage(`{"from":"hist"}`))
	if err := store.SaveLiveShot(ctx, LiveShotInput{
		ID: "transient2", Time: 1_700_000_502.0, Name: "L2",
		ProfileName: "Espresso",
		Samples:     json.RawMessage(`[{"time":0,"shot":{"pressure":5}}]`),
	}); err != nil {
		t.Fatalf("SaveLiveShot2: %v", err)
	}
	_ = store.SaveAnalysis(ctx, "transient2", "m", json.RawMessage(`{"from":"live2"}`))

	if err := NewSyncer(store, upstream.URL, time.Second).SyncOnce(ctx); err != nil {
		t.Fatalf("sync2: %v", err)
	}

	got, err := store.GetAnalysis(ctx, "canonical-id", "m")
	if err != nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if string(got) != `{"from":"hist"}` {
		t.Fatalf("history analysis should win, got %s", got)
	}
}

// TestGetLatestAnalysisFallsBackAcrossModels verifies that switching
// the active model in Settings doesn't hide an existing analysis from
// the user — GetLatestAnalysis returns the most recent result under
// any model, along with which model produced it.
func TestGetLatestAnalysisFallsBackAcrossModels(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	older := json.RawMessage(`{"summary":"old","model":"openai:gpt-4o-mini"}`)
	if err := store.SaveAnalysis(ctx, "s1", "openai:gpt-4o-mini", older); err != nil {
		t.Fatalf("SaveAnalysis old: %v", err)
	}
	// Ensure the second row has a strictly later created_at. SaveAnalysis
	// uses unix seconds, so sleep a hair over a second or manually bump.
	time.Sleep(1100 * time.Millisecond)
	newer := json.RawMessage(`{"summary":"new","model":"gemini:gemini-2.5-flash"}`)
	if err := store.SaveAnalysis(ctx, "s1", "gemini:gemini-2.5-flash", newer); err != nil {
		t.Fatalf("SaveAnalysis new: %v", err)
	}

	model, got, err := store.GetLatestAnalysis(ctx, "s1")
	if err != nil {
		t.Fatalf("GetLatestAnalysis: %v", err)
	}
	if model != "gemini:gemini-2.5-flash" {
		t.Fatalf("want latest model=gemini:gemini-2.5-flash, got %q", model)
	}
	if string(got) != string(newer) {
		t.Fatalf("want newest analysis, got %s", got)
	}

	// Shot with no analysis at all → ErrNotFound.
	if _, _, err := store.GetLatestAnalysis(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
