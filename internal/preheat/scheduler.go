package preheat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Scheduler ticks once a minute and triggers preheat on schedules whose
// HH:MM matches the current local time. It also exposes a manual TriggerNow
// for the "Preheat now" UI button.
//
// Triggering = GET ${machine}/api/v1/action/preheat. This is the
// official action endpoint per meticulous-typescript-api (ActionType
// includes 'preheat'). The machine responds with an ActionResponse and
// enters its preheat cycle. Note GET not POST — Meticulous models all
// machine actions (start/stop/tare/preheat/calibration/...) as GETs on
// /action/{name}.
type Scheduler struct {
	store      *Store
	machineURL string
	client     *http.Client
	now        func() time.Time // overridable for tests

	mu          sync.RWMutex
	lastTrigger time.Time
	lastError   string
	lastSource  string // "manual" or schedule ID
}

// NewScheduler constructs a Scheduler. machineURL is the same base URL the
// proxy uses (e.g. http://meticulous.local).
func NewScheduler(store *Store, machineURL string) *Scheduler {
	return &Scheduler{
		store:      store,
		machineURL: strings.TrimRight(machineURL, "/"),
		client:     &http.Client{Timeout: 15 * time.Second},
		now:        time.Now,
	}
}

// Run blocks until ctx is cancelled, ticking every minute on the wall clock
// minute boundary.
func (s *Scheduler) Run(ctx context.Context) {
	// Align the first tick to the next wall-clock minute so HH:MM
	// comparisons are accurate. Sleeping a fraction of a second avoids
	// firing at HH:MM:59 and missing the schedule.
	first := s.now().Truncate(time.Minute).Add(time.Minute)
	wait := time.Until(first)
	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.tick(ctx)
			timer.Reset(time.Minute)
		}
	}
}

// tick checks every enabled schedule once. It is exported via test seam only.
func (s *Scheduler) tick(ctx context.Context) {
	scheds, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("preheat tick: list schedules failed", "err", err)
		return
	}
	now := s.now()
	hhmm := now.Format("15:04")
	weekdayBit := 1 << int(now.Weekday()) // Sun=0 → bit0
	for _, sch := range scheds {
		if !sch.Enabled || sch.TimeOfDay != hhmm {
			continue
		}
		if sch.WeekdayMask&weekdayBit == 0 {
			continue
		}
		if err := s.trigger(ctx, sch.ID); err != nil {
			slog.Warn("preheat trigger failed", "schedule", sch.ID, "err", err)
		} else {
			slog.Info("preheat triggered by schedule", "schedule", sch.ID, "name", sch.Name)
		}
	}
}

// TriggerNow fires preheat immediately. Returns the machine's response so
// the UI can surface it.
func (s *Scheduler) TriggerNow(ctx context.Context) error {
	return s.trigger(ctx, "manual")
}

func (s *Scheduler) trigger(ctx context.Context, source string) error {
	if s.machineURL == "" {
		return errors.New("machine url not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.machineURL+"/api/v1/action/preheat", nil)
	if err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.recordError(err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := fmt.Sprintf("machine returned %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
		s.recordError(msg)
		return errors.New(msg)
	}
	s.mu.Lock()
	s.lastTrigger = s.now()
	s.lastSource = source
	s.lastError = ""
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) recordError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

// Status is the current preheat state for the API/UI.
type Status struct {
	LastTriggered time.Time `json:"last_triggered,omitempty"`
	LastSource    string    `json:"last_source,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	NextScheduled time.Time `json:"next_scheduled,omitempty"`
	NextSchedule  string    `json:"next_schedule,omitempty"` // schedule name
	// Timezone the stored HH:MM times are interpreted in (server local).
	// Included so the UI can warn when it doesn't match the browser's zone.
	Timezone string `json:"timezone"`
}

// Status reports the most recent trigger and the next time any enabled
// schedule will fire.
func (s *Scheduler) Status(ctx context.Context) Status {
	s.mu.RLock()
	out := Status{
		LastTriggered: s.lastTrigger,
		LastSource:    s.lastSource,
		LastError:     s.lastError,
		Timezone:      s.now().Location().String(),
	}
	s.mu.RUnlock()

	scheds, err := s.store.List(ctx)
	if err != nil {
		return out
	}
	now := s.now()
	for _, sch := range scheds {
		if !sch.Enabled {
			continue
		}
		next := nextOccurrence(now, sch)
		if next.IsZero() {
			continue
		}
		if out.NextScheduled.IsZero() || next.Before(out.NextScheduled) {
			out.NextScheduled = next
			out.NextSchedule = sch.Name
		}
	}
	return out
}

// nextOccurrence returns the next wall-clock time at or after `now` (in the
// same location as now) when the schedule fires. Returns zero time if the
// schedule's mask is empty (defensive — Validate prevents this).
func nextOccurrence(now time.Time, sch Schedule) time.Time {
	t, err := time.Parse("15:04", sch.TimeOfDay)
	if err != nil {
		return time.Time{}
	}
	for offset := 0; offset < 8; offset++ {
		day := now.AddDate(0, 0, offset)
		bit := 1 << int(day.Weekday())
		if sch.WeekdayMask&bit == 0 {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(),
			t.Hour(), t.Minute(), 0, 0, now.Location())
		if !candidate.After(now) {
			continue
		}
		return candidate
	}
	return time.Time{}
}
