package router

import (
	"testing"
	"time"
)

// BenchmarkCategorize benchmarks the Categorize function on the hot path:
// all-lowercase prompts (no toUnicodeLower allocation) with early-exit
// matches. This exercises the pre-compiled regex path (issue #877).
func BenchmarkCategorize(b *testing.B) {
	// All-lowercase so toUnicodeLower returns s unchanged (zero allocation).
	// Mix of early-exit (first category) and late-exit (CategoryOther) prompts.
	prompts := []string{
		"fix the css padding on the header",            // CategoryCSS (early)
		"debug this memory leak in production",         // CategoryDebugging (early)
		"what is the capital of france",                // CategoryOther (late - no match)
		"design the system architecture for payments",  // CategoryArchitecture (early)
		"please refactor this method to be cleaner",    // CategoryRefactoring (early)
		"explain how goroutines work",                  // CategoryOther (late)
		"generate the crud boilerplate for this model", // CategoryBoilerplate (early)
		"write a docstring for this method",            // CategoryDocumentation (early)
		"check for sql injection vulnerabilities",      // CategorySecurity (early)
		"optimize this sql query for the users table",  // CategoryData (early)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, prompt := range prompts {
			Categorize(prompt)
		}
	}
}

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
		// New categories from issue #528
		// Note: with word-boundary matching (issue #797), "test" in "tests" and "mock"
		// in "mocks" don't match because there's no word boundary after them before 's'.
		{"testing_unit_test", "generate unit tests for the auth module", CategoryOther},
		{"testing_test_case", "write a test case for the login function", CategoryTesting},
		{"testing_coverage", "run test coverage on the new feature", CategoryTesting},
		{"testing_mock", "add mocks for the database calls", CategoryData},
		{"testing_fixture", "set up test fixtures for the API", CategoryTesting},
		{"security_scan", "run a security scan on the input handler", CategorySecurity},
		{"security_vulnerability", "check for SQL injection vulnerabilities", CategorySecurity},
		{"security_xss", "prevent XSS attacks in the template", CategorySecurity},
		{"security_owasp", "follow owasp best practices for auth", CategorySecurity},
		{"data_sql_query", "optimize this SQL query for the users table", CategoryData},
		{"data_database", "design the database schema for orders", CategoryData},
		{"data_migration", "write a migration to add the audit column", CategoryData},
		{"data_json", "parse json from the webhook payload", CategoryData},
		// Existing uncategorized
		{"other", "what is the capital of France", CategoryOther},
		{"empty", "", CategoryOther},
		// Non-ASCII + uppercase ASCII keyword (issue #587: Unicode-aware lowercasing)
		{"chinese_with_REFACTOR", "请REFACTOR这个函数", CategoryRefactoring},
		{"russian_with_DEBUG", "помогите DEBUG", CategoryDebugging},
		{"greek_with_CSS", "αλλαγή CSS", CategoryCSS},
		{"arabic_with_DEBUG", "تصحيح DEBUG", CategoryDebugging},
		{"chinese_no_keyword", "你好世界", CategoryOther},
		{"russian_no_keyword", "привет мир", CategoryOther},
		// Word-boundary cases (issue #797): keywords must not match inside other words.
		// "test the css" → CSS because CSS category is checked before Testing.
		{"css_after_test_word", "test the css", CategoryCSS},
		{"re_factor_hyphen", "re-factor this", CategoryRefactoring},
		{"re_factoring_hyphen", "re-factoring the function", CategoryRefactoring},
		// Substring false-positives must NOT match: contest, detest, subtest
		// contain "test" but have no word boundary before it.
		{"contest_no_match", "contest app", CategoryOther},
		{"detest_no_match", "detest this pattern", CategoryOther},
		{"subtest_no_match", "subtest function", CategoryOther},
		// "css3 styling" contains "styling" (CSS keyword) with proper word boundaries.
		{"css3_has_styling", "css3 styling for the header", CategoryCSS},
		// "unit-testing" and "integration-testing" contain "unit" and "test" as words,
		// but "unit-test" as a phrase doesn't match because "unit-testing" has
		// "unit" followed by hyphen, not space. Testing keywords "test" and "mock"
		// don't match in "unit-testing" or "mocks" because of missing trailing boundaries.
		{"unit_testing_has_no_test", "unit-testing the code", CategoryOther},
		{"integration_testing_has_no_test", "integration-testing setup", CategoryOther},
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
	got, err := cs.LocalConfidence(CategoryDebugging)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got >= DefaultConfidenceFloor {
		t.Errorf("LocalConfidence = %v, want < %v (floor)", got, DefaultConfidenceFloor)
	}
}

func TestConfidenceHighScoresAboveCeiling(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	for i := 0; i < 6; i++ {
		cs.RecordOutcome(CategoryCSS, RouteLocal, 4+i%2) // 4s and 5s
	}
	got, err := cs.LocalConfidence(CategoryCSS)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
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
	got, err := cs.LocalConfidence(CategoryDebugging)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != NeutralConfidence {
		t.Errorf("LocalConfidence = %v, want %v (neutral)", got, NeutralConfidence)
	}
}

func TestConfidenceUnknownCategoryIsNeutral(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	got, err := cs.LocalConfidence(CategoryArchitecture)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != NeutralConfidence {
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
	got, err := cs.LocalConfidence(CategoryRefactoring)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != NeutralConfidence {
		t.Errorf("expired-only LocalConfidence = %v, want %v (neutral)", got, NeutralConfidence)
	}
	// Adding fresh low scores tips it below the floor once past min-samples.
	for i := 0; i < 6; i++ {
		cs.RecordOutcome(CategoryRefactoring, RouteLocal, 1)
	}
	got, err = cs.LocalConfidence(CategoryRefactoring)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got >= DefaultConfidenceFloor {
		t.Errorf("fresh LocalConfidence = %v, want < %v", got, DefaultConfidenceFloor)
	}
}

func TestConfidenceIgnoresOutOfRangeScores(t *testing.T) {
	cs := newTestConfidenceStore(t, 1, time.Hour)
	cs.RecordOutcome(CategoryOther, RouteLocal, 0) // parse failure, ignored
	cs.RecordOutcome(CategoryOther, RouteLocal, 9) // out of range, ignored
	got, err := cs.LocalConfidence(CategoryOther)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != NeutralConfidence {
		t.Errorf("LocalConfidence with only invalid scores = %v, want neutral", got)
	}
}

func TestConfidenceOnlyLocalRouteCounts(t *testing.T) {
	cs := newTestConfidenceStore(t, 3, time.Hour)
	// Frontier outcomes must not influence LocalConfidence.
	for i := 0; i < 5; i++ {
		cs.RecordOutcome(CategoryDebugging, RouteFrontier, 5)
	}
	got, err := cs.LocalConfidence(CategoryDebugging)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != NeutralConfidence {
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
	got, err := cs.LocalConfidence(CategoryOther)
	if err != nil {
		t.Fatalf("LocalConfidence: %v", err)
	}
	if got != 0.5 {
		t.Errorf("LocalConfidence mixed = %v, want 0.5", got)
	}
}

func TestOpenConfidenceStoreRejectsEmptyPath(t *testing.T) {
	if _, err := OpenConfidenceStore(ConfidenceConfig{Path: ""}); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestConfidenceRecordOutcomeRejectsEmptyCategory verifies that an empty
// category is surfaced as an error rather than silently coerced to
// CategoryOther (issue #591). This makes upstream RecordOutcome bugs
// visible instead of polluting the "other" bucket.
func TestConfidenceRecordOutcomeRejectsEmptyCategory(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	if err := cs.RecordOutcome("", RouteLocal, 3); err == nil {
		t.Fatal("RecordOutcome with empty category: expected non-nil error, got nil")
	}
}

// TestConfidenceRecordAtRejectsEmptyCategory covers the test-helper
// recordAt path with the same guard.
func TestConfidenceRecordAtRejectsEmptyCategory(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	if err := cs.recordAt("", RouteLocal, 3, time.Now().UTC()); err == nil {
		t.Fatal("recordAt with empty category: expected non-nil error, got nil")
	}
}

// TestConfidenceLocalConfidenceRejectsEmptyCategory verifies that an empty
// category is surfaced as an error rather than silently coerced to
// CategoryOther (issue #802). This makes upstream LocalConfidence bugs
// visible instead of silently returning neutral confidence.
func TestConfidenceLocalConfidenceRejectsEmptyCategory(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, time.Hour)
	got, err := cs.LocalConfidence("")
	if err == nil {
		t.Fatal("LocalConfidence with empty category: expected non-nil error, got nil")
	}
	if got != NeutralConfidence {
		t.Errorf("LocalConfidence with empty category = %v, want %v (neutral)", got, NeutralConfidence)
	}
}

// TestConfidenceCleanupDeletesOldRows verifies that after cleanEveryN inserts,
// rows older than 2*window are deleted (issue #834).
func TestConfidenceCleanupDeletesOldRows(t *testing.T) {
	window := time.Hour
	cs, err := OpenConfidenceStore(ConfidenceConfig{
		Path:       ":memory:",
		MinSamples: 1,
		Window:     window,
	})
	if err != nil {
		t.Fatalf("OpenConfidenceStore: %v", err)
	}
	defer cs.Close()

	old := time.Now().UTC().Add(-3 * window) // older than 2*window
	for i := 0; i < cleanEveryN+10; i++ {
		cs.recordAt("test-cleanup", RouteLocal, 3, old)
	}

	got := cs.RowsTotal()
	if got != 10 {
		t.Errorf("RowsTotal after %d inserts = %d, want 10 (1 old row deleted at insert 1000, then 10 more added)",
			cleanEveryN+10, got)
	}
}
