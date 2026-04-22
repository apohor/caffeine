package live

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// fakeHub is a minimal Subscriber we drive from tests.
type fakeHub struct {
	ch chan Event
}

func newFakeHub() *fakeHub { return &fakeHub{ch: make(chan Event, 64)} }
func (f *fakeHub) Subscribe() (<-chan Event, func()) {
	return f.ch, func() {}
}
func (f *fakeHub) send(t *testing.T, name string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	f.ch <- Event{Name: name, At: time.Now(), Data: b}
}

// recordingSink captures SaveLiveShot calls.
type recordingSink struct {
	mu    sync.Mutex
	shots []LiveShot
}

func (r *recordingSink) SaveLiveShot(_ context.Context, s LiveShot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shots = append(r.shots, s)
	return nil
}
func (r *recordingSink) snapshot() []LiveShot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LiveShot, len(r.shots))
	copy(out, r.shots)
	return out
}

// statusFrame is a compact builder for the status payload shape the
// recorder decodes. Fields map 1:1 to what the machine emits. Note
// that on the real machine `time` == `profile_time` (ms since shot
// start), not a Unix timestamp — the recorder uses wall-clock for the
// shot's absolute timestamp.
func statusFrame(id, state string, extracting bool, profileTime float64, p float64) map[string]any {
	return map[string]any{
		"id":             id,
		"state":          state,
		"extracting":     extracting,
		"time":           profileTime, // ms since shot start — same as profile_time
		"profile_time":   profileTime,
		"loaded_profile": "Classic",
		"profile":        "",
		"sensors": map[string]any{
			"p": p, "f": 2.0, "w": 30.0, "t": 93.0, "g": 2.5,
		},
	}
}

// runRecorder starts a recorder against a fake hub and returns a stop
// func the test can call to flush and tear down. The recorder's Run
// returns when ctx is cancelled; we also close the event channel so
// any pending select on ch unblocks cleanly.
func runRecorder(t *testing.T, hub *fakeHub, sink ShotSink, trig AnalysisTrigger) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	rec := NewRecorder(hub, sink, trig)
	go func() {
		rec.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("recorder did not stop in time")
		}
	}
}

// waitFor polls cond up to 1s. Keeps test flakes under control without
// needing a channel from the recorder.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestRecorder_SavesOnTerminalState(t *testing.T) {
	hub := newFakeHub()
	sink := &recordingSink{}
	var analyzed []string
	var amu sync.Mutex
	trig := func(id string) {
		amu.Lock()
		analyzed = append(analyzed, id)
		amu.Unlock()
	}
	stop := runRecorder(t, hub, sink, trig)
	defer stop()

	// Enough samples to clear MinSamplesToSave.
	for i := 0; i < MinSamplesToSave+5; i++ {
		hub.send(t, "status", statusFrame("shot-1", "infusion", true, float64(i*100), 8.0))
	}
	// Terminal state flushes the buffer.
	hub.send(t, "status", statusFrame("shot-1", "purge", true, 3000, 0.5))

	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	got := sink.snapshot()[0]
	if got.ID != "shot-1" {
		t.Fatalf("id: got %q want shot-1", got.ID)
	}
	if got.Name != "Classic" {
		t.Fatalf("name: got %q want Classic", got.Name)
	}
	// The live `time` field is ms-since-shot-start (mirrors profile_time),
	// NOT wall-clock. The recorder must capture wall-clock separately, so
	// LiveShot.Time should land within a few seconds of "now" regardless
	// of what the fake frames emit.
	nowUnix := float64(time.Now().Unix())
	if got.Time < nowUnix-5 || got.Time > nowUnix+5 {
		t.Fatalf("shot time %f not close to now %f (live `time` must not leak in)", got.Time, nowUnix)
	}
	// Samples array should contain only the extracting=true frames
	// (terminal frame is not appended).
	var arr []historySample
	if err := json.Unmarshal(got.Samples, &arr); err != nil {
		t.Fatalf("samples not a JSON array: %v", err)
	}
	if len(arr) != MinSamplesToSave+5 {
		t.Fatalf("samples: got %d want %d", len(arr), MinSamplesToSave+5)
	}
	if arr[0].Shot.Pressure != 8.0 {
		t.Fatalf("pressure mapping broken: %+v", arr[0])
	}

	// Analyzer should have been invoked exactly once with the saved id.
	waitFor(t, func() bool {
		amu.Lock()
		defer amu.Unlock()
		return len(analyzed) == 1
	})
	if analyzed[0] != "shot-1" {
		t.Fatalf("analyze called with %q", analyzed[0])
	}
}

func TestRecorder_DropsShortShots(t *testing.T) {
	hub := newFakeHub()
	sink := &recordingSink{}
	stop := runRecorder(t, hub, sink, nil)
	defer stop()

	// A flush / tare: a handful of extracting frames then idle.
	for i := 0; i < 5; i++ {
		hub.send(t, "status", statusFrame("flush-1", "flushing", true, float64(i*100), 1.0))
	}
	hub.send(t, "status", statusFrame("flush-1", "idle", false, 600, 0))

	// Let the recorder process those events before we assert non-action.
	time.Sleep(50 * time.Millisecond)
	if n := len(sink.snapshot()); n != 0 {
		t.Fatalf("short shot should not save; got %d rows", n)
	}
}

func TestRecorder_NewIDFlushesPrevious(t *testing.T) {
	hub := newFakeHub()
	sink := &recordingSink{}
	stop := runRecorder(t, hub, sink, nil)
	defer stop()

	// First shot: long enough, no terminal state — the switch to a new
	// id must flush it.
	for i := 0; i < MinSamplesToSave+1; i++ {
		hub.send(t, "status", statusFrame("a", "infusion", true, float64(i*100), 6.0))
	}
	// A new shot id arrives before any 'purge' frame for shot a.
	hub.send(t, "status", statusFrame("b", "preinfusion", true, 0, 1.0))

	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	if got := sink.snapshot()[0].ID; got != "a" {
		t.Fatalf("flushed id: %q want a", got)
	}
}

func TestRecorder_ExtractingFalseFlushes(t *testing.T) {
	hub := newFakeHub()
	sink := &recordingSink{}
	stop := runRecorder(t, hub, sink, nil)
	defer stop()

	// Firmware that doesn't move to a 'purge' state but simply flips
	// extracting=false after enough real samples should still flush.
	for i := 0; i < MinSamplesToSave+2; i++ {
		hub.send(t, "status", statusFrame("shot-x", "profile", true, float64(i*100), 7.5))
	}
	hub.send(t, "status", statusFrame("shot-x", "profile", false, 3000, 0))

	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
}
