// Command caffeine is the single-binary server for the Caffeine espresso companion.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// Embed the IANA time zone database so `TZ=America/New_York` etc. work
	// even on distroless images that don't ship /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/apohor/caffeine/internal/ai"
	"github.com/apohor/caffeine/internal/api"
	"github.com/apohor/caffeine/internal/beans"
	"github.com/apohor/caffeine/internal/config"
	"github.com/apohor/caffeine/internal/live"
	"github.com/apohor/caffeine/internal/preheat"
	"github.com/apohor/caffeine/internal/profileimages"
	"github.com/apohor/caffeine/internal/push"
	"github.com/apohor/caffeine/internal/settings"
	"github.com/apohor/caffeine/internal/shots"
	"github.com/apohor/caffeine/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Env var resolution: prefer the short, unprefixed names (MACHINE_URL,
	// DATA_DIR, ...) but still accept the legacy CAFFEINE_-prefixed names
	// so existing deployments keep working.
	addr := flag.String("addr", envFirst(":8080", "ADDR", "CAFFEINE_ADDR"), "HTTP listen address")
	machineURL := flag.String("machine-url", envFirst("http://meticulous.local", "MACHINE_URL", "CAFFEINE_MACHINE_URL"), "Meticulous machine base URL")
	dataDir := flag.String("data-dir", envFirst("./data", "DATA_DIR", "CAFFEINE_DATA_DIR"), "directory for the SQLite cache")
	syncInterval := flag.Duration("sync-interval", parseDuration(envFirst("15m", "SYNC_INTERVAL", "CAFFEINE_SYNC_INTERVAL"), 15*time.Minute), "how often to pull shot history from the machine")
	aiProvider := flag.String("ai-provider", envFirst("", "AI_PROVIDER", "CAFFEINE_AI_PROVIDER"), "AI provider: openai | anthropic | gemini. If empty, auto-picks the first provider whose API key is set.")
	openAIKey := flag.String("openai-api-key", os.Getenv("OPENAI_API_KEY"), "OpenAI API key (or OPENAI_API_KEY env)")
	openAIModel := flag.String("openai-model", envOr("OPENAI_MODEL", "gpt-4o-mini"), "OpenAI chat model")
	anthropicKey := flag.String("anthropic-api-key", os.Getenv("ANTHROPIC_API_KEY"), "Anthropic API key (or ANTHROPIC_API_KEY env)")
	anthropicModel := flag.String("anthropic-model", envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"), "Anthropic model")
	geminiKey := flag.String("gemini-api-key", firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")), "Google Gemini API key (or GEMINI_API_KEY / GOOGLE_API_KEY env)")
	geminiModel := flag.String("gemini-model", envOr("GEMINI_MODEL", "gemini-2.5-flash"), "Gemini model")
	flag.Parse()

	// Startup banner: makes the effective config visible in container
	// logs so a missed env var (e.g. compose not rebuilt after editing)
	// is obvious without needing to exec into the container.
	logger.Info("caffeine starting",
		"addr", *addr,
		"machine_url", *machineURL,
		"data_dir", *dataDir,
		"sync_interval", syncInterval.String(),
		// Preheat schedules match HH:MM in this zone — set TZ env to change.
		"tz", time.Now().Location().String(),
	)

	cfg := config.Config{
		Addr:       *addr,
		MachineURL: *machineURL,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Shot cache (best-effort: the server still starts if this fails).
	var (
		shotStore  *shots.Store
		shotSyncer *shots.Syncer
	)
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		logger.Warn("cannot create data dir", "path", *dataDir, "err", err.Error())
	} else {
		dbPath := filepath.Join(*dataDir, "caffeine.db")
		if st, err := shots.OpenStore(dbPath); err != nil {
			logger.Warn("shot cache disabled", "err", err.Error())
		} else {
			shotStore = st
			defer shotStore.Close()
			shotSyncer = shots.NewSyncer(shotStore, *machineURL, *syncInterval)
			go shotSyncer.Run(ctx)
			logger.Info("shot cache ready", "db", dbPath, "sync_interval", syncInterval.String())
		}
	}

	// Optional AI shot analyzer. The Settings manager owns the Analyzer and
	// rebuilds it when the operator changes provider/model/key via the UI.
	// Env vars and CLI flags act purely as *seeds* for a fresh database.
	var aiSettings *settings.Manager
	if shotStore != nil {
		dbPath := filepath.Join(*dataDir, "caffeine.db")
		if sstore, err := settings.OpenStore(dbPath); err != nil {
			logger.Warn("settings store disabled", "err", err.Error())
		} else {
			seed := settings.AIConfig{
				Provider:       *aiProvider,
				OpenAIModel:    *openAIModel,
				OpenAIKey:      *openAIKey,
				AnthropicModel: *anthropicModel,
				AnthropicKey:   *anthropicKey,
				GeminiModel:    *geminiModel,
				GeminiKey:      *geminiKey,
			}
			if m, err := settings.NewManager(ctx, sstore, seed); err != nil {
				logger.Warn("ai settings manager failed", "err", err.Error())
				_ = sstore.Close()
			} else {
				aiSettings = m
				defer sstore.Close()
			}
		}
	}

	// Preheat schedules. Same SQLite file, separate table. Best-effort:
	// the server still starts (without preheat) if this fails.
	var (
		preheatStore     *preheat.Store
		preheatScheduler *preheat.Scheduler
	)
	if shotStore != nil {
		dbPath := filepath.Join(*dataDir, "caffeine.db")
		if ps, err := preheat.OpenStore(dbPath); err != nil {
			logger.Warn("preheat disabled", "err", err.Error())
		} else {
			preheatStore = ps
			defer preheatStore.Close()
			preheatScheduler = preheat.NewScheduler(preheatStore, *machineURL)
			go preheatScheduler.Run(ctx)
			logger.Info("preheat scheduler ready")
		}
	}

	// Live event hub. Owns one upstream WebSocket to the machine and
	// fans events out to browser clients + the in-process recorder.
	liveHub := live.NewHub(ctx, *machineURL)

	// Web push: subscriptions + VAPID identity persisted in the same
	// SQLite file as the rest of the app. Best-effort: if the store
	// can't open or the VAPID keys can't be generated, push is simply
	// disabled and the UI shows a "push unavailable" state.
	var (
		pushStore   *push.Store
		pushService *push.Service
	)
	if shotStore != nil {
		dbPath := filepath.Join(*dataDir, "caffeine.db")
		if ps, err := push.OpenStore(dbPath); err != nil {
			logger.Warn("push disabled", "err", err.Error())
		} else {
			pushStore = ps
			defer pushStore.Close()
			vapid, err := push.LoadOrGenerateVAPID(ctx, pushStore,
				envFirst("", "VAPID_PUBLIC_KEY", "CAFFEINE_VAPID_PUBLIC_KEY"),
				envFirst("", "VAPID_PRIVATE_KEY", "CAFFEINE_VAPID_PRIVATE_KEY"),
				envFirst("", "VAPID_SUBJECT", "CAFFEINE_VAPID_SUBJECT"),
			)
			if err != nil {
				logger.Warn("push disabled: vapid init failed", "err", err.Error())
			} else {
				pushService = push.NewService(pushStore, vapid, logger)
				logger.Info("push notifications ready",
					"vapid_public_key_prefix", vapidPreview(vapid.PublicKey),
					"subject", vapid.Subject)
			}
		}
	}

	// Live shot recorder: persists shots the moment the machine reports
	// the end of extraction so the UI doesn't wait for the next /history
	// sync (which can be minutes away). Auto-analyzes when AI is on.
	//
	// AI usage ledger is initialised first so the auto-analyze trigger
	// can record calls alongside manual ones.
	var aiRecorder *ai.Recorder
	if shotStore != nil {
		if rec, err := ai.NewRecorder(shotStore.DB()); err != nil {
			logger.Warn("ai usage ledger disabled", "err", err.Error())
		} else {
			aiRecorder = rec
			logger.Info("ai usage ledger ready")
		}
	}
	if shotStore != nil {
		trigger := newAutoAnalyzeTrigger(shotStore, aiSettings, pushService, aiRecorder)
		rec := live.NewRecorder(liveHub, liveSink{shotStore}, trigger)
		if pushService != nil {
			rec = rec.WithShotFinishedHook(func(shotID, name string) {
				pushService.NotifyShotFinished(shotID, name)
			})
		}
		go rec.Run(ctx)
		logger.Info("live shot recorder ready")
	}

	// AI-generated profile artwork. Filesystem-backed, lives alongside
	// the SQLite file. Best-effort: if we can't create the directory
	// the feature is simply disabled in the UI.
	var profileImages *profileimages.Store
	if pi, err := profileimages.Open(filepath.Join(*dataDir, "profile-images")); err != nil {
		logger.Warn("profile images disabled", "err", err.Error())
	} else {
		profileImages = pi
		logger.Info("profile images ready", "dir", filepath.Join(*dataDir, "profile-images"))
	}

	// Beans store — piggybacks on the shot DB so migrations run once.
	var beansStore *beans.Store
	if shotStore != nil {
		if bs, err := beans.New(ctx, shotStore.DB()); err != nil {
			logger.Warn("beans store disabled", "err", err.Error())
		} else {
			beansStore = bs
			logger.Info("beans store ready")
			// Let the shot store auto-tag new shots with the
			// currently-active bag. Kept as a fn so the shots
			// package doesn't import internal/beans.
			shotStore.SetActiveBeanResolver(func(ctx context.Context) (string, string, *float64) {
				id, grind, rpm, err := beansStore.ActiveDefaults(ctx)
				if err != nil {
					logger.Warn("active bean lookup failed", "err", err.Error())
					return "", "", nil
				}
				return id, grind, rpm
			})
		}
	}

	handler := api.New(cfg, web.Assets(), api.Deps{
		ShotStore:        shotStore,
		ShotSyncer:       shotSyncer,
		LiveHub:          liveHub,
		AISettings:       aiSettings,
		PreheatStore:     preheatStore,
		PreheatScheduler: preheatScheduler,
		PushStore:        pushStore,
		PushService:      pushService,
		ProfileImages:    profileImages,
		AIRecorder:       aiRecorder,
		Beans:            beansStore,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("caffeine starting", "addr", cfg.Addr, "machine_url", cfg.MachineURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envFirst returns the first non-empty value among the given env var names,
// or the fallback if none are set. Used to accept both short (MACHINE_URL)
// and legacy (CAFFEINE_MACHINE_URL) names for the same setting.
func envFirst(fallback string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return fallback
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// vapidPreview returns a short, log-safe prefix of the VAPID public key so
// operators can visually confirm the same identity is used across restarts
// without dumping the whole 87-char base64 string into every startup line.
func vapidPreview(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8] + "…"
}

// buildAIProvider has been replaced by internal/settings.Manager, which
// owns the live AI configuration and rebuilds the Analyzer when the operator
// changes provider/model/key at runtime. The flags above still work as
// *seed* values for a fresh database.

// liveSink adapts *shots.Store to the live.ShotSink interface so the
// recorder can persist shots without importing the live package into
// shots (and vice versa).
type liveSink struct{ store *shots.Store }

func (l liveSink) SaveLiveShot(ctx context.Context, s live.LiveShot) error {
	return l.store.SaveLiveShot(ctx, shots.LiveShotInput{
		ID:          s.ID,
		Time:        s.Time,
		Name:        s.Name,
		ProfileID:   s.ProfileID,
		ProfileName: s.ProfileName,
		Samples:     s.Samples,
		Profile:     s.Profile,
	})
}

// newAutoAnalyzeTrigger returns a function the live recorder invokes
// after persisting a shot. It runs an AI analysis in the background and
// caches the result via the same path the HTTP endpoint uses, so the UI
// sees it "for free" on next load. Returns nil if AI isn't configured —
// the recorder handles that by skipping the call.
func newAutoAnalyzeTrigger(store *shots.Store, mgr *settings.Manager, pushSvc *push.Service, rec *ai.Recorder) live.AnalysisTrigger {
	return func(shotID string) {
		if mgr == nil {
			return
		}
		analyzer := mgr.Analyzer()
		if analyzer == nil {
			return
		}
		// Skip if we already have an analysis for this (shot, model).
		if _, err := store.GetAnalysis(context.Background(), shotID, analyzer.ModelName()); err == nil {
			return
		}
		// Reasoning models (gemini-3-pro-preview, o1, claude extended-
		// thinking) routinely exceed a 90s budget. Match the manual
		// /api/shots/:id/analysis handler's 5-minute ceiling so the live
		// auto-trigger doesn't silently fail where the button would work.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		shot, err := store.GetShot(ctx, shotID)
		if err != nil {
			slog.Warn("auto-analyze: load shot failed", "shot_id", shotID, "err", err.Error())
			return
		}
		analysis, err := analyzer.Analyze(ctx, ai.ShotInput{
			Name:        shot.Name,
			ProfileName: shot.ProfileName,
			Samples:     shot.Samples,
			Profile:     shot.Profile,
		})
		if err != nil {
			recordAutoAICall(rec, analyzer.ModelName(), "analyze-auto", shotID, ai.CallUsage{}, err)
			slog.Warn("auto-analyze failed", "shot_id", shotID, "model", analyzer.ModelName(), "err", err.Error())
			return
		}
		recordAutoAICall(rec, analyzer.ModelName(), "analyze-auto", shotID, analysis.Usage, nil)
		raw, err := json.Marshal(analysis)
		if err != nil {
			slog.Warn("auto-analyze marshal failed", "shot_id", shotID, "err", err.Error())
			return
		}
		if err := store.SaveAnalysis(context.Background(), shotID, analyzer.ModelName(), raw); err != nil {
			slog.Warn("auto-analyze save failed", "shot_id", shotID, "err", err.Error())
			return
		}
		slog.Info("auto-analyzed live shot", "shot_id", shotID, "model", analyzer.ModelName())
		if pushSvc != nil {
			pushSvc.NotifyAnalysisReady(shotID, analyzer.ModelName())
		}
	}
}

// recordAutoAICall mirrors api.recordAICall for the main-package
// auto-analyze trigger. Kept local to avoid widening the api package's
// exported surface.
func recordAutoAICall(rec *ai.Recorder, modelName, feature, shotID string, u ai.CallUsage, err error) {
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
