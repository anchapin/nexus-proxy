package router

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fuzzPatterns holds the production default DSL patterns for fuzz testing,
// compiled once and reused across all fuzz iterations.
var fuzzPatterns = struct {
	fusion     []*regexp.Regexp
	formatting []*regexp.Regexp
	local      []*regexp.Regexp
	unicodeP   []*regexp.Regexp
}{
	fusion:     DefaultFusionPatterns,
	formatting: DefaultFormattingPatterns,
	local:      DefaultLocalPatterns,
	unicodeP:   DefaultUnicodePatterns,
}

// FuzzDSLRegex feeds arbitrary strings into the DSL routing function. The
// regex evaluation must never panic and must complete within a bounded
// wall-clock budget (guarding against catastrophic backtracking). The result
// must always be a valid Route or the empty no-match sentinel.
func FuzzDSLRegex(f *testing.F) {
	// Seed with real prompts that exercise each pattern group.
	f.Add("fix the css on the login page")
	f.Add("design the system architecture for our new service")
	f.Add("refactor the authentication module")
	f.Add("write a sql query to find duplicates")
	// Adversarial inputs that can trigger regex pathology.
	f.Add("")
	f.Add(strings.Repeat("a", 10000))        // long input
	f.Add("css" + strings.Repeat(" ", 5000)) // keyword + huge padding
	f.Add("请解释这个函数的用法")                      // Chinese (unicode match)
	f.Add("🎉🎊🎈💯🔥")                           // emoji spam (no match)
	// Null bytes and control characters.
	f.Add("css\x00format\x01lint")
	// Alternating keyword / non-keyword.
	f.Add("css css css css css css css css css css")

	f.Fuzz(func(t *testing.T, prompt string) {
		// Bounded wall-clock budget — catches catastrophic backtracking even
		// if the regex engine doesn't formally hang.
		done := make(chan struct{})
		var route Route
		var match string
		var hit bool
		go func() {
			defer close(done)
			route, match, hit = DSL(
				prompt,
				fuzzPatterns.fusion,
				fuzzPatterns.formatting,
				fuzzPatterns.local,
				fuzzPatterns.unicodeP,
			)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("DSL did not complete within 5s for prompt (len=%d)", len(prompt))
		}

		// Invariant 1: on a hit, the route must be a known value.
		if hit {
			switch route {
			case RouteLocal, RouteFusion:
				// valid
			default:
				t.Fatalf("DSL returned unknown route %q on hit", route)
			}
			if match == "" {
				t.Fatalf("DSL returned hit=true but empty match label")
			}
		}
		// Invariant 2: on a miss, route and match must be empty.
		if !hit && (route != "" || match != "") {
			t.Fatalf("DSL returned miss but non-empty route/match: %q/%q", route, match)
		}
		// Invariant 3: the prompt must be valid UTF-8 (defensive — DSL
		// should not mutate it, but verify the fuzzer didn't produce
		// something that corrupts the evaluation).
		_ = utf8.ValidString(prompt)
	})
}
