package router

import (
	"testing"
	"time"
)

func TestCategorize(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"css", "Please tweak the CSS padding on the header", CategoryCSS},
		{"tailwind", "make this responsive with tailwind", CategoryCSS},
		{"refactor", "Refactor this function to remove duplication", CategoryRefactoring},
		{"rename", "rename the variable across the file", CategoryRefactoring},
		{"debug", "help me debug why this test is failing", CategoryDebugging},
		{"exception", "I get an exception and a stack trace here", CategoryDebugging},
		{"architecture", "design the system architecture for the payment service", CategoryArchitecture},
		{"arch-word", "review the architectural design of this module", CategoryArchitecture},
		{"boilerplate", "generate the CRUD boilerplate for this model", CategoryBoilerplate},
		{"documentation", "write a docstring for this method", CategoryDocumentation},
		{"other", "what is the capital of France", CategoryOther},
		{"empty", "", CategoryOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Categorize(tc.prompt); got != tc.want {
				t.Errorf("Categorize(%q) = %q, want %q", tc.prompt, got, tc.want)
			}
		})
	}
}

// newTestConfidenceStore opens an in-memory store with a small min-samples
// gate so the round-trip tests do not need to insert dozens of rows.
func newTestConfidenceStore(t *testing.T, minSamples int, window time.Duration) *SQLiteConfidenceStore {
	t.Helper()
	cs, err := OpenConfidenceStore(ConfidenceConfig{
		Path:       ":memory:",
		MinSamples: minSamples,
		Window:     window,
	})
	if err != nil {
		t.Fatalf("OpenConfidenceStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestConfidenceLowScoresBiasFrontier(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	for i := 0; i < 6; i++ {
		cs.RecordOutcome(CategoryDebugging, RouteLocal, 1+i%2) // 1s and 2s
	}
	got := cs.LocalConfidence(CategoryDebugging)
	if got >= DefaultConfidenceFloor {
		t.Errorf("LocalConfidence = %v, want < %v (floor)", got, DefaultConfidenceFloor)
	}
}

func TestConfidenceHighScoresAboveCeiling(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	for i := 0; i < 6; i++ {
		cs.RecordOutcome(CategoryCSS, RouteLocal, 4+i%2) // 4s and 5s
	}
	got := cs.LocalConfidence(CategoryCSS)
	if got <= DefaultConfidenceCeiling {
		t.Errorf("LocalConfidence = %v, want > %v (ceiling)", got, DefaultConfidenceCeiling)
	}
}

func TestConfidenceInsufficientSamplesIsNeutral(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	// Only 4 outcomes: below the min-samples gate of 5.
	for i := 0; i < 4; i++ {
		cs.RecordOutcome(CategoryDebugging, RouteLocal, 1)
	}
	if got := cs.LocalConfidence(CategoryDebugging); got != NeutralConfidence {
		t.Errorf("LocalConfidence = %v, want %v (neutral)", got, NeutralConfidence)
	}
}

func TestConfidenceUnknownCategoryIsNeutral(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	if got := cs.LocalConfidence(CategoryArchitecture); got != NeutralConfidence {
		t.Errorf("LocalConfidence(no data) = %v, want %v", got, NeutralConfidence)
	}
}

func TestConfidenceSlidingWindowExpiry(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	old := time.Now().UTC().Add(-2 * time.Hour) // outside the 1h window
	for i := 0; i < 8; i++ {
		cs.recordAt(CategoryRefactoring, RouteLocal, 1, old)
	}
	// All rows are stale, so the window sees zero samples -> neutral.
	if got := cs.LocalConfidence(CategoryRefactoring); got != NeutralConfidence {
		t.Errorf("expired-only LocalConfidence = %v, want %v (neutral)", got, NeutralConfidence)
	}
	// Adding fresh low scores tips it below the floor once past min-samples.
	for i := 0; i < 6; i++ {
		cs.RecordOutcome(CategoryRefactoring, RouteLocal, 1)
	}
	if got := cs.LocalConfidence(CategoryRefactoring); got >= DefaultConfidenceFloor {
		t.Errorf("fresh LocalConfidence = %v, want < %v", got, DefaultConfidenceFloor)
	}
}

func TestConfidenceIgnoresOutOfRangeScores(t *testing.T) {
	cs := newTestConfidenceStore(t, 1, time.Hour)
	cs.RecordOutcome(CategoryOther, RouteLocal, 0) // parse failure, ignored
	cs.RecordOutcome(CategoryOther, RouteLocal, 9) // out of range, ignored
	if got := cs.LocalConfidence(CategoryOther); got != NeutralConfidence {
		t.Errorf("LocalConfidence with only invalid scores = %v, want neutral", got)
	}
}

func TestConfidenceOnlyLocalRouteCounts(t *testing.T) {
	cs := newTestConfidenceStore(t, 3, time.Hour)
	// Frontier outcomes must not influence LocalConfidence.
	for i := 0; i < 5; i++ {
		cs.RecordOutcome(CategoryDebugging, RouteFrontier, 5)
	}
	if got := cs.LocalConfidence(CategoryDebugging); got != NeutralConfidence {
		t.Errorf("LocalConfidence with only frontier rows = %v, want neutral", got)
	}
}

func TestConfidenceMixedScoresFraction(t *testing.T) {
	cs := newTestConfidenceStore(t, 4, time.Hour)
	// 2 successes (>=3) and 2 failures -> 0.5 exactly.
	cs.RecordOutcome(CategoryOther, RouteLocal, 5)
	cs.RecordOutcome(CategoryOther, RouteLocal, 4)
	cs.RecordOutcome(CategoryOther, RouteLocal, 2)
	cs.RecordOutcome(CategoryOther, RouteLocal, 1)
	if got := cs.LocalConfidence(CategoryOther); got != 0.5 {
		t.Errorf("LocalConfidence mixed = %v, want 0.5", got)
	}
}

func TestOpenConfidenceStoreRejectsEmptyPath(t *testing.T) {
	if _, err := OpenConfidenceStore(ConfidenceConfig{Path: ""}); err == nil {
		t.Fatal("expected error for empty path")
	}
}
