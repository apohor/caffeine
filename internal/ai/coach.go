package ai

// Profile coach: given a shot plus its recent siblings (same profile),
// the coach produces ONE focused, actionable recipe suggestion to try
// on the next pull. Distinct from Analyzer, which generates a full
// critique — this is a short, single-change nudge, cheaper and easier
// to act on.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CoachInput is the minimal bundle of context the coach prompt needs.
type CoachInput struct {
	Shot       ShotInput
	ShotRating *int          // 1..5 or nil (user's own rating)
	ShotNote   string        // user's own note
	Siblings   []ShotSummary // recent shots with the same profile, newest first
}

// ShotSummary is the compact per-shot line item the coach sees for
// historical comparison. Keep it cheap; we never include raw samples.
type ShotSummary struct {
	Name         string  `json:"name"`
	TimeISO      string  `json:"time_iso"`
	Duration     float64 `json:"duration_s"`
	PeakPressure float64 `json:"peak_pressure_bar"`
	AvgPressure  float64 `json:"avg_pressure_bar"`
	PeakFlow     float64 `json:"peak_flow_mls"`
	FinalWeight  float64 `json:"final_weight_g"`
	FirstDripAt  float64 `json:"first_drip_s,omitempty"`
	Rating       *int    `json:"user_rating,omitempty"`
	Note         string  `json:"user_note,omitempty"`
}

// Suggestion is the structured output of the coach. The LLM is asked to
// return JSON so the UI can render labels/values directly.
type Suggestion struct {
	Model      string    `json:"model"`
	CreatedAt  time.Time `json:"created_at"`
	Change     string    `json:"change"`            // short imperative, e.g. "Grind 2 notches finer"
	Rationale  string    `json:"rationale"`         // 1-2 sentences citing the numbers
	VarKey     string    `json:"var_key,omitempty"` // profile variable key or ""
	Before     *float64  `json:"before,omitempty"`  // current profile value
	After      *float64  `json:"after,omitempty"`   // proposed new value
	Confidence string    `json:"confidence"`        // "low"|"medium"|"high"
	Usage      CallUsage `json:"-"`
}

const coachSystemPrompt = `You are an espresso coach. Review a just-pulled shot and its recent siblings on the same profile, and propose exactly ONE concrete change to try for the next pull. Favour changes that are easy to act on and backed by the numbers.

Respond with a single JSON object, no markdown fences, matching this schema:
{
  "change":      "short imperative sentence, <= 80 chars",
  "rationale":   "one or two sentences citing at least one number",
  "var_key":     "profile variable key from the profile JSON, or empty string if not applicable",
  "before":      number or null,
  "after":       number or null,
  "confidence":  "low" | "medium" | "high"
}

Rules:
- Exactly ONE suggestion. Pick the highest-leverage change.
- If you propose a recipe variable change, var_key MUST match an existing key in the profile's variables array.
- If the change is off-machine (grind, dose, distribution, beans), var_key is empty and before/after are null.
- Never invent sensor data. If the data doesn't support a strong suggestion, return "confidence": "low".
- Output VALID JSON. No text before or after the object.`

// Coach wraps a Provider to produce structured single-suggestion output.
type Coach struct {
	provider Provider
}

// NewCoach wraps a Provider.
func NewCoach(p Provider) *Coach { return &Coach{provider: p} }

// ModelName exposes the provider identifier.
func (c *Coach) ModelName() string { return c.provider.Name() }

// Suggest runs the coach and returns the parsed suggestion.
func (c *Coach) Suggest(ctx context.Context, in CoachInput) (*Suggestion, error) {
	user := buildCoachPrompt(in)
	started := time.Now()
	out, tok, err := c.provider.Complete(ctx, coachSystemPrompt, user)
	if err != nil {
		return nil, err
	}
	cleaned := stripJSONFences(out)
	var parsed struct {
		Change     string   `json:"change"`
		Rationale  string   `json:"rationale"`
		VarKey     string   `json:"var_key"`
		Before     *float64 `json:"before"`
		After      *float64 `json:"after"`
		Confidence string   `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("coach: parse suggestion: %w (raw=%q)", err, firstN(cleaned, 200))
	}
	if parsed.Change == "" {
		return nil, fmt.Errorf("coach: empty suggestion")
	}
	if parsed.Confidence == "" {
		parsed.Confidence = "medium"
	}
	return &Suggestion{
		Model:      c.provider.Name(),
		CreatedAt:  time.Now().UTC(),
		Change:     parsed.Change,
		Rationale:  parsed.Rationale,
		VarKey:     parsed.VarKey,
		Before:     parsed.Before,
		After:      parsed.After,
		Confidence: parsed.Confidence,
		Usage: CallUsage{
			InputTokens:  tok.InputTokens,
			OutputTokens: tok.OutputTokens,
			DurationMs:   time.Since(started).Milliseconds(),
		},
	}, nil
}

// buildCoachPrompt renders the user turn: current shot metrics, profile
// JSON, recent siblings, user rating/note.
func buildCoachPrompt(in CoachInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current shot: %q on profile %q\n\n", in.Shot.Name, in.Shot.ProfileName)
	if in.ShotRating != nil {
		fmt.Fprintf(&b, "User rating: %d/5\n", *in.ShotRating)
	}
	if in.ShotNote != "" {
		fmt.Fprintf(&b, "User note: %s\n", in.ShotNote)
	}
	if beanBlock := renderBeanBlock(in.Shot.Bean, in.Shot.Grind, in.Shot.GrindRPM); beanBlock != "" {
		b.WriteString("\n")
		b.WriteString(beanBlock)
	}
	// Metrics for the current shot (reuse the analyzer's computation).
	var samples []sample
	if len(in.Shot.Samples) > 0 {
		_ = json.Unmarshal(in.Shot.Samples, &samples)
	}
	metrics := computeMetrics(samples)
	mb, _ := json.MarshalIndent(metrics, "", "  ")
	fmt.Fprintf(&b, "\n## Current shot metrics\n%s\n", string(mb))

	if len(in.Shot.Profile) > 0 && string(in.Shot.Profile) != "null" {
		fmt.Fprintln(&b, "\n## Profile JSON")
		b.Write(in.Shot.Profile)
		b.WriteString("\n")
	}

	if len(in.Siblings) > 0 {
		fmt.Fprintln(&b, "\n## Recent shots on the same profile (newest first)")
		sb, _ := json.MarshalIndent(in.Siblings, "", "  ")
		b.Write(sb)
		b.WriteString("\n")
	}
	return b.String()
}

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i > 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
