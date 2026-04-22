package ai

import "testing"

func TestExtractRating(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantNil  bool
		wantScr  int
		wantLbl  string
		wantRest string
	}{
		{
			name:     "canonical",
			in:       "RATING: 7/10 good\n\nNice shot, pressure peaked at 9 bar.",
			wantScr:  7,
			wantLbl:  "good",
			wantRest: "Nice shot, pressure peaked at 9 bar.",
		},
		{
			name:     "bolded line",
			in:       "**RATING: 9/10 excellent**\n\nTextbook.",
			wantScr:  9,
			wantLbl:  "excellent",
			wantRest: "Textbook.",
		},
		{
			name:     "lowercase rating",
			in:       "rating: 4/10 off\nSomething is up with the grind.",
			wantScr:  4,
			wantLbl:  "off",
			wantRest: "Something is up with the grind.",
		},
		{
			name:     "leading blank line",
			in:       "\n  RATING: 10 / 10 excellent\nBody goes here.",
			wantScr:  10,
			wantLbl:  "excellent",
			wantRest: "Body goes here.",
		},
		{
			name:     "heading before rating",
			in:       "# Shot Review\n\nRATING: 6/10 fine\n\nBody goes here.",
			wantScr:  6,
			wantLbl:  "fine",
			wantRest: "# Shot Review\n\nBody goes here.",
		},
		{
			name:     "no label",
			in:       "RATING: 8/10\nBody.",
			wantScr:  8,
			wantLbl:  "",
			wantRest: "Body.",
		},
		{
			name:    "no rating line",
			in:      "This is just markdown with no rating.",
			wantNil: true,
		},
		{
			name:    "garbage rating is rejected",
			in:      "RATING: bad shot, no score here.",
			wantNil: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, rest := extractRating(c.in)
			if c.wantNil {
				if r != nil {
					t.Fatalf("expected nil rating, got %+v (rest=%q)", r, rest)
				}
				if rest != c.in {
					t.Fatalf("expected text unchanged when no rating; got %q", rest)
				}
				return
			}
			if r == nil {
				t.Fatalf("expected rating, got nil (rest=%q)", rest)
			}
			if r.Score != c.wantScr {
				t.Errorf("score: got %d want %d", r.Score, c.wantScr)
			}
			if r.Label != c.wantLbl {
				t.Errorf("label: got %q want %q", r.Label, c.wantLbl)
			}
			if rest != c.wantRest {
				t.Errorf("rest: got %q want %q", rest, c.wantRest)
			}
		})
	}
}
