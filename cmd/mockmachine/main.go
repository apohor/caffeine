// Command mockmachine is a tiny stand-in for a Meticulous espresso
// machine, enough to let the caffeine backend run end-to-end against
// `http://meticulous.local` (or any localhost URL) without real
// hardware. It speaks just the endpoints caffeine polls:
//
//   - GET /                           → 200 OK (reachability probe)
//   - GET /api/v1/history             → JSON shot history
//   - GET /api/v1/action/preheat      → 200 OK (ack)
//   - WS  /socket.io/?EIO=4&…         → Engine.IO v4 + socket.io v4
//     emitting "status" events that simulate a live extraction
//
// To point caffeine at it, run the mock on the port of your choice and
// set MACHINE_URL=http://localhost:<port>. To keep the default
// http://meticulous.local URL working, add a hosts entry instead:
//
//	sudo sh -c 'echo "127.0.0.1 meticulous.local" >> /etc/hosts'
//	mockmachine -addr :80          # needs sudo for port 80
//
// Usage:
//
//	mockmachine -addr :8090 -simulate 20s
//
// With -simulate set, the mock fires a fake 25-second extraction every
// N (default 0 = disabled; must be opened explicitly to avoid stealing
// focus on Live when the user is just smoke-testing /history).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	simulate := flag.Duration("simulate", 0, "if >0, emit a fake shot every N (e.g. 30s)")
	shots := flag.Int("shots", 8, "number of synthetic shots to serve on /api/v1/history")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	h := newHub()

	// In-memory profile overrides keyed by id. Starts empty; any POST to
	// /profile/save writes here, and GET /profile/get/:id prefers this
	// map over the static fixtures. Lets integration smoke tests verify
	// the "Apply: <var> = <value>" round-trip without needing real
	// hardware. Lost when the mock restarts, which is fine — fixtures
	// come back untouched.
	var profileMu sync.RWMutex
	profileOverrides := map[string]map[string]any{}

	// Optional shot generator — broadcasts a fake extraction to any
	// currently-connected socket.io client at a fixed interval.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *simulate > 0 {
		go runSimulator(ctx, h, *simulate)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("mockmachine ok\n"))
	})
	mux.HandleFunc("/api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"history": fixtureShots(*shots)})
	})
	mux.HandleFunc("/api/v1/action/preheat", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("preheat triggered")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// --- Profile + settings endpoints (proxied via /api/machine/…) ---
	mux.HandleFunc("/api/v1/profile/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fixtureProfiles())
	})
	mux.HandleFunc("/api/v1/profile/get/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/profile/get/")
		profileMu.RLock()
		override, ok := profileOverrides[id]
		profileMu.RUnlock()
		if ok {
			writeJSON(w, override)
			return
		}
		for _, p := range fixtureProfilesFull() {
			if p["id"] == id {
				writeJSON(w, p)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/profile/save", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, _ := body["id"].(string)
		if id == "" {
			id = fmt.Sprintf("mock-%d", time.Now().UnixNano())
			body["id"] = id
		}
		profileMu.Lock()
		profileOverrides[id] = body
		profileMu.Unlock()
		writeJSON(w, map[string]any{"id": id, "ok": true})
	})
	mux.HandleFunc("/api/v1/profile/load/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		// GET returns current, POST merges and returns current (same body).
		writeJSON(w, map[string]any{
			"save_debug_shot_data":       true,
			"auto_preheat":               false,
			"auto_purge_after_shot":      true,
			"disallow_firmware_flashing": true,
		})
	})
	mux.HandleFunc("/api/v1/action/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/action/")
		slog.Info("action", "name", name)
		writeJSON(w, map[string]any{"ok": true, "action": name})
	})
	mux.HandleFunc("/socket.io/", func(w http.ResponseWriter, r *http.Request) {
		h.handleWS(w, r)
	})
	// Broadcast a shot on demand from a shell (curl /debug/fire-shot).
	mux.HandleFunc("/debug/fire-shot", func(w http.ResponseWriter, r *http.Request) {
		go fireOneShot(h)
		_, _ = w.Write([]byte("firing\n"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logMW(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	slog.Info("mockmachine listening", "addr", *addr, "simulate", simulate.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen", "err", err.Error())
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------
// /api/v1/history fixtures
// ---------------------------------------------------------------------

// fixtureShots returns n synthetic shots at plausible recent
// timestamps. Enough fields are populated to exercise the rendering
// and analysis paths: profile metadata, a trimmed sample stream, and
// stable ids so re-runs don't duplicate on the caffeine side.
func fixtureShots(n int) []map[string]any {
	if n < 1 {
		n = 1
	}
	out := make([]map[string]any, 0, n)
	now := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < n; i++ {
		ts := now - int64(i)*3600 // one per hour going back
		out = append(out, map[string]any{
			"id":         fmt.Sprintf("mock-%d", ts),
			"db_key":     1000 + i,
			"time":       float64(ts),
			"name":       fmt.Sprintf("Mock shot %d", i+1),
			"file":       fmt.Sprintf("mock/%d.json.zst", ts),
			"debug_file": "",
			"profile": map[string]any{
				"id":   "mock-espresso",
				"name": "Mock Espresso",
			},
			"data": sampleStream(25.0),
		})
	}
	return out
}

// sampleStream generates a plausible pressure/flow/weight curve for a
// duration-second extraction. Shape: ramp-up to 9 bar, flat middle,
// taper; flow mirrors inversely; weight integrates flow.
func sampleStream(duration float64) []map[string]any {
	const hz = 10.0
	n := int(duration * hz)
	samples := make([]map[string]any, 0, n)
	weight := 0.0
	for i := 0; i < n; i++ {
		t := float64(i) / hz
		// pressure: ramp over 4s, hold 6 bar preinfusion→9 bar, taper last 3s
		var p float64
		switch {
		case t < 4:
			p = 6 + (9-6)*(t/4)
		case t > duration-3:
			p = 9 * (duration - t) / 3
		default:
			p = 9
		}
		// flow inversely tied to resistance; peak mid-shot
		f := 1.5 + 1.5*math.Sin(math.Pi*t/duration)
		weight += f / hz
		samples = append(samples, map[string]any{
			"time":         t,
			"profile_time": t,
			"shot": map[string]any{
				"pressure":         round2(p),
				"flow":             round2(f),
				"weight":           round2(weight),
				"gravimetric_flow": round2(f * 0.95),
				"temperature":      round2(92.5 + 0.5*math.Sin(t)),
			},
			"status": "extracting",
		})
	}
	return samples
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// fixtureProfiles returns the short-form list that /profile/list
// returns (enough fields for the Profiles index page to render).
func fixtureProfiles() []map[string]any {
	return []map[string]any{
		{
			"id":           "mock-espresso",
			"name":         "Mock Espresso",
			"author":       "mockmachine",
			"temperature":  93.0,
			"final_weight": 36.0,
			"last_changed": time.Now().Add(-24 * time.Hour).Unix(),
			"display":      map[string]any{"accentColor": "#8b5a2b"},
		},
		{
			"id":           "mock-ristretto",
			"name":         "Mock Ristretto",
			"author":       "mockmachine",
			"temperature":  94.0,
			"final_weight": 22.0,
			"last_changed": time.Now().Add(-48 * time.Hour).Unix(),
			"display":      map[string]any{"accentColor": "#3b82f6"},
		},
		{
			"id":           "mock-lungo",
			"name":         "Mock Lungo",
			"author":       "mockmachine",
			"temperature":  92.0,
			"final_weight": 50.0,
			"last_changed": time.Now().Add(-7 * 24 * time.Hour).Unix(),
			"display":      map[string]any{"accentColor": "#10b981"},
		},
	}
}

// fixtureProfilesFull returns the full profile objects /profile/get/{id}
// returns. Minimal but non-empty so the detail page doesn't blow up.
func fixtureProfilesFull() []map[string]any {
	base := fixtureProfiles()
	out := make([]map[string]any, 0, len(base))
	for _, p := range base {
		full := map[string]any{
			"id":           p["id"],
			"name":         p["name"],
			"author":       p["author"],
			"temperature":  p["temperature"],
			"final_weight": p["final_weight"],
			"display":      p["display"],
			"variables": []map[string]any{
				{"name": "Dose", "key": "dose", "type": "number", "value": 18.0},
				{"name": "Ratio", "key": "ratio", "type": "number", "value": 2.0},
			},
			"stages": []map[string]any{
				{
					"name":     "preinfusion",
					"type":     "pressure",
					"target":   4.0,
					"duration": 6,
					"exit_triggers": []map[string]any{
						{"type": "pressure", "value": 4.0},
					},
				},
				{
					"name":     "Pressure Decline",
					"type":     "pressure",
					"target":   6.0,
					"duration": 20,
					// Two triggers here so the UI can exercise both
					// REMOVE exit_trigger flow FROM stage "Pressure Decline"
					// and SET exit_trigger flow = 2 ON stage "Pressure Decline"
					// without touching the time trigger.
					"exit_triggers": []map[string]any{
						{"type": "flow", "value": 1.5},
						{"type": "time", "value": 15},
					},
				},
				{"name": "extract", "type": "pressure", "target": 9.0, "duration": 25},
			},
		}
		out = append(out, full)
	}
	return out
}

// ---------------------------------------------------------------------
// socket.io hub (server-side)
// ---------------------------------------------------------------------

type hub struct {
	mu    sync.Mutex
	conns map[*connState]struct{}
}

type connState struct {
	c    *websocket.Conn
	send chan []byte
}

func newHub() *hub { return &hub{conns: make(map[*connState]struct{})} }

func (h *hub) add(cs *connState) {
	h.mu.Lock()
	h.conns[cs] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(cs *connState) {
	h.mu.Lock()
	delete(h.conns, cs)
	h.mu.Unlock()
}

// broadcast sends a socket.io EVENT frame to every connected client.
// Slow clients drop their oldest message rather than blocking the
// simulator — we prefer a current view over a perfect history.
func (h *hub) broadcast(name string, data any) {
	body, err := json.Marshal([]any{name, data})
	if err != nil {
		return
	}
	frame := append([]byte("42"), body...) // Engine.IO 4 + socket.io 2 (EVENT)

	h.mu.Lock()
	targets := make([]*connState, 0, len(h.conns))
	for cs := range h.conns {
		targets = append(targets, cs)
	}
	h.mu.Unlock()
	for _, cs := range targets {
		select {
		case cs.send <- frame:
		default:
			// drop
		}
	}
}

// handleWS performs the Engine.IO v4 + socket.io v4 handshake, then
// spools outbound frames and consumes inbound pings. We don't need to
// handle any inbound EVENTs — caffeine's client is receive-only.
func (h *hub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local dev only
	})
	if err != nil {
		slog.Warn("ws accept", "err", err.Error())
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	ctx := r.Context()
	// Engine.IO OPEN frame. sid / pingInterval / pingTimeout / maxPayload
	// per Engine.IO v4 spec. The caffeine client ignores sid.
	open := map[string]any{
		"sid":          fmt.Sprintf("mock-%d", time.Now().UnixNano()),
		"upgrades":     []string{},
		"pingInterval": 25000,
		"pingTimeout":  20000,
		"maxPayload":   1000000,
	}
	openBody, _ := json.Marshal(open)
	if err := conn.Write(ctx, websocket.MessageText, append([]byte("0"), openBody...)); err != nil {
		return
	}

	cs := &connState{c: conn, send: make(chan []byte, 64)}
	h.add(cs)
	defer h.remove(cs)

	// Writer goroutine: pulls queued frames and writes them.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-cs.send:
				if !ok {
					return
				}
				if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
					return
				}
			}
		}
	}()

	// Server-side periodic ping so the connection doesn't idle out.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				select {
				case cs.send <- []byte("2"):
				default:
				}
			}
		}
	}()

	// Read loop: handle socket.io CONNECT ("40") and Engine.IO PING ("2"),
	// ignore anything else. Exits when the socket closes.
	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText || len(payload) == 0 {
			continue
		}
		switch payload[0] {
		case '2': // ping
			select {
			case cs.send <- []byte("3"):
			default:
			}
		case '4': // socket.io packet
			if len(payload) >= 2 && payload[1] == '0' {
				// CONNECT — reply with CONNECT ack.
				ack, _ := json.Marshal(map[string]any{"sid": "mock-sio"})
				select {
				case cs.send <- append([]byte("40"), ack...):
				default:
				}
			}
		}
	}
}

// ---------------------------------------------------------------------
// shot simulator
// ---------------------------------------------------------------------

// runSimulator fires a fake extraction every `every`, with a short
// random jitter so it doesn't always land on the same second. Useful
// for exercising the Live page and the auto-analyze pipeline without
// pulling a real shot.
func runSimulator(ctx context.Context, h *hub, every time.Duration) {
	// Small initial delay so the first simulated shot doesn't race the
	// backend startup.
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	for {
		fireOneShot(h)
		jitter := time.Duration(rand.Intn(5000)) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(every + jitter):
		}
	}
}

// fireOneShot streams a 25-second extraction over socket.io. Emits
// "status" frames at 10 Hz that match the shape the caffeine live
// recorder expects.
func fireOneShot(h *hub) {
	shotID := fmt.Sprintf("mock-live-%d", time.Now().Unix())
	const duration = 25.0
	const hz = 10.0
	n := int(duration * hz)

	slog.Info("simulator: firing shot", "id", shotID)
	weight := 0.0
	for i := 0; i <= n; i++ {
		t := float64(i) / hz
		var p float64
		switch {
		case t < 4:
			p = 6 + (9-6)*(t/4)
		case t > duration-3:
			p = 9 * (duration - t) / 3
		default:
			p = 9
		}
		if p < 0 {
			p = 0
		}
		f := 1.5 + 1.5*math.Sin(math.Pi*t/duration)
		weight += f / hz
		state := "extracting"
		extracting := true
		if i == n {
			state = "idle"
			extracting = false
		}
		h.broadcast("status", map[string]any{
			"id":             shotID,
			"state":          state,
			"extracting":     extracting,
			"time":           round2(t),
			"profile_time":   round2(t),
			"loaded_profile": "Mock Espresso",
			"profile":        nil,
			"sensors": map[string]any{
				"p": round2(p),
				"f": round2(f),
				"w": round2(weight),
				"t": round2(92.5),
				"g": round2(f * 0.95),
			},
		})
		time.Sleep(100 * time.Millisecond)
	}
	slog.Info("simulator: shot done", "id", shotID, "weight", round2(weight))
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logMW logs one line per request at INFO.
func logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}
