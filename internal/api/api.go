// Package api wires the HTTP handlers for caffeine.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/apohor/caffeine/internal/ai"
	"github.com/apohor/caffeine/internal/beans"
	"github.com/apohor/caffeine/internal/config"
	"github.com/apohor/caffeine/internal/live"
	"github.com/apohor/caffeine/internal/machine"
	"github.com/apohor/caffeine/internal/preheat"
	"github.com/apohor/caffeine/internal/profileimages"
	"github.com/apohor/caffeine/internal/push"
	"github.com/apohor/caffeine/internal/settings"
	"github.com/apohor/caffeine/internal/shots"
	"github.com/google/uuid"
)

// Deps bundles the runtime collaborators the API needs. Fields may be nil.
type Deps struct {
	ShotStore        *shots.Store
	ShotSyncer       *shots.Syncer
	LiveHub          *live.Hub
	AISettings       *settings.Manager
	PreheatStore     *preheat.Store
	PreheatScheduler *preheat.Scheduler
	PushStore        *push.Store
	PushService      *push.Service
	ProfileImages    *profileimages.Store
	AIRecorder       *ai.Recorder
	Beans            *beans.Store
}

// New builds the root http.Handler. webAssets is the embedded web app (may be nil in tests).
func New(cfg config.Config, webAssets fs.FS, deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Local development: Vite dev server (5173) may call the API directly.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:5173", "http://127.0.0.1:5173"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Route("/api", func(r chi.Router) {
		// Per-request timeout applies only to conventional JSON endpoints;
		// long-lived WebSockets and the machine reverse-proxy are mounted
		// on the parent router below.
		r.Use(middleware.Timeout(30 * time.Second))
		r.Get("/health", handleHealth)
		r.Get("/config", handleConfig(cfg))

		// Shot history (served from local SQLite cache).
		if deps.ShotStore != nil {
			r.Get("/shots", handleShotsList(deps.ShotStore))
			r.Get("/shots/sparklines", handleShotsSparklines(deps.ShotStore))
			r.Get("/shots/metrics", handleShotsMetrics(deps.ShotStore))
			r.Get("/shots/{id}", handleShotGet(deps.ShotStore))
			r.Delete("/shots/{id}", handleShotDelete(deps.ShotStore))
			r.Put("/shots/{id}/feedback", handleShotFeedback(deps.ShotStore))
			r.Post("/shots/sync", handleShotsSync(deps.ShotSyncer))
			r.Get("/shots/status", handleShotsStatus(deps.ShotSyncer))
			r.Get("/shots/{id}/analysis", handleAnalysisGet(deps.ShotStore, deps.AISettings))
			r.Get("/shots/{id}/coach", handleShotCoachGet(deps.ShotStore, deps.AISettings))
			r.Get("/shots/compare", handleShotsCompareGet(deps.ShotStore, deps.AISettings))
			// Analysis POST can take much longer than the default 30s route
			// timeout: reasoning models (gemini-3-pro-preview, o1, claude
			// extended-thinking) routinely take 60-120s on first-token, and
			// the analyzer retries transient provider errors with
			// exponential backoff. Mount it in a nested group with a
			// 5-minute route timeout, matching the handler's independent
			// analyze context. The per-provider HTTP client timeout (3min)
			// still bounds each individual call.
			r.Group(func(r chi.Router) {
				r.Use(middleware.Timeout(5 * time.Minute))
				r.Post("/shots/{id}/analysis", handleAnalysisCreate(deps.ShotStore, deps.Beans, deps.AISettings, deps.AIRecorder))
				r.Post("/shots/{id}/coach", handleShotCoach(deps.ShotStore, deps.Beans, deps.AISettings, deps.AIRecorder))
				r.Post("/shots/compare", handleShotsCompare(deps.ShotStore, deps.Beans, deps.AISettings, deps.AIRecorder))
			})
		}

		// App-level settings (currently just AI).
		if deps.AISettings != nil {
			r.Get("/settings/ai", handleAISettingsGet(deps.AISettings))
			r.Put("/settings/ai", handleAISettingsPut(deps.AISettings))
			r.Get("/settings/ai/models/{provider}", handleAIModelsList(deps.AISettings))
			// Voice-note transcription. Whisper can take 30s+ on a long
			// clip, so mount it in its own timeout group.
			r.Group(func(r chi.Router) {
				r.Use(middleware.Timeout(3 * time.Minute))
				r.Post("/ai/transcribe", handleAITranscribe(deps.AISettings))
				r.Post("/ai/beans/from-image", handleAIBeanFromImage(deps.AISettings, deps.AIRecorder))
			})
		}

		// AI usage dashboard data. Cheap SQLite aggregates — ok to
		// serve from the default 30s timeout group.
		if deps.AIRecorder != nil {
			r.Get("/ai/usage", handleAIUsage(deps.AIRecorder))
		}

		// AI profile-name suggestion (used on the Import form). Quick
		// call (under 10s typically) so the default timeout is fine.
		if deps.AISettings != nil {
			r.Post("/ai/profile-name", handleProfileNameSuggest(deps.AISettings, deps.AIRecorder))
		}

		// Beans CRUD.
		if deps.Beans != nil {
			r.Get("/beans", handleBeansList(deps.Beans))
			r.Post("/beans", handleBeansCreate(deps.Beans))
			r.Get("/beans/{id}", handleBeansGet(deps.Beans))
			r.Put("/beans/{id}", handleBeansUpdate(deps.Beans))
			r.Delete("/beans/{id}", handleBeansDelete(deps.Beans))
			// Mark a bean as the "bag currently in use". New shots get
			// auto-tagged with this id until cleared / swapped.
			r.Put("/beans/{id}/active", handleBeansSetActive(deps.Beans))
			r.Delete("/beans/active", handleBeansClearActive(deps.Beans))
			// Attach a bean (or clear it with "") on a shot.
			if deps.ShotStore != nil {
				r.Put("/shots/{id}/bean", handleShotSetBean(deps.ShotStore))
			}
		}

		// Grinder setting + RPM per shot. Lives next to the other
		// shot-edit endpoints; lightweight, so default timeout is fine.
		if deps.ShotStore != nil {
			r.Put("/shots/{id}/grind", handleShotSetGrind(deps.ShotStore))
		}

		// AI-generated profile artwork. Stored on the local filesystem;
		// the SPA overlays these over the machine's built-in images.
		if deps.ProfileImages != nil {
			r.Get("/profiles/images", handleProfileImageList(deps.ProfileImages))
			r.Get("/profiles/{id}/image", handleProfileImageGet(deps.ProfileImages))
			r.Put("/profiles/{id}/image", handleProfileImageUpload(deps.ProfileImages))
			r.Delete("/profiles/{id}/image", handleProfileImageDelete(deps.ProfileImages))
			// Image generation routinely takes 20-60s; give it its own
			// 5-minute timeout to override the 30s default for POSTs.
			r.Group(func(r chi.Router) {
				r.Use(middleware.Timeout(5 * time.Minute))
				r.Post("/profiles/{id}/image/generate",
					handleProfileImageGenerate(cfg.MachineURL, deps.ProfileImages, deps.AISettings))
			})
		}

		// Preheat schedules + manual trigger.
		if deps.PreheatStore != nil && deps.PreheatScheduler != nil {
			r.Get("/preheat/schedules", handlePreheatList(deps.PreheatStore))
			r.Post("/preheat/schedules", handlePreheatCreate(deps.PreheatStore))
			r.Put("/preheat/schedules/{id}", handlePreheatUpdate(deps.PreheatStore))
			r.Delete("/preheat/schedules/{id}", handlePreheatDelete(deps.PreheatStore))
			r.Get("/preheat/status", handlePreheatStatus(deps.PreheatScheduler))
			r.Post("/preheat/now", handlePreheatNow(deps.PreheatScheduler))
		}

		// Live shot feed (browser WebSocket; bypasses the 30s chi timeout).
		if deps.LiveHub != nil {
			r.Get("/live/status", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, deps.LiveHub.State())
			})
		}

		// Web push subscriptions. The VAPID key is always readable (even
		// if push is disabled) so the frontend can show a consistent
		// "push unavailable" state rather than tripping on a 404.
		if deps.PushStore != nil {
			r.Get("/push/vapid-public-key", handlePushVAPIDKey(deps.PushService))
			r.Get("/push/status", handlePushStatus(deps.PushService, deps.PushStore))
			r.Post("/push/subscribe", handlePushSubscribe(deps.PushStore))
			r.Post("/push/unsubscribe", handlePushUnsubscribe(deps.PushStore))
			if deps.PushService != nil {
				r.Post("/push/test", handlePushTest(deps.PushService))
			}
		}
	})

	// WebSocket mounted OUTSIDE the chi timeout middleware so long-lived
	// streams aren't killed after 30s.
	if deps.LiveHub != nil {
		r.Get("/api/live/ws", func(w http.ResponseWriter, r *http.Request) {
			deps.LiveHub.ServeWS(w, r)
		})
	}

	// Machine reverse-proxy mounted OUTSIDE the chi timeout middleware.
	//
	// chi's middleware.Timeout interacts badly with httputil.ReverseProxy:
	// when the 30s deadline fires it writes a 504, but the proxy doesn't
	// check ctx.Err() before its own WriteHeader, producing the noisy
	//
	//   http: superfluous response.WriteHeader call from
	//   ...internal/api.New.func1.Timeout.6.1.1 (timeout.go:39)
	//
	// The proxy already enforces its own http.Client timeout (25s) and
	// ResponseHeaderTimeout (20s), so this doesn't remove any safety net.
	if proxy, err := machine.New(cfg.MachineURL); err != nil {
		slog.Warn("machine proxy disabled", "reason", err.Error())
		r.HandleFunc("/api/machine/*", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":  "machine proxy not configured",
				"detail": err.Error(),
			})
		})
		r.Get("/api/machine-status", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"reachable":   false,
				"machine_url": cfg.MachineURL,
				"error":       err.Error(),
			})
		})
	} else {
		r.Handle("/api/machine/*", proxy.Handler())
		r.Get("/api/machine-status", func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, http.StatusOK, proxy.Status(req.Context()))
		})
	}

	// Serve embedded web assets (SPA). Unknown paths fall back to index.html.
	if webAssets != nil {
		r.Handle("/*", spaHandler(webAssets))
	}

	return r
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func handleConfig(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"machine_url": cfg.MachineURL,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func handleShotsList(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if q := r.URL.Query().Get("limit"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				limit = v
			}
		}
		items, err := store.ListShots(r.Context(), limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if items == nil {
			items = []shots.ShotListItem{}
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func handleShotsSparklines(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				limit = v
			}
		}
		points := 24
		if q := r.URL.Query().Get("points"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				points = v
			}
		}
		m, err := store.ListSparklines(r.Context(), limit, points)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if m == nil {
			m = map[string][]float64{}
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// handleShotsMetrics is the richer cousin of handleShotsSparklines: same
// list iteration, but the response also carries peak pressure and final
// weight per shot. Used by the HistoryPage row decorations.
func handleShotsMetrics(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				limit = v
			}
		}
		points := 24
		if q := r.URL.Query().Get("points"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				points = v
			}
		}
		m, err := store.ListShotMetrics(r.Context(), limit, points)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if m == nil {
			m = map[string]shots.ShotMetrics{}
		}
		writeJSON(w, http.StatusOK, m)
	}
}

func handleShotGet(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		shot, err := store.GetShot(r.Context(), id)
		if errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, shot)
	}
}

// handleShotDelete soft-deletes a shot (flips its hidden flag so it
// disappears from the history list without racing the next sync).
func handleShotDelete(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := store.HideShot(r.Context(), id); err != nil {
			if errors.Is(err, shots.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleShotFeedback persists the user's 1..5 rating + free-form note.
// Both fields are optional; omitting rating (or sending null) clears it.
func handleShotFeedback(store *shots.Store) http.HandlerFunc {
	type body struct {
		Rating *int   `json:"rating"`
		Note   string `json:"note"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var in body
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
		if err := store.SetFeedback(r.Context(), id, in.Rating, in.Note); err != nil {
			if errors.Is(err, shots.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"rating": in.Rating,
			"note":   in.Note,
		})
	}
}

func handleShotsSync(sync *shots.Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sync == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "sync disabled"})
			return
		}
		// The full history can be several MB. Give the machine up to 2
		// minutes, independent of the per-request chi timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := sync.SyncOnce(ctx); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sync.Status(r.Context()))
	}
}

func handleShotsStatus(sync *shots.Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sync == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, sync.Status(r.Context()))
	}
}

// handleAnalysisGet returns a cached analysis if one exists. Prefers an
// analysis generated with the currently-configured model; if none
// exists for that model, falls back to the most recent analysis under
// any model so a user switching providers in Settings doesn't appear
// to "lose" their existing analyses. The analysis payload itself
// carries its own `model` field so the UI can show which model the
// response came from.
func handleAnalysisGet(store *shots.Store, mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		analyzer := currentAnalyzer(mgr)
		id := chi.URLParam(r, "id")
		// If AI is configured, look up by active model first.
		if analyzer != nil {
			if raw, err := store.GetAnalysis(r.Context(), id, analyzer.ModelName()); err == nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			} else if !errors.Is(err, shots.ErrNotFound) {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		// Fall back to the latest analysis under any model. This covers
		// two cases: (a) AI is currently disabled but historical results
		// still exist, (b) the user switched models and we should still
		// surface the previous analysis.
		if _, raw, err := store.GetLatestAnalysis(r.Context(), id); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
			return
		} else if !errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if analyzer == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not analyzed"})
	}
}

// handleAnalysisCreate runs a fresh analysis and stores it. When ?cached=1
// is passed we return the existing cached value if present (cheap refresh).
func handleAnalysisCreate(store *shots.Store, beansStore *beans.Store, mgr *settings.Manager, rec *ai.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		analyzer := currentAnalyzer(mgr)
		if analyzer == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		id := chi.URLParam(r, "id")
		if r.URL.Query().Get("cached") == "1" {
			if raw, err := store.GetAnalysis(r.Context(), id, analyzer.ModelName()); err == nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			}
		}
		shot, err := store.GetShot(r.Context(), id)
		if errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Independent context: LLM calls can exceed the chi 3-minute route
		// timeout when the analyzer retries transient provider errors with
		// exponential backoff. Give it room: 5 attempts × up to 3min each
		// + 1+2+4+8s of backoff waits ≈ 15min worst case. We don't want to
		// *actually* wait 15min in practice, so cap at 5 minutes — enough
		// for one slow reasoning-model call + a couple of quick retries,
		// but short enough that a stuck provider is caught and bubbled up.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		bean, grind, rpm := beanContextForShot(r.Context(), beansStore, shot)
		analysis, err := analyzer.Analyze(ctx, ai.ShotInput{
			Name:        shot.Name,
			ProfileName: shot.ProfileName,
			Samples:     shot.Samples,
			Profile:     shot.Profile,
			Bean:        bean,
			Grind:       grind,
			GrindRPM:    rpm,
		})
		if err != nil {
			// Surface the provider error in logs \u2014 UI only shows a short
			// message; the actual cause (bad key, quota, network, bad JSON
			// from the provider) is what we usually need.
			slog.Warn("shot analysis failed",
				"shot_id", id,
				"model", analyzer.ModelName(),
				"err", err.Error(),
			)
			recordAICall(rec, analyzer.ModelName(), "analyze", id, ai.CallUsage{}, err)
			// Provider overload / rate-limit exhaustion becomes 503 so the
			// UI can show a "busy, try again later" affordance instead of
			// the generic "Analysis failed" error box. Everything else
			// (auth, bad model id, malformed response) stays 502.
			status := http.StatusBadGateway
			if ai.IsTransient(err) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		recordAICall(rec, analyzer.ModelName(), "analyze", id, analysis.Usage, nil)
		raw, err := json.Marshal(analysis)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := store.SaveAnalysis(context.Background(), id, analyzer.ModelName(), raw); err != nil {
			slog.Warn("save analysis failed", "err", err.Error())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
}

// beanContextForShot resolves a shot's bean_id into the AI-facing
// BeanInfo plus pulls the grind settings the user recorded on the
// shot. Every field is optional: if the store is nil, the id is
// empty, or the lookup fails, we just return (nil, "", nil) and the
// prompt simply omits the "Beans & grind" section. Never errors —
// the feature is additive and must never block an analysis.
func beanContextForShot(ctx context.Context, store *beans.Store, shot *shots.Shot) (*ai.BeanInfo, string, *float64) {
	if shot == nil {
		return nil, "", nil
	}
	grind := shot.Grind
	rpm := shot.GrindRPM
	if store == nil || shot.BeanID == "" {
		return nil, grind, rpm
	}
	b, err := store.Get(ctx, shot.BeanID)
	if err != nil || b == nil {
		return nil, grind, rpm
	}
	return &ai.BeanInfo{
		Name:       b.Name,
		Roaster:    b.Roaster,
		Origin:     b.Origin,
		Process:    b.Process,
		RoastLevel: b.RoastLevel,
		RoastDate:  b.RoastDate,
		Notes:      b.Notes,
	}, grind, rpm
}

// recordAICall is the tiny one-liner used by every AI-facing handler to
// append a row to the usage ledger. nil recorder / zero usage are OK —
// the recorder ignores them.
func recordAICall(rec *ai.Recorder, modelName, feature, shotID string, u ai.CallUsage, err error) {
	if rec == nil {
		return
	}
	provider, model := ai.SplitModelName(modelName)
	errStr := ""
	if err != nil {
		errStr = err.Error()
		if len(errStr) > 500 {
			errStr = errStr[:500]
		}
	}
	rec.Record(context.Background(), ai.Record{
		Time:         time.Now().UTC(),
		Provider:     provider,
		Model:        model,
		Feature:      feature,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		DurationMs:   u.DurationMs,
		ShotID:       shotID,
		OK:           err == nil,
		Err:          errStr,
	})
}

// handleShotCoach runs the profile coach for a single shot: loads the
// shot plus its recent siblings on the same profile and asks the LLM
// for one focused, actionable next-attempt suggestion.
func handleShotCoach(store *shots.Store, beansStore *beans.Store, mgr *settings.Manager, rec *ai.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled"})
			return
		}
		provider := mgr.Provider()
		if provider == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		id := chi.URLParam(r, "id")
		shot, err := store.GetShot(r.Context(), id)
		if errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		siblings, _ := store.ListShotSiblings(r.Context(), shot.ProfileID, id, 10)
		coachSibs := make([]ai.ShotSummary, 0, len(siblings))
		for _, s := range siblings {
			coachSibs = append(coachSibs, ai.ShotSummary{
				Name:         s.Name,
				TimeISO:      s.TimeISO,
				Duration:     s.Duration,
				PeakPressure: s.PeakPressure,
				FinalWeight:  s.FinalWeight,
				Rating:       s.Rating,
				Note:         s.Note,
			})
		}
		coach := ai.NewCoach(provider)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		bean, grind, rpm := beanContextForShot(r.Context(), beansStore, shot)
		sug, err := coach.Suggest(ctx, ai.CoachInput{
			Shot: ai.ShotInput{
				Name:        shot.Name,
				ProfileName: shot.ProfileName,
				Samples:     shot.Samples,
				Profile:     shot.Profile,
				Bean:        bean,
				Grind:       grind,
				GrindRPM:    rpm,
			},
			ShotRating: shot.Rating,
			ShotNote:   shot.Note,
			Siblings:   coachSibs,
		})
		if err != nil {
			recordAICall(rec, coach.ModelName(), "coach", id, ai.CallUsage{}, err)
			slog.Warn("coach failed", "shot_id", id, "err", err.Error())
			status := http.StatusBadGateway
			if ai.IsTransient(err) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		recordAICall(rec, coach.ModelName(), "coach", id, sug.Usage, nil)
		// Persist so the user sees the same suggestion when they reopen
		// the shot. Failure here is non-fatal — log and keep serving
		// the fresh result; the next POST will retry the save.
		if raw, mErr := json.Marshal(sug); mErr == nil {
			if sErr := store.SaveCoachSuggestion(context.Background(), id, coach.ModelName(), raw); sErr != nil {
				slog.Warn("save coach suggestion", "shot_id", id, "err", sErr.Error())
			}
		}
		writeJSON(w, http.StatusOK, sug)
	}
}

// handleShotCoachGet returns the most recent cached coach suggestion
// for a shot. Prefers the user's currently-active model (for
// consistency with what a fresh run would show) and falls back to the
// latest suggestion under any model — same escape valve as analyses
// so switching provider doesn't hide existing suggestions.
func handleShotCoachGet(store *shots.Store, mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var activeModel string
		if mgr != nil {
			if p := mgr.Provider(); p != nil {
				activeModel = p.Name()
			}
		}
		if activeModel != "" {
			if raw, err := store.GetCoachSuggestion(r.Context(), id, activeModel); err == nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			} else if !errors.Is(err, shots.ErrNotFound) {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if _, raw, err := store.GetLatestCoachSuggestion(r.Context(), id); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
			return
		} else if !errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if activeModel == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not coached"})
	}
}

// handleShotsCompare asks the LLM to compare two shots and explain why
// one differed from the other. Body: { "a": id, "b": id }.
func handleShotsCompare(store *shots.Store, beansStore *beans.Store, mgr *settings.Manager, rec *ai.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled"})
			return
		}
		provider := mgr.Provider()
		if provider == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		var body struct {
			A string `json:"a"`
			B string `json:"b"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		if body.A == "" || body.B == "" || body.A == body.B {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "need two distinct shot ids in {a,b}"})
			return
		}
		shotA, err := store.GetShot(r.Context(), body.A)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot A not found"})
			return
		}
		shotB, err := store.GetShot(r.Context(), body.B)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot B not found"})
			return
		}
		comp := ai.NewComparator(provider)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		beanA, grindA, rpmA := beanContextForShot(r.Context(), beansStore, shotA)
		beanB, grindB, rpmB := beanContextForShot(r.Context(), beansStore, shotB)
		out, err := comp.Compare(ctx, ai.CompareInput{
			A:       ai.ShotInput{Name: shotA.Name, ProfileName: shotA.ProfileName, Samples: shotA.Samples, Profile: shotA.Profile, Bean: beanA, Grind: grindA, GrindRPM: rpmA},
			B:       ai.ShotInput{Name: shotB.Name, ProfileName: shotB.ProfileName, Samples: shotB.Samples, Profile: shotB.Profile, Bean: beanB, Grind: grindB, GrindRPM: rpmB},
			ARating: shotA.Rating, ANote: shotA.Note,
			BRating: shotB.Rating, BNote: shotB.Note,
		})
		if err != nil {
			recordAICall(rec, comp.ModelName(), "compare", body.A, ai.CallUsage{}, err)
			slog.Warn("compare failed", "err", err.Error())
			status := http.StatusBadGateway
			if ai.IsTransient(err) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		recordAICall(rec, comp.ModelName(), "compare", body.A, out.Usage, nil)
		// Persist so the same (A,B) pair hits the cache on next open.
		// Non-fatal on error — we still serve the fresh result.
		if raw, mErr := json.Marshal(out); mErr == nil {
			if sErr := store.SaveCompare(context.Background(), body.A, body.B, comp.ModelName(), raw); sErr != nil {
				slog.Warn("save compare", "a", body.A, "b", body.B, "err", sErr.Error())
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleShotsCompareGet returns a cached A/B comparison. Query params:
//
//	?a=<shotID>&b=<shotID>
//
// a and b may be passed in either order — the store canonicalises.
// Prefers the currently-configured model, falls back to the latest
// cached entry across any model. Used by the UI to reconcile the
// Compare panel when re-opening a shot with a previously-compared
// peer so the user isn't re-charged for a fresh LLM call.
func handleShotsCompareGet(store *shots.Store, mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a := q.Get("a")
		b := q.Get("b")
		if a == "" || b == "" || a == b {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "need distinct ?a=&b= query params"})
			return
		}
		var activeModel string
		if mgr != nil {
			if p := mgr.Provider(); p != nil {
				activeModel = p.Name()
			}
		}
		if activeModel != "" {
			if raw, err := store.GetCompare(r.Context(), a, b, activeModel); err == nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			} else if !errors.Is(err, shots.ErrNotFound) {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if _, raw, err := store.GetLatestCompare(r.Context(), a, b); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
			return
		} else if !errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if activeModel == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not compared"})
	}
}

// handleProfileNameSuggest asks the LLM for a short, human-friendly
// name for a pasted profile. Body: { "profile": <raw JSON>, "current_name": "..." }.
func handleProfileNameSuggest(mgr *settings.Manager, rec *ai.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := mgr.Provider()
		if provider == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai disabled — configure a provider and API key under Settings"})
			return
		}
		var body struct {
			Profile     json.RawMessage `json:"profile"`
			CurrentName string          `json:"current_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Profile) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected {\"profile\": <json>}"})
			return
		}
		namer := ai.NewNamer(provider)
		sug, err := namer.Suggest(r.Context(), ai.ProfileNameInput{
			Profile:     body.Profile,
			CurrentName: body.CurrentName,
		})
		if err != nil {
			recordAICall(rec, namer.ModelName(), "profile-name", "", ai.CallUsage{}, err)
			status := http.StatusBadGateway
			if ai.IsTransient(err) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		recordAICall(rec, namer.ModelName(), "profile-name", "", sug.Usage, nil)
		writeJSON(w, http.StatusOK, sug)
	}
}

// handleAIUsage returns rollups + recent rows for the Settings dashboard.
func handleAIUsage(rec *ai.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if q := r.URL.Query().Get("days"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v >= 0 {
				days = v
			}
		}
		recent := 50
		if q := r.URL.Query().Get("recent"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v >= 0 && v <= 500 {
				recent = v
			}
		}
		sum, err := rec.Summarize(r.Context(), days, recent)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sum)
	}
}

// currentAnalyzer is a small helper so the analysis handlers always pull the
// latest Analyzer from the manager (provider/model/key may have changed at
// runtime via the settings endpoints).
func currentAnalyzer(mgr *settings.Manager) *ai.Analyzer {
	if mgr == nil {
		return nil
	}
	return mgr.Analyzer()
}

// handleAITranscribe accepts an audio upload and returns a transcript.
// Provider selection:
//   - default → whatever the user picked in Settings → Voice transcription
//     (falls back to "auto": Gemini when its key is set, else OpenAI).
//   - ?provider=openai|gemini → one-off override (still requires the
//     corresponding API key).
//
// Body is the raw audio bytes; Content-Type identifies the container
// (audio/webm, audio/mp4, …).
func handleAITranscribe(mgr *settings.Manager) http.HandlerFunc {
	const maxAudioBytes = 25 * 1024 * 1024 // Whisper's documented upload cap.
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai settings unavailable"})
			return
		}

		// Default: use the provider configured in Settings.
		provider, apiKey, model := mgr.SpeechCreds()

		// One-off override via query string. We re-resolve the key/model
		// from the raw credential accessors so the request stays honest.
		if override := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider"))); override != "" {
			switch override {
			case "openai":
				k, _ := mgr.OpenAICreds()
				provider, apiKey, model = "openai", k, ""
			case "gemini":
				k, gm := mgr.GeminiCreds()
				provider, apiKey, model = "gemini", k, gm
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "unknown provider: " + override + " (want openai or gemini)",
				})
				return
			}
		}

		if provider == "" || apiKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "no speech provider configured — add an OpenAI or Gemini key in Settings → AI",
			})
			return
		}

		mime := r.Header.Get("Content-Type")
		if mime == "" {
			mime = "audio/webm"
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
		audio, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": "audio too large or read failed: " + err.Error(),
			})
			return
		}
		if len(audio) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty audio body"})
			return
		}

		// Nudge the models toward espresso jargon so "pre-infusion" doesn't
		// come back as "pre-confusion".
		hint := "Espresso tasting note: grind, dose, yield, preinfusion, pressure, flow, temperature, bean origin, taste."
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()

		var tr *ai.Transcription
		switch provider {
		case "gemini":
			tr, err = ai.TranscribeGemini(ctx, ai.TranscribeGeminiRequest{
				APIKey: apiKey,
				Model:  model,
				Audio:  audio,
				MIME:   mime,
				Prompt: "Transcribe this audio verbatim. Output only the transcript. Context: " + hint,
			})
		case "openai":
			tr, err = ai.TranscribeOpenAI(ctx, ai.TranscribeOpenAIRequest{
				APIKey: apiKey,
				// Whisper only has one public model right now ("whisper-1");
				// honour a configured override if it's set.
				Model:  model,
				Audio:  audio,
				MIME:   mime,
				Prompt: hint,
			})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "unsupported speech provider: " + provider,
			})
			return
		}

		if err != nil {
			slog.Warn("transcribe failed", "provider", provider, "err", err.Error(), "bytes", len(audio), "mime", mime)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		slog.Info("audio transcribed", "provider", provider, "bytes", len(audio), "mime", mime, "text_len", len(tr.Text))
		writeJSON(w, http.StatusOK, map[string]any{
			"text":     tr.Text,
			"language": tr.Language,
			"provider": provider,
		})
	}
}

// handleAIBeanFromImage accepts a photo of a coffee bag and returns
// the extracted bean fields for the user to review before saving.
// Follows the main chat provider (Settings → AI → Text provider) so
// the same LLM that analyses your shots also reads your bags: OpenAI
// chat model → OpenAI vision, Gemini → Gemini vision. Anthropic has
// no vision wired yet so it transparently falls back to whichever of
// OpenAI/Gemini has a key. The image is NOT saved to disk; it lives
// in memory only for the duration of the extraction call.
func handleAIBeanFromImage(mgr *settings.Manager, rec *ai.Recorder) http.HandlerFunc {
	const maxImageBytes = 10 * 1024 * 1024 // plenty for a single bag photo
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ai settings unavailable"})
			return
		}
		provider, apiKey, model := mgr.VisionCreds()
		if provider == "" || apiKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "bean scan needs an OpenAI or Gemini key — add one under Settings → AI",
			})
			return
		}

		// Prefer multipart for camera uploads but accept a raw image
		// body too (useful for curl testing / future fetch() calls).
		var image []byte
		var mime string
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/") {
			if err := r.ParseMultipartForm(maxImageBytes); err != nil {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": err.Error()})
				return
			}
			f, fh, err := r.FormFile("image")
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing 'image' file field"})
				return
			}
			defer f.Close()
			if fh.Size > maxImageBytes {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "image too large"})
				return
			}
			buf, err := io.ReadAll(f)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			image = buf
			mime = fh.Header.Get("Content-Type")
		} else {
			r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
			buf, err := io.ReadAll(r.Body)
			if err != nil {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": err.Error()})
				return
			}
			image = buf
			mime = ct
		}
		if len(image) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty image"})
			return
		}
		if mime == "" {
			mime = "image/jpeg"
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		started := time.Now()

		var (
			res       *ai.ExtractBeanResponse
			err       error
			modelName = provider + ":" + model
		)
		switch provider {
		case settings.ProviderOpenAI:
			res, err = ai.ExtractBeanFromImage(ctx, ai.ExtractBeanRequest{
				APIKey: apiKey,
				Model:  model,
				Image:  image,
				MIME:   mime,
			})
		case settings.ProviderGemini:
			res, err = ai.ExtractBeanFromImageGemini(ctx, ai.ExtractBeanRequestGemini{
				APIKey: apiKey,
				Model:  model,
				Image:  image,
				MIME:   mime,
			})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "vision provider '" + provider + "' is not supported",
			})
			return
		}
		if err != nil {
			slog.Warn("bean image extract failed", "err", err.Error(), "bytes", len(image), "mime", mime, "model", modelName)
			recordAICall(rec, modelName, "bean_extract", "", ai.CallUsage{
				DurationMs: time.Since(started).Milliseconds(),
			}, err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		recordAICall(rec, modelName, "bean_extract", "", ai.CallUsage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
			DurationMs:   time.Since(started).Milliseconds(),
		}, nil)
		writeJSON(w, http.StatusOK, res.Bean)
	}
}

// handleAISettingsGet returns the redacted AI configuration (API keys are
// reported as a boolean "has_key", never as the actual secret).
func handleAISettingsGet(mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Public())
	}
}

// handleAISettingsPut accepts a partial patch: nil fields are untouched,
// empty-string fields clear, non-empty values update.
func handleAISettingsPut(mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var patch settings.AIUpdate
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
		pub, err := mgr.Update(r.Context(), patch)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, pub)
	}
}

// handleAIModelsList hits the provider's own catalogue endpoint using the
// stored API key and returns the usable model IDs. Having a dropdown fed by
// this keeps the UI from requiring users to memorise model names and from
// going stale when providers ship new models.
func handleAIModelsList(mgr *settings.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := chi.URLParam(r, "provider")
		// Independent timeout: the provider list endpoints are small but
		// we don't want to inherit a stalled request's context.
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		models, err := mgr.ListModels(ctx, provider)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
	}
}

// --- Preheat ---------------------------------------------------------------

func handlePreheatList(store *preheat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if items == nil {
			items = []preheat.Schedule{}
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func handlePreheatCreate(store *preheat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sch preheat.Schedule
		if err := json.NewDecoder(r.Body).Decode(&sch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		sch.ID = uuid.NewString()
		if err := store.Create(r.Context(), &sch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, sch)
	}
}

func handlePreheatUpdate(store *preheat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sch preheat.Schedule
		if err := json.NewDecoder(r.Body).Decode(&sch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		sch.ID = chi.URLParam(r, "id")
		if err := store.Update(r.Context(), &sch); err != nil {
			if errors.Is(err, preheat.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sch)
	}
}

func handlePreheatDelete(store *preheat.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := store.Delete(r.Context(), id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handlePreheatStatus(sch *preheat.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sch.Status(r.Context()))
	}
}

func handlePreheatNow(sch *preheat.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Use a short independent timeout — the trigger is fire-and-forget
		// from the operator's perspective; the machine's actual preheat
		// cycle takes minutes.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := sch.TriggerNow(ctx); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sch.Status(r.Context()))
	}
}

// --- Beans -----------------------------------------------------------------

func handleBeansList(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if list == nil {
			list = []beans.Bean{}
		}
		writeJSON(w, http.StatusOK, list)
	}
}

func handleBeansGet(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := store.Get(r.Context(), chi.URLParam(r, "id"))
		if errors.Is(err, beans.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "bean not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

func handleBeansCreate(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in beans.Input
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		b, err := store.Create(r.Context(), in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, b)
	}
}

func handleBeansUpdate(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in beans.Input
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		b, err := store.Update(r.Context(), chi.URLParam(r, "id"), in)
		if errors.Is(err, beans.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "bean not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

func handleBeansDelete(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := store.Delete(r.Context(), chi.URLParam(r, "id"))
		if errors.Is(err, beans.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "bean not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleShotSetBean attaches (or clears with bean_id="") a bean on a shot.
func handleShotSetBean(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BeanID string `json:"bean_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		err := store.SetBean(r.Context(), chi.URLParam(r, "id"), body.BeanID)
		if errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleShotSetGrind persists the grind setting label and optional RPM
// on a shot. Body: {"grind": "2.8", "rpm": 800}. Either field may be
// omitted or null to clear; both nullable on the SQL side (rpm) or
// allowed-empty (grind).
func handleShotSetGrind(store *shots.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Grind string   `json:"grind"`
			RPM   *float64 `json:"rpm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		err := store.SetGrind(r.Context(), chi.URLParam(r, "id"), body.Grind, body.RPM)
		if errors.Is(err, shots.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "shot not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBeansSetActive marks /api/beans/{id}/active: sets this bean as
// the "bag currently in use", clearing the flag on every other row.
func handleBeansSetActive(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := store.SetActive(r.Context(), chi.URLParam(r, "id"))
		if errors.Is(err, beans.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "bean not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBeansClearActive clears the active marker from every bean.
// No-op if no bean is currently active.
func handleBeansClearActive(store *beans.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.SetActive(r.Context(), ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
