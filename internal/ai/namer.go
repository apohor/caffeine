package ai

// Profile naming assistant: given a Meticulous profile JSON, suggest
// a short, human-friendly name (e.g. "Steady 9-bar ramp" or
// "Fast Italian espresso"). Useful on import when the supplied name
// is generic or numeric.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProfileNameInput is the bundle the namer sees.
type ProfileNameInput struct {
	Profile     json.RawMessage
	CurrentName string
}

// ProfileNameSuggestion is the result — a short name plus a one-line
// reason so the user can tell what the model picked up on.
type ProfileNameSuggestion struct {
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
	Reason    string    `json:"reason"`
	Usage     CallUsage `json:"-"`
}

const profileNameSystemPrompt = `You are naming espresso profiles. Given a Meticulous profile JSON, produce a short, human-friendly name describing the shot style.

Rules:
- Name is 2-5 words, Title Case, no quotes or punctuation at the end.
- Capture the headline character: pressure/flow style, temp, ramp shape, length — whatever stands out.
- Do NOT repeat the current name. If it is already descriptive, produce a refined variant.
- Keep it practical: "Fast Italian Ristretto", "Long 6-Bar Pour", "Hot Pressure-Limited Lungo" — not "Optimal Pro Shot".

Return a single JSON object, no markdown fences:
{
  "name":   "string (2-5 words)",
  "reason": "one sentence explaining the choice, citing a number or stage from the profile"
}`

// Namer wraps a Provider to produce profile names.
type Namer struct{ provider Provider }

// NewNamer wraps a provider.
func NewNamer(p Provider) *Namer { return &Namer{provider: p} }

// ModelName returns the provider identifier.
func (n *Namer) ModelName() string { return n.provider.Name() }

// Suggest asks the LLM for a name suggestion.
func (n *Namer) Suggest(ctx context.Context, in ProfileNameInput) (*ProfileNameSuggestion, error) {
	if len(in.Profile) == 0 {
		return nil, fmt.Errorf("namer: empty profile JSON")
	}
	var b strings.Builder
	if in.CurrentName != "" {
		fmt.Fprintf(&b, "Current name: %q\n\n", in.CurrentName)
	}
	b.WriteString("Profile JSON:\n")
	b.Write(in.Profile)
	b.WriteString("\n")

	started := time.Now()
	out, tok, err := n.provider.Complete(ctx, profileNameSystemPrompt, b.String())
	if err != nil {
		return nil, err
	}
	cleaned := stripJSONFences(out)
	var parsed struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("namer: parse: %w (raw=%q)", err, firstN(cleaned, 200))
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		return nil, fmt.Errorf("namer: empty name")
	}
	return &ProfileNameSuggestion{
		Model:     n.provider.Name(),
		CreatedAt: time.Now().UTC(),
		Name:      name,
		Reason:    strings.TrimSpace(parsed.Reason),
		Usage: CallUsage{
			InputTokens:  tok.InputTokens,
			OutputTokens: tok.OutputTokens,
			DurationMs:   time.Since(started).Milliseconds(),
		},
	}, nil
}
