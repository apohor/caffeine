// Recorder captures live shot samples from the Hub and, on shot end,
// persists them as a shot row and (optionally) kicks off an AI analysis.
//
// The machine's /api/v1/history endpoint is the canonical source of truth
// but it can lag by many minutes on a busy machine. Subscribing to the
// live event stream lets us store the shot the instant the user releases
// the paddle — history sync, when it arrives, upserts by id and overwrites
// our row with the richer machine-side copy. Analyses are keyed on
// (shot_id, model) so re-analyzing after the upsert is idempotent.
package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// statusPayload mirrors the JSON the machine emits on the "status" event.
// Only the fields the recorder needs.
type statusPayload struct {
	ID            string  `json:"id"`
	State         string  `json:"state"`
	Extracting    bool    `json:"extracting"`
	Time          float64 `json:"time"`
	ProfileTime   float64 `json:"profile_time"`
	LoadedProfile string  `json:"loaded_profile"`
	Profile       string  `json:"profile"`
	Sensors       struct {
		P float64 `json:"p"`
		F float64 `json:"f"`
		W float64 `json:"w"`
		T float64 `json:"t"`
		G float64 `json:"g"`
	} `json:"sensors"`
}

// historySample matches the per-point shape inside /api/v1/history's "data"
// array. The AI analyzer and UI both read this layout, so the recorder
// writes samples in exactly this shape.
type historySample struct {
	Time        float64         `json:"time"`
	ProfileTime float64         `json:"profile_time"`
	Shot        historySampleIn `json:"shot"`
	Status      string          `json:"status"`
}
type historySampleIn struct {
	Pressure        float64 `json:"pressure"`
	Flow            float64 `json:"flow"`
	Weight          float64 `json:"weight"`
	GravimetricFlow float64 `json:"gravimetric_flow"`
	Temperature     float64 `json:"temperature"`
}

// ShotSink is what the recorder needs from the shot store: a narrow
// interface so tests don't have to spin up SQLite.
type ShotSink interface {
	// SaveLiveShot inserts a shot captured from the live stream iff no
	// row exists for id. It must not clobber a row synced from the
	// canonical /api/v1/history endpoint.
	SaveLiveShot(ctx context.Context, shot LiveShot) error
}

// AnalysisTrigger runs an analysis for the given shot id asynchronously.
// Implemented by a small closure in main.go that wires the analyzer +
// store together — kept as an interface so the live package doesn't
// depend on internal/ai or internal/settings.
type AnalysisTrigger func(shotID string)

// ShotFinishedHook fires after a live shot has been successfully saved.
// Used to trigger push notifications; nil disables the hook. Kept as a
// plain function so the live package stays unaware of internal/push.
type ShotFinishedHook func(shotID, name string)

// LiveShot is the payload SaveLiveShot stores.
type LiveShot struct {
	ID          string
	Time        float64 // unix seconds with ms fraction
	Name        string
	ProfileID   string
	ProfileName string
	// Samples is a JSON array matching the /api/v1/history per-shot `data`
	// shape so downstream code (analyzer, UI) reads it without branching.
	Samples json.RawMessage
	// Profile is the loaded profile JSON if the machine provided one as
	// a string on the status event; may be null.
	Profile json.RawMessage
}

// MinSamplesToSave is the threshold below which we treat a captured trace
// as noise rather than a real shot. A flush/flush-to-cup or a tare tap
// will produce a handful of 'extracting=true' frames; we don't want to
// save those as shots.
const MinSamplesToSave = 20

// endedStates mirrors the UI set: first state that means "shot is done".
var endedStates = map[string]struct{}{
	"purge": {}, "end": {}, "ended": {}, "finished": {},
	"done": {}, "retract": {}, "retracting": {}, "idle": {},
}

// Subscriber is the subset of *Hub the recorder uses. Extracted so tests
// can feed synthetic event streams without constructing a real Hub.
type Subscriber interface {
	Subscribe() (<-chan Event, func())
}

// Recorder subscribes to a Hub, buffers samples per-shot, and flushes on
// shot end. Create with NewRecorder; call Run in a goroutine.
type Recorder struct {
	hub      Subscriber
	sink     ShotSink
	analyze  AnalysisTrigger  // optional; nil disables auto-analysis
	onFinish ShotFinishedHook // optional; nil disables push notifications

	mu           sync.Mutex
	curID        string
	curSamples   []historySample
	curProfile   string
	curStartUnix float64
	curName      string
	frozen       bool // shot ended; ignore further samples for this id
}

// NewRecorder wires a recorder. analyze may be nil.
func NewRecorder(hub Subscriber, sink ShotSink, analyze AnalysisTrigger) *Recorder {
	return &Recorder{hub: hub, sink: sink, analyze: analyze}
}

// WithShotFinishedHook attaches a hook that fires after a live shot is
// saved. Returns the recorder so it can be chained in main.go's wiring.
func (r *Recorder) WithShotFinishedHook(hook ShotFinishedHook) *Recorder {
	r.onFinish = hook
	return r
}

// Run subscribes to the hub and processes events until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	ch, cancel := r.hub.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			// Final flush so an in-flight shot isn't lost on shutdown.
			r.flush(context.Background())
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Name != "status" || len(ev.Data) == 0 {
				continue
			}
			var s statusPayload
			if err := json.Unmarshal(ev.Data, &s); err != nil {
				continue
			}
			r.ingest(ctx, s)
		}
	}
}

func (r *Recorder) ingest(ctx context.Context, s statusPayload) {
	r.mu.Lock()
	// New shot id → flush the previous one (if any) and start fresh.
	if s.ID != "" && s.ID != r.curID {
		prev := r.snapshotLocked()
		r.resetLocked()
		r.curID = s.ID
		// The machine's live `time` field is ms-since-shot-start (it
		// mirrors profile_time), not a Unix timestamp. Use wall-clock
		// so the stored shot lands at roughly the actual moment it
		// started rather than 1970-01-01.
		r.curStartUnix = float64(time.Now().UnixMilli()) / 1000.0
		r.curName = s.LoadedProfile
		r.curProfile = s.Profile
		r.mu.Unlock()
		if prev != nil {
			r.flushShot(ctx, *prev)
		}
		r.mu.Lock()
	}

	// If this frame ends the shot, flush now (once).
	st := strings.ToLower(s.State)
	if !r.frozen {
		if _, terminal := endedStates[st]; terminal || (!s.Extracting && len(r.curSamples) > 0) {
			r.frozen = true
			snap := r.snapshotLocked()
			r.resetSamplesLocked()
			r.mu.Unlock()
			if snap != nil {
				r.flushShot(ctx, *snap)
			}
			return
		}
	}

	// Actively extracting → buffer this sample.
	if s.Extracting && !r.frozen {
		r.curSamples = append(r.curSamples, historySample{
			Time:        s.Time,
			ProfileTime: s.ProfileTime,
			Shot: historySampleIn{
				Pressure:        s.Sensors.P,
				Flow:            s.Sensors.F,
				Weight:          s.Sensors.W,
				GravimetricFlow: s.Sensors.G,
				Temperature:     s.Sensors.T,
			},
			Status: s.State,
		})
	}
	r.mu.Unlock()
}

// flush is called on shutdown to write whatever's buffered.
func (r *Recorder) flush(ctx context.Context) {
	r.mu.Lock()
	snap := r.snapshotLocked()
	r.resetLocked()
	r.mu.Unlock()
	if snap != nil {
		r.flushShot(ctx, *snap)
	}
}

// snapshotLocked returns a LiveShot if the current buffer is worth saving.
// Caller holds r.mu. Returns nil if there's nothing (or not enough) to save.
func (r *Recorder) snapshotLocked() *LiveShot {
	if r.curID == "" || len(r.curSamples) < MinSamplesToSave {
		return nil
	}
	samples, err := json.Marshal(r.curSamples)
	if err != nil {
		return nil
	}
	var prof json.RawMessage
	if r.curProfile != "" {
		// The machine sends `profile` as an opaque string; store it as a
		// JSON string value so the summary round-trips as valid JSON.
		if b, err := json.Marshal(r.curProfile); err == nil {
			prof = b
		}
	}
	return &LiveShot{
		ID:          r.curID,
		Time:        r.curStartUnix,
		Name:        r.curName,
		ProfileName: r.curName,
		Samples:     samples,
		Profile:     prof,
	}
}

func (r *Recorder) resetSamplesLocked() { r.curSamples = nil }
func (r *Recorder) resetLocked() {
	r.curID = ""
	r.curSamples = nil
	r.curProfile = ""
	r.curStartUnix = 0
	r.curName = ""
	r.frozen = false
}

func (r *Recorder) flushShot(ctx context.Context, shot LiveShot) {
	// Use a fresh context with a short timeout so shutdown doesn't hang
	// on a wedged DB write, and so an analyze trigger can outlive the
	// request-scoped ctx.
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.sink.SaveLiveShot(saveCtx, shot); err != nil {
		slog.Warn("live shot save failed", "shot_id", shot.ID, "err", err.Error())
		return
	}
	slog.Info("live shot saved", "shot_id", shot.ID, "samples", countJSONArray(shot.Samples))
	if r.onFinish != nil {
		// Fire-and-forget: the hook owns its own timeout + error logging.
		go r.onFinish(shot.ID, shot.Name)
	}
	if r.analyze != nil {
		// Fire-and-forget: the trigger owns its own timeout and logging.
		go r.analyze(shot.ID)
	}
	_ = ctx // ctx only used for ingest; save uses an independent budget
}

// countJSONArray is a tiny local helper to avoid an import cycle with
// the shots package. It's fine-grained enough to just count commas at
// depth 1, but json.Unmarshal is both simpler and safe here because
// samples are already validated by our marshal above.
func countJSONArray(raw json.RawMessage) int {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}
