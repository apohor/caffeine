package ai

// Shot-to-shot comparator: given two shots, explain the headline
// differences and why they likely tasted different. Returned as a short
// markdown blob the UI renders verbatim.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CompareInput bundles two shots (A and B) plus their user feedback.
type CompareInput struct {
	A       ShotInput
	B       ShotInput
	ARating *int
	BRating *int
	ANote   string
	BNote   string
}

// Comparison is the LLM output plus bookkeeping.
type Comparison struct {
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	Markdown  string    `json:"markdown"`
	Usage     CallUsage `json:"-"`
}

const compareSystemPrompt = `You are an espresso reviewer comparing two shots side by side. Produce a short markdown report with:

## Headline
(one sentence summarising the key difference)

## Metric diffs
A bulleted list of the 3-5 biggest numeric differences, each line: "- <metric>: A=… B=… (→ meaning)".

## Likely cause
1-3 sentences on what probably drove the difference — grind, dose, profile change, extraction dynamics. Reference the numbers.

## Next move
One concrete, practical suggestion for the next pull.

Do not invent data. If one shot lacks information, say so.`

// Comparator wraps a Provider for the compare task.
type Comparator struct {
	provider Provider
}

// NewComparator wraps a provider.
func NewComparator(p Provider) *Comparator { return &Comparator{provider: p} }

// ModelName returns the provider identifier.
func (c *Comparator) ModelName() string { return c.provider.Name() }

// Compare runs the comparator and returns the markdown report.
func (c *Comparator) Compare(ctx context.Context, in CompareInput) (*Comparison, error) {
	user := buildComparePrompt(in)
	started := time.Now()
	out, tok, err := c.provider.Complete(ctx, compareSystemPrompt, user)
	if err != nil {
		return nil, err
	}
	md := strings.TrimSpace(out)
	if md == "" {
		return nil, fmt.Errorf("compare: empty response")
	}
	return &Comparison{
		Model:     c.provider.Name(),
		CreatedAt: time.Now().UTC(),
		Markdown:  md,
		Usage: CallUsage{
			InputTokens:  tok.InputTokens,
			OutputTokens: tok.OutputTokens,
			DurationMs:   time.Since(started).Milliseconds(),
		},
	}, nil
}

func buildComparePrompt(in CompareInput) string {
	var b strings.Builder
	writeSide := func(tag string, si ShotInput, rating *int, note string) {
		fmt.Fprintf(&b, "## Shot %s: %q on profile %q\n", tag, si.Name, si.ProfileName)
		if rating != nil {
			fmt.Fprintf(&b, "User rating: %d/5\n", *rating)
		}
		if note != "" {
			fmt.Fprintf(&b, "User note: %s\n", note)
		}
		if bean := renderBeanBlock(si.Bean, si.Grind, si.GrindRPM); bean != "" {
			b.WriteString(bean)
		}
		var samples []sample
		if len(si.Samples) > 0 {
			_ = json.Unmarshal(si.Samples, &samples)
		}
		metrics := computeMetrics(samples)
		mb, _ := json.MarshalIndent(metrics, "", "  ")
		fmt.Fprintf(&b, "Metrics:\n%s\n", string(mb))
		if len(si.Profile) > 0 && string(si.Profile) != "null" {
			fmt.Fprintf(&b, "Profile JSON:\n")
			b.Write(si.Profile)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	writeSide("A", in.A, in.ARating, in.ANote)
	writeSide("B", in.B, in.BRating, in.BNote)
	return b.String()
}
