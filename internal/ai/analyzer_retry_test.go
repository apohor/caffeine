package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// stubProvider is a Provider whose Complete behaviour is driven by a
// script: each call returns the next (output, err) pair. Retry tests
// script a few failures followed by a success.
type stubProvider struct {
	name    string
	calls   atomic.Int32
	outputs []string
	errs    []error
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Complete(ctx context.Context, system, user string) (string, TokenUsage, error) {
	i := int(s.calls.Add(1)) - 1
	if i >= len(s.errs) {
		i = len(s.errs) - 1
	}
	return s.outputs[i], TokenUsage{}, s.errs[i]
}

// samplesFixture is the smallest shot body that passes Analyze's
// "no samples" guard. Contents aren't inspected by the retry path.
func samplesFixture(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{
		{"time": 0, "profile_time": 0, "shot": map[string]any{"pressure": 0.0}},
		{"time": 1, "profile_time": 1000, "shot": map[string]any{"pressure": 6.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAnalyze_RetriesTransientThenSucceeds(t *testing.T) {
	stub := &stubProvider{
		name:    "stub:busy-then-ok",
		outputs: []string{"", "", "ok analysis"},
		errs: []error{
			fmt.Errorf("anthropic: http 529: overloaded"),
			fmt.Errorf("openai: http 503: server had an error while processing your request"),
			nil,
		},
	}
	// Speed the test: short context means the default 1s backoff is too
	// slow. The analyzer clamps its sleep by ctx, so a 3s deadline lets
	// the two 1s + 2s backoffs still fit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	out, err := NewAnalyzer(stub).Analyze(ctx, ShotInput{
		Name: "t", ProfileName: "p", Samples: samplesFixture(t),
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if out.Summary != "ok analysis" {
		t.Fatalf("summary: %q", out.Summary)
	}
	if got := stub.calls.Load(); got != 3 {
		t.Fatalf("calls: got %d want 3", got)
	}
	// Sanity: we did sleep between retries (two backoffs ≥ 1s each, but
	// jitter can shave ~25%). Anything under 500ms would indicate the
	// sleep was skipped.
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("retries finished too fast (%s) — backoff not applied", elapsed)
	}
}

func TestAnalyze_DoesNotRetryNonTransient(t *testing.T) {
	stub := &stubProvider{
		name:    "stub:auth",
		outputs: []string{""},
		errs:    []error{fmt.Errorf("openai: http 401: invalid api key")},
	}
	_, err := NewAnalyzer(stub).Analyze(context.Background(), ShotInput{
		Name: "t", ProfileName: "p", Samples: samplesFixture(t),
	})
	if err == nil {
		t.Fatal("want auth error")
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("calls: got %d want 1 (must not retry 401)", got)
	}
}

func TestAnalyze_GivesUpAfterMaxAttempts(t *testing.T) {
	// Always return a retryable error; with a short ctx the analyzer
	// should bail promptly mid-backoff rather than waiting the full
	// exponential budget. This specifically exercises the ctx.Done()
	// branch inside completeWithRetry.
	transient := errors.New("gemini: http 429: rate limit exceeded")
	stub := &stubProvider{
		name:    "stub:always-429",
		outputs: make([]string, maxAttempts),
		errs:    make([]error, maxAttempts),
	}
	for i := range stub.errs {
		stub.errs[i] = transient
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := NewAnalyzer(stub).Analyze(ctx, ShotInput{
		Name: "t", ProfileName: "p", Samples: samplesFixture(t),
	})
	if err == nil {
		t.Fatal("want error after ctx cancel")
	}
	// Should return well under the full backoff budget (~15s).
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ctx cancel not honoured — ran for %s", elapsed)
	}
	if got := stub.calls.Load(); got < 1 {
		t.Fatalf("expected at least one provider call, got %d", got)
	}
}

func TestIsTransient(t *testing.T) {
	retry := []string{
		"anthropic: http 529: {\"type\":\"overloaded_error\"}",
		"openai: http 429: rate limit exceeded",
		"openai: http 503: server had an error",
		"gemini: http 502: bad gateway",
		"gemini: http 504: gateway timeout",
		"model overloaded, try again later",
		"resource_exhausted: quota",
	}
	noRetry := []string{
		"openai: http 401: invalid api key",
		"anthropic: http 400: invalid model",
		"gemini: http 404: model not found",
		"decode samples: unexpected end of json",
	}
	for _, s := range retry {
		if !isTransient(errors.New(s)) {
			t.Errorf("should retry: %q", s)
		}
	}
	for _, s := range noRetry {
		if isTransient(errors.New(s)) {
			t.Errorf("should NOT retry: %q", s)
		}
	}
}
