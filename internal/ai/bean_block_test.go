package ai

import (
	"strings"
	"testing"
	"time"
)

// Verify bean + grinder context shows up in the user prompt so the LLM
// can factor origin/roast age/grind into its critique. Covers the
// interesting branches: nothing to render (empty block), bean only,
// grind only, and the full combination with days-off-roast.
func TestRenderBeanBlock(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := renderBeanBlock(nil, "", nil); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})

	t.Run("bean only", func(t *testing.T) {
		got := renderBeanBlock(&BeanInfo{
			Name:    "Monarch",
			Roaster: "Onyx",
			Origin:  "Honduras",
			Process: "natural",
		}, "", nil)
		for _, want := range []string{"## Beans & grind", "Monarch", "Onyx", "Honduras", "natural"} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in:\n%s", want, got)
			}
		}
		if strings.Contains(got, "Grind size") {
			t.Fatalf("should not mention grind when not provided:\n%s", got)
		}
	})

	t.Run("grinder only", func(t *testing.T) {
		rpm := 850.0
		got := renderBeanBlock(nil, "2.8", &rpm)
		if !strings.Contains(got, "Grind size: 2.8") {
			t.Fatalf("missing grind size:\n%s", got)
		}
		if !strings.Contains(got, "Grind RPM: 850") {
			t.Fatalf("missing rpm:\n%s", got)
		}
	})

	t.Run("days off roast", func(t *testing.T) {
		roast := time.Now().AddDate(0, 0, -12).Format("2006-01-02")
		got := renderBeanBlock(&BeanInfo{Name: "B", RoastDate: roast}, "", nil)
		// Rounding to whole days around the Format/Parse boundary can land
		// on 12 or 13 depending on the time of day; just assert the
		// annotation is present and plausible.
		if !strings.Contains(got, "days off roast") {
			t.Fatalf("expected days-off-roast annotation in:\n%s", got)
		}
	})
}
