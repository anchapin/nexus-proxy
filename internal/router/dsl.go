// Package router decides where a request should be executed: local Ollama,
// frontier API, or both (fusion). Routing is two-tier: a cheap regex DSL
// fast-pass that handles obvious cases, and an SLM fallback that asks a
// small local model to judge complexity when the DSL has no opinion.
package router

import (
	"regexp"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// Route names are the canonical string identifiers used across packages.
const (
	RouteLocal    = "local"
	RouteFrontier = "frontier"
	RouteFusion   = "fusion"
)

// Guardrail returns RouteFrontier when the prompt is too large for the
// configured VRAM budget. It uses tiktoken/cl100k_base token counting
// (issue #231) so estimates are within 15%% of actual counts. The threshold
// is the maximum *estimated* token count the local model can safely handle.
// When maxTokens <= 0 the guardrail is disabled and ("", false) is returned.
func Guardrail(prompt string, maxTokens int) (Route, bool) {
	if maxTokens <= 0 {
		return "", false
	}
	if telemetry.EstimateTokens(prompt) > maxTokens {
		return RouteFrontier, true
	}
	return "", false
}

// Route is a string alias for the routing decision. Use the Route* constants
// rather than raw strings so typos surface at compile time.
type Route string

// DSL runs the heuristic fast-pass. Returns one of RouteLocal, RouteFusion,
// or "" if no rule matched (caller should fall back to the SLM).
//
// formattingRegex matches simple formatting keywords (css, format, docstring,
// lint, typo, boilerplate). localPatternsRegex matches common coding task
// keywords (refactor, security scan, generate tests, explain this code,
// performance analysis, etc.). Both may be nil.
func DSL(prompt string, formattingRegex, localPatternsRegex *regexp.Regexp) (Route, bool) {
	lower := toLowerASCII(prompt)

	if stringsContains(lower, "architectural design") ||
		stringsContains(lower, "system architecture") {
		return RouteFusion, true
	}
	if formattingRegex != nil && formattingRegex.MatchString(lower) {
		return RouteLocal, true
	}
	if localPatternsRegex != nil && localPatternsRegex.MatchString(lower) {
		return RouteLocal, true
	}
	return "", false
}

// toLowerASCII lowercases ASCII letters only. The DSL rules are
// ASCII-keyword matches; full Unicode lowercasing is unnecessary and
// would force an allocation proportional to prompt length.
func toLowerASCII(s string) string {
	if !hasUpperASCII(s) {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func hasUpperASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

func stringsContains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	n, m := len(s), len(substr)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == substr {
			return i
		}
	}
	return -1
}
