// Package ai produces human-readable critiques of espresso shots using an LLM.
//
// The package is split intentionally:
//
//   - Analyzer: the public API the HTTP handler calls. It extracts the
//     relevant signals from the raw shot blob, downsamples the time-series,
//     builds a deterministic prompt, and asks the configured Provider.
//   - Provider: a small interface implemented by the OpenAI client below.
//     Swapping providers later (Anthropic, Ollama, …) means adding another
//     file in this package; no call sites change.
//
// We never send the entire raw sample blob to the LLM (it can be >1000
// points). Instead we downsample to ~60 evenly spaced points, round to a
// sensible precision, and include the profile JSON verbatim.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// Provider is the minimal contract the Analyzer needs from an LLM backend.
type Provider interface {
	// Complete sends a system+user prompt pair and returns the assistant
	// text along with real token usage parsed from the provider response.
	// Implementations MUST return usage whenever the API gives it to them;
	// a zero-valued TokenUsage is only acceptable when the upstream
	// response omitted the counts (fall back to zeros, never estimate).
	Complete(ctx context.Context, system, user string) (string, TokenUsage, error)
	// Name returns a short identifier (e.g. "openai:gpt-4o-mini") used for
	// cache keying so a change of model invalidates old cached analyses.
	Name() string
}

// TokenUsage is the real input/output token count reported by a provider
// for a single call. Zero values mean the provider didn't report counts.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Analysis is the structured output returned by the analyzer.
type Analysis struct {
	Model     string    `json:"model"` // e.g. "openai:gpt-4o-mini"
	CreatedAt time.Time `json:"created_at"`
	// Rating is the model's high-level grade of the shot. Parsed out of
	// the first line of the LLM response and stripped from Summary before
	// markdown rendering. nil when the model didn't emit one in the
	// expected format — the UI just hides the badge.
	Rating *Rating `json:"rating,omitempty"`
	// Markdown summary (2-5 short paragraphs) suitable for rendering directly.
	Summary string `json:"summary"`
	// Extracted numeric metrics the UI can render as stat tiles.
	Metrics Metrics `json:"metrics"`
	// Usage is rough input/output byte accounting for the usage ledger.
	// Not part of the cached analysis payload the UI cares about, so we
	// keep it out of the JSON.
	Usage CallUsage `json:"-"`
}

// CallUsage is the minimum-viable accounting struct: real token counts
// (parsed from the provider response) plus wall-clock duration. Feeds
// the usage ledger + cost computation.
type CallUsage struct {
	InputTokens  int64
	OutputTokens int64
	DurationMs   int64
}

// Rating is a compact 0-10 grade of a shot with a one-word qualitative
// label. The label vocabulary is small on purpose so the UI can colour-
// code it: "excellent", "good", "fine", "off", "bad".
type Rating struct {
	Score int    `json:"score"` // 0..10 inclusive
	Label string `json:"label,omitempty"`
}

// Metrics are computed locally from the samples before sending to the LLM,
// and are returned as-is alongside the critique.
type Metrics struct {
	Duration       float64 `json:"duration_s"`
	PreinfusionEnd float64 `json:"preinfusion_end_s,omitempty"`
	PeakPressure   float64 `json:"peak_pressure_bar"`
	AvgPressure    float64 `json:"avg_pressure_bar"`
	PeakFlow       float64 `json:"peak_flow_mls"`
	AvgFlow        float64 `json:"avg_flow_mls"`
	FinalWeight    float64 `json:"final_weight_g"`
	FirstDripAt    float64 `json:"first_drip_s,omitempty"`
}

// Analyzer turns a Shot into an Analysis.
type Analyzer struct {
	provider Provider
}

// NewAnalyzer wraps a Provider.
func NewAnalyzer(p Provider) *Analyzer { return &Analyzer{provider: p} }

// ModelName exposes the provider identifier for cache keying.
func (a *Analyzer) ModelName() string { return a.provider.Name() }

// ShotInput is the subset of a shot the analyzer needs. We accept raw JSON
// for samples + profile so the caller doesn't have to decode them.
type ShotInput struct {
	Name        string
	ProfileName string
	Samples     json.RawMessage
	Profile     json.RawMessage
	// Bean describes the bag the shot was pulled with (optional). When
	// set, the analyzer surfaces it in the user prompt so the LLM can
	// factor origin / roast age / process into its critique instead of
	// guessing from numbers alone.
	Bean *BeanInfo
	// Grind is the user's grinder setting for this shot (free-form
	// label, e.g. "2.8" or "12 clicks"). Empty = not recorded.
	Grind string
	// GrindRPM is the variable-speed grinder RPM for this shot. Nil =
	// not recorded / not applicable to this grinder.
	GrindRPM *float64
}

// BeanInfo is the subset of a Bean the analyzer actually uses. Kept
// here (rather than importing internal/beans) so internal/ai stays
// free of app-layer dependencies and is easy to unit-test.
type BeanInfo struct {
	Name       string
	Roaster    string
	Origin     string
	Process    string
	RoastLevel string
	RoastDate  string // ISO yyyy-mm-dd; empty if unknown
	Notes      string
}

// sample mirrors the machine's per-point structure. Only the fields we use.
type sample struct {
	Time        float64 `json:"time"`
	ProfileTime float64 `json:"profile_time"`
	Shot        struct {
		Pressure        float64 `json:"pressure"`
		Flow            float64 `json:"flow"`
		Weight          float64 `json:"weight"`
		GravimetricFlow float64 `json:"gravimetric_flow"`
	} `json:"shot"`
	Status string `json:"status"`
}

// Analyze computes metrics then asks the provider for a critique.
func (a *Analyzer) Analyze(ctx context.Context, in ShotInput) (*Analysis, error) {
	var samples []sample
	if len(in.Samples) > 0 {
		if err := json.Unmarshal(in.Samples, &samples); err != nil {
			return nil, fmt.Errorf("decode samples: %w", err)
		}
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("shot has no samples to analyze")
	}

	metrics := computeMetrics(samples)
	traceText := buildTraceTable(downsample(samples, 60))

	system := systemPrompt
	user := buildUserPrompt(in, metrics, traceText)

	// Retry transient provider errors (rate limits, overloaded servers,
	// 5xx). Backoff is exponential-with-jitter, capped by ctx deadline:
	// if the caller only gave us 10s, we won't burn it on sleeps.
	started := time.Now()
	summary, tok, err := a.completeWithRetry(ctx, system, user)
	if err != nil {
		return nil, err
	}

	rating, cleanSummary := extractRating(summary)

	return &Analysis{
		Model:     a.provider.Name(),
		CreatedAt: time.Now().UTC(),
		Rating:    rating,
		Summary:   strings.TrimSpace(cleanSummary),
		Metrics:   metrics,
		Usage: CallUsage{
			InputTokens:  tok.InputTokens,
			OutputTokens: tok.OutputTokens,
			DurationMs:   time.Since(started).Milliseconds(),
		},
	}, nil
}

// maxAttempts is the total number of provider calls (1 initial + 4 retries).
// 5 attempts with exponential backoff covers the typical Anthropic/Gemini
// overload window (15–45s) without pinning the handler for too long.
const maxAttempts = 5

// completeWithRetry calls the provider and retries transient failures.
// Non-retryable errors (auth, invalid model, bad input) return immediately.
func (a *Analyzer) completeWithRetry(ctx context.Context, system, user string) (string, TokenUsage, error) {
	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, tok, err := a.provider.Complete(ctx, system, user)
		if err == nil {
			return out, tok, nil
		}
		lastErr = err
		if !isTransient(err) || attempt == maxAttempts {
			return "", TokenUsage{}, err
		}
		// Jitter ±25% so concurrent auto-analyses don't stampede on the
		// same retry boundary after a provider-wide blip.
		jitter := time.Duration((rand.Float64() - 0.5) * float64(backoff) / 2)
		sleep := backoff + jitter
		slog.Info("ai provider transient error; retrying",
			"model", a.provider.Name(),
			"attempt", attempt,
			"sleep", sleep.String(),
			"err", err.Error(),
		)
		select {
		case <-ctx.Done():
			return "", TokenUsage{}, fmt.Errorf("%w (last: %v)", ctx.Err(), lastErr)
		case <-time.After(sleep):
		}
		if backoff < 16*time.Second {
			backoff *= 2
		}
	}
	return "", TokenUsage{}, lastErr
}

// isTransient reports whether err looks like a provider-side busy signal
// worth retrying. We inspect the message because all three providers
// surface errors as `fmt.Errorf("...: http %d: <body>")` — structuring
// them further would be nicer but isn't worth the churn for a retry
// decision that's already heuristic.
//
// Covered:
//   - HTTP 429 (rate limit) across all providers
//   - HTTP 503 / 502 / 504 (gateway/server unavailable)
//   - HTTP 529 (Anthropic "overloaded")
//   - Body tokens providers use even on 200s or inconsistent statuses:
//     "overloaded", "rate limit", "try again", "temporarily unavailable"
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "http 429"),
		strings.Contains(s, "http 502"),
		strings.Contains(s, "http 503"),
		strings.Contains(s, "http 504"),
		strings.Contains(s, "http 529"),
		strings.Contains(s, "overloaded"),
		strings.Contains(s, "rate limit"),
		strings.Contains(s, "rate_limit"),
		strings.Contains(s, "try again"),
		strings.Contains(s, "temporarily unavailable"),
		strings.Contains(s, "server had an error"),
		strings.Contains(s, "resource_exhausted"):
		return true
	}
	return false
}

// IsTransient exposes the retry predicate so HTTP handlers can distinguish
// provider-overload errors (which deserve a 503 + retry-later UX) from hard
// failures (auth, invalid model, bad input) that should surface as 502.
func IsTransient(err error) bool { return isTransient(err) }

// --- sample processing -----------------------------------------------------

func computeMetrics(ss []sample) Metrics {
	if len(ss) == 0 {
		return Metrics{}
	}
	var m Metrics
	var sumP, sumF float64
	var nP, nF int
	firstDrip := -1.0
	preEnd := -1.0
	start := ss[0].ProfileTime
	end := ss[len(ss)-1].ProfileTime
	m.Duration = roundTo((end-start)/1000.0, 2)

	for _, s := range ss {
		p := s.Shot.Pressure
		f := s.Shot.Flow
		w := s.Shot.Weight
		if p > m.PeakPressure {
			m.PeakPressure = p
		}
		if f > m.PeakFlow {
			m.PeakFlow = f
		}
		if p > 0.1 {
			sumP += p
			nP++
		}
		if f > 0.1 {
			sumF += f
			nF++
		}
		if firstDrip < 0 && w > 0.5 {
			firstDrip = s.ProfileTime / 1000.0
		}
		// Heuristic: "preinfusion" ends when pressure first crosses 6 bar.
		if preEnd < 0 && p >= 6.0 {
			preEnd = s.ProfileTime / 1000.0
		}
		m.FinalWeight = w
	}
	if nP > 0 {
		m.AvgPressure = roundTo(sumP/float64(nP), 2)
	}
	if nF > 0 {
		m.AvgFlow = roundTo(sumF/float64(nF), 2)
	}
	m.PeakPressure = roundTo(m.PeakPressure, 2)
	m.PeakFlow = roundTo(m.PeakFlow, 2)
	m.FinalWeight = roundTo(m.FinalWeight, 2)
	if firstDrip > 0 {
		m.FirstDripAt = roundTo(firstDrip, 2)
	}
	if preEnd > 0 {
		m.PreinfusionEnd = roundTo(preEnd, 2)
	}
	return m
}

// downsample picks n evenly spaced samples; returns all if len(ss) <= n.
func downsample(ss []sample, n int) []sample {
	if len(ss) <= n {
		return ss
	}
	out := make([]sample, 0, n)
	step := float64(len(ss)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		idx := int(math.Round(float64(i) * step))
		if idx >= len(ss) {
			idx = len(ss) - 1
		}
		out = append(out, ss[idx])
	}
	return out
}

func buildTraceTable(ss []sample) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, "t_s  | pressure_bar | flow_mls | weight_g")
	for _, s := range ss {
		fmt.Fprintf(&b, "%5.2f | %5.2f | %5.2f | %5.2f\n",
			s.ProfileTime/1000.0,
			roundTo(s.Shot.Pressure, 2),
			roundTo(s.Shot.Flow, 2),
			roundTo(s.Shot.Weight, 2),
		)
	}
	return b.String()
}

func roundTo(v float64, dp int) float64 {
	m := math.Pow(10, float64(dp))
	return math.Round(v*m) / m
}

// --- prompts ---------------------------------------------------------------

const systemPrompt = `You are an expert espresso coach reviewing a completed shot pulled on a Meticulous espresso machine. You will receive:

- shot metrics computed from the raw data,
- the loaded profile JSON (the intended pressure/flow targets),
- a downsampled trace of pressure, flow, and weight over time.

Your response MUST begin with a single line in this exact machine-parsed format — no heading, no prose, no bold, nothing before it:

    RATING: N/10 LABEL

Rules for that first line:
- N is an integer 0–10 (10 = textbook, 7 = good, 5 = flawed but drinkable, 3 = bad, 0 = undrinkable).
- LABEL is exactly one of: excellent, good, fine, off, bad.
- No other words on the line. No markdown, no punctuation after LABEL.
- This line is not optional. If you omit it the output will be rejected.

After that line, leave one blank line, then write the critique as markdown with this structure:

1. A short opening paragraph (2–3 sentences) summarising what happened and whether the shot matched intent.
2. A section titled **## What the numbers say** with 3–5 bullet points, each starting with a bold label (e.g. **Pressure:**, **Flow:**, **Weight:**) followed by a specific quantitative observation.
3. A section titled **## Preparation** with 0–3 numbered items for things the barista can change OFF the machine before the next shot: grind size, dose, tamp pressure, WDT / distribution, bean freshness, puck screen. One sentence each. Omit the section entirely if you have no preparation advice.
4. A section titled **## Recipe changes** with 0–5 numbered items for changes to the loaded profile. Each item must:
   - describe the change in plain prose first,
   - then end with a fenced machine-parsable directive on its own line. Use exactly ONE directive per item, in one of these forms:
     ` + "`SET variable <key> = <number>`" + ` — scalar write. ` + "`<key>`" + ` must exactly match the ` + "`key`" + ` field of a variable in the profile's ` + "`variables`" + ` array (NOT its display name).
     ` + "`REMOVE exit_trigger <type> FROM stage \"<name>\"`" + ` — drop every exit_trigger of the given ` + "`type`" + ` (e.g. flow, pressure, time, weight) from the named stage. ` + "`<name>`" + ` must exactly match the ` + "`name`" + ` field of a stage in the profile's ` + "`stages`" + ` array, in double quotes.
     ` + "`SET exit_trigger <type> = <number> ON stage \"<name>\"`" + ` — update the value of an exit_trigger on the named stage (inserts one if absent).
   - Only emit a directive for variables, stages, and triggers that actually exist in the profile JSON. If the change you want isn't backed by an existing entity, describe it in prose and do not emit a directive.
   Omit the section entirely if you have no recipe advice.

Be specific and quantitative — always cite the numbers you see. Avoid boilerplate. Do not invent sensor data. Never output JSON or code fences other than the directive forms listed above; plain markdown only.`

// ratingPattern matches a RATING line anywhere in the first few lines of
// the response. Spec says the very first line, but real-world LLMs will
// occasionally prepend a heading ("# Shot Review") or a bold version of
// the rating. Accept any of those and strip just the matched line.
// Tolerates: leading whitespace, optional bold "**", optional "- " bullet
// prefix, "rating"/"RATING", spaces around the slash, label optional.
var ratingPattern = regexp.MustCompile(`(?im)^[ \t]*(?:[-*]\s+)?\*{0,2}\s*rating\s*:\s*(\d{1,2})\s*/\s*10\b[^\S\n]*([A-Za-z]+)?[^\n]*\n?`)

// extractRating pulls the machine-readable rating line out of the model's
// response and returns it alongside the remaining markdown (which is the
// human-facing critique). If no RATING line is found in the first ~5
// lines (or ~400 chars) of output, we leave the text untouched and
// return nil — the UI then just hides the badge rather than showing a
// misleading zero.
func extractRating(summary string) (*Rating, string) {
	// Only scan the preamble: a rogue "rating: 7/10" deep inside the
	// Suggestions section shouldn't be pulled out.
	head, tail := splitHead(summary, 5, 400)
	loc := ratingPattern.FindStringSubmatchIndex(head)
	if loc == nil {
		return nil, summary
	}
	score := 0
	fmt.Sscanf(head[loc[2]:loc[3]], "%d", &score)
	if score < 0 {
		score = 0
	}
	if score > 10 {
		score = 10
	}
	label := ""
	if loc[4] >= 0 {
		label = strings.ToLower(head[loc[4]:loc[5]])
	}
	// Rebuild summary with the matched line excised.
	cleaned := head[:loc[0]] + head[loc[1]:] + tail
	// Drop any blank lines we now have at the very top.
	cleaned = strings.TrimLeft(cleaned, " \t\r\n")
	return &Rating{Score: score, Label: label}, cleaned
}

// splitHead slices off roughly the first `maxLines` lines (capped at
// `maxBytes` bytes) so we only hunt for the rating line in the preamble
// without having to scan large responses.
func splitHead(s string, maxLines, maxBytes int) (head, tail string) {
	lines := 0
	for i := 0; i < len(s) && i < maxBytes; i++ {
		if s[i] == '\n' {
			lines++
			if lines >= maxLines {
				return s[:i+1], s[i+1:]
			}
		}
	}
	if len(s) <= maxBytes {
		return s, ""
	}
	return s[:maxBytes], s[maxBytes:]
}

func buildUserPrompt(in ShotInput, m Metrics, trace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Shot: %q\n", in.Name)
	fmt.Fprintf(&b, "Profile: %q\n\n", in.ProfileName)

	if beanBlock := renderBeanBlock(in.Bean, in.Grind, in.GrindRPM); beanBlock != "" {
		b.WriteString(beanBlock)
		b.WriteString("\n")
	}

	fmt.Fprintln(&b, "## Metrics")
	mb, _ := json.MarshalIndent(m, "", "  ")
	b.Write(mb)
	b.WriteString("\n\n")

	if len(in.Profile) > 0 && string(in.Profile) != "null" {
		fmt.Fprintln(&b, "## Profile JSON")
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, in.Profile, "", "  "); err == nil {
			b.Write(pretty.Bytes())
		} else {
			b.Write(in.Profile)
		}
		b.WriteString("\n\n")
	}

	fmt.Fprintln(&b, "## Trace (downsampled)")
	b.WriteString(trace)
	return b.String()
}

// renderBeanBlock formats the bag/grinder context as a small markdown
// section. Returns "" when there's nothing worth showing so the caller
// can skip the heading entirely. Days-off-roast is computed client-side
// from RoastDate (ISO yyyy-mm-dd) — the LLM doesn't need to know the
// current date.
func renderBeanBlock(bean *BeanInfo, grind string, rpm *float64) string {
	if bean == nil && grind == "" && rpm == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Beans & grind\n")
	if bean != nil {
		if bean.Name != "" {
			fmt.Fprintf(&b, "- Name: %s\n", bean.Name)
		}
		if bean.Roaster != "" {
			fmt.Fprintf(&b, "- Roaster: %s\n", bean.Roaster)
		}
		if bean.Origin != "" {
			fmt.Fprintf(&b, "- Origin: %s\n", bean.Origin)
		}
		if bean.Process != "" {
			fmt.Fprintf(&b, "- Process: %s\n", bean.Process)
		}
		if bean.RoastLevel != "" {
			fmt.Fprintf(&b, "- Roast level: %s\n", bean.RoastLevel)
		}
		if bean.RoastDate != "" {
			fmt.Fprintf(&b, "- Roast date: %s", bean.RoastDate)
			if t, err := time.Parse("2006-01-02", bean.RoastDate); err == nil {
				days := int(math.Round(time.Since(t).Hours() / 24))
				if days >= 0 {
					fmt.Fprintf(&b, " (%d days off roast)", days)
				}
			}
			b.WriteString("\n")
		}
		if bean.Notes != "" {
			// Keep notes on one line so the LLM doesn't mistake them
			// for markdown structure.
			single := strings.Join(strings.Fields(bean.Notes), " ")
			fmt.Fprintf(&b, "- Roaster notes: %s\n", single)
		}
	}
	if grind != "" {
		fmt.Fprintf(&b, "- Grind size: %s\n", grind)
	}
	if rpm != nil {
		fmt.Fprintf(&b, "- Grind RPM: %g\n", *rpm)
	}
	return b.String()
}
