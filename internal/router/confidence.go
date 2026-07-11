package router

// confidence.go closes the feedback loop between the async LLM-as-a-judge
// evaluator (internal/judge, issue #15) and the routing decision (issue #47).
//
// The judge scores ~10% of local-route responses on a 1..5 scale. Those
// scores were previously fire-and-forget. This file introduces a
// ConfidenceStore that aggregates historical judge scores by task
// category and exposes a single "how well does local do on this kind of
// task?" signal (0.0..1.0). The SLM router (slm.go) consumes that signal
// via DecideWithConfidence and nudges its system prompt toward frontier
// (low confidence) or local (high confidence).
//
// Dependency direction: internal/router does NOT import internal/judge.
// cmd/nexus/main.go owns both packages and bridges JudgeScore -> RecordOutcome.
// The confidence store lives here so the handler can call it without pulling
// in the judge package (the AGENTS.md hot-path dependency rule).

// NeutralConfidence is the confidence value that produces routing behaviour
// byte-for-byte identical to the pre-issue-47 path: no system-prompt
// augmentation. It is returned whenever there is insufficient data, the
// judge is disabled, or a query fails. 0.5 sits inside the default
// [floor, ceiling] band so the SLM request is unchanged.
const NeutralConfidence = 0.5

// Default confidence thresholds. When empirical local confidence for a
// category drops below the floor the SLM is biased toward frontier; above
// the ceiling it is biased toward local; inside the band the request is
// unchanged. Operators override via NEXUS_ROUTING_CONFIDENCE_FLOOR /
// NEXUS_ROUTING_CONFIDENCE_CEILING.
const (
	DefaultConfidenceFloor   = 0.4
	DefaultConfidenceCeiling = 0.85
)

// DefaultConfidenceMinSamples is the minimum number of judged outcomes a
// category needs before LocalConfidence reports anything other than
// NeutralConfidence. Below this the store returns 0.5 so routing is
// identical to today.
const DefaultConfidenceMinSamples = 5

// DefaultSuccessScore is the judge score (1..5) at or above which a local
// outcome is counted as "acceptable quality" when computing confidence.
// A 3 means "usable"; scores of 1..2 count as failures. This is a package
// constant rather than a config knob because the 1..5 scale is fixed by
// the judge's system prompt.
const DefaultSuccessScore = 3

// Category constants are the fixed set of task buckets Categorize emits.
// They mirror the DSL keyword spirit in dsl.go but cover the broader space
// the judge scores against.
const (
	CategoryCSS           = "css"
	CategoryRefactoring   = "refactoring"
	CategoryDebugging     = "debugging"
	CategoryArchitecture  = "architecture"
	CategoryBoilerplate   = "boilerplate"
	CategoryDocumentation = "documentation"
	CategoryOther         = "other"
)

// ConfidenceStore aggregates historical judge outcomes by task category and
// answers "for prompts like this, does the local model deliver acceptable
// quality?".
//
// Implementations must be safe for concurrent use: RecordOutcome is called
// from the judge worker pool while LocalConfidence is called from request
// goroutines.
type ConfidenceStore interface {
	// RecordOutcome persists one judged outcome. route is the route that
	// produced the scored output (only RouteLocal outcomes influence
	// LocalConfidence). judgeScore is the 1..5 judge rating; scores
	// outside that range are ignored.
	RecordOutcome(category string, route Route, judgeScore int)

	// LocalConfidence returns the fraction of recent local outcomes for
	// category that scored acceptably (0.0..1.0). It returns
	// NeutralConfidence (0.5) when there is insufficient data so the
	// caller's routing is unchanged.
	LocalConfidence(category string) float64
}

// categoryKeywords maps each category to the substrings that select it.
// Order matters: earlier entries win when a prompt matches several
// categories, so the list runs from most-specific/most-complex
// (architecture, debugging) to least (documentation) before falling
// through to "other". Keywords are matched against the ASCII-lowercased
// prompt using the same cheap contains helpers as dsl.go.
var categoryKeywords = []struct {
	category string
	keywords []string
}{
	{CategoryArchitecture, []string{
		"architecture", "architectural", "system design", "design pattern",
		"microservice", "scalability", "distributed system", "high-level design",
	}},
	{CategoryDebugging, []string{
		"debug", "bug", "stack trace", "stacktrace", "traceback", "exception",
		"panic", "segfault", "crash", "error message", "why does", "not working",
		"fails", "failing", "broken",
	}},
	{CategoryRefactoring, []string{
		"refactor", "restructure", "extract method", "rename", "clean up",
		"cleanup", "simplify", "deduplicate", "move method", "reorganize",
	}},
	{CategoryCSS, []string{
		"css", "tailwind", "flexbox", "stylesheet", "styling", "responsive",
		"media query", "padding", "margin", "layout", "scss", "sass",
	}},
	{CategoryBoilerplate, []string{
		"boilerplate", "scaffold", "template", "getter", "setter", "crud",
		"skeleton", "stub", "starter",
	}},
	{CategoryDocumentation, []string{
		"docstring", "documentation", "document ", "readme", "comment",
		"explain", "javadoc", "godoc", "changelog",
	}},
}

// Categorize buckets prompt into one of the fixed Category* constants using
// a lightweight keyword classifier. It never calls out to an embedding
// model — it is a pure, cheap function on the prompt text so it is safe to
// call on the request hot path. Unmatched prompts fall through to
// CategoryOther.
func Categorize(prompt string) string {
	lower := toLowerASCII(prompt)
	for _, group := range categoryKeywords {
		for _, kw := range group.keywords {
			if stringsContains(lower, kw) {
				return group.category
			}
		}
	}
	return CategoryOther
}
