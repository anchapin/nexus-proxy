// Package router decides where a request should be executed: local Ollama,
// frontier API, or both (fusion). Routing is two-tier: a cheap regex DSL
// fast-pass that handles obvious cases, and an SLM fallback that asks a
// small local model to judge complexity when the DSL has no opinion.
package router

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// Route names are the canonical string identifiers used across packages.
const (
	RouteLocal    = "local"
	RouteFrontier = "frontier"
	RouteFusion   = "fusion"
)

// Default DSL patterns. These match the hardcoded behaviour prior to issue #305.
// Exported so the chat handler can fall back to them when the config fields
// are nil (e.g. in tests that construct config.Config directly).
var (
	DefaultFormattingPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate|debug|fix bug|git commit|sql query|parse json|validate input|regex|api endpoint|test|optimize|readme)\b`),
	}
	DefaultFusionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(architectural design|system architecture)\b`),
	}
	DefaultLocalPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis)\b`),
	}
	// DefaultUnicodePatterns matches non-ASCII text categories (issue #422).
	// Operators can override via NEXUS_DSL_UNICODE_PATTERNS.
	DefaultUnicodePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\p{Han}`),   // Chinese characters
		regexp.MustCompile(`(?i)\p{Arabic}`), // Arabic characters
	}
)

// Guardrail returns RouteFrontier when the prompt is too large for the
// configured VRAM budget. The threshold is the maximum *estimated* token
// count the local model can safely handle. When maxTokens <= 0 the
// guardrail is disabled and ("", false) is returned.
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
// fusionPatterns matches architecture keywords that warrant fusion (both
// local and frontier). formattingPatterns matches simple formatting keywords
// (css, format, docstring, lint, typo, boilerplate). localPatterns matches
// common coding task keywords (refactor, security scan, generate tests,
// explain this code, performance analysis, etc.). unicodePatterns matches
// non-ASCII text categories (e.g. Chinese characters via \p{Han}) via
// NEXUS_DSL_UNICODE_PATTERNS (issue #422). Each pattern slice may be
// nil or empty in which case that branch is skipped.
func DSL(prompt string, fusionPatterns, formattingPatterns, localPatterns, unicodePatterns []*regexp.Regexp) (Route, bool) {
	lower := toUnicodeLower(prompt)

	if len(fusionPatterns) > 0 {
		for _, re := range fusionPatterns {
			if re.MatchString(lower) {
				return RouteFusion, true
			}
		}
	}
	if len(formattingPatterns) > 0 {
		for _, re := range formattingPatterns {
			if re.MatchString(lower) {
				return RouteLocal, true
			}
		}
	}
	if len(localPatterns) > 0 {
		for _, re := range localPatterns {
			if re.MatchString(lower) {
				return RouteLocal, true
			}
		}
	}
	// Unicode patterns match non-ASCII text directly (issue #422).
	// These patterns are NOT lowercased because they target script
	// categories (e.g. \p{Han}) rather than ASCII keywords.
	if len(unicodePatterns) > 0 {
		for _, re := range unicodePatterns {
			if re.MatchString(prompt) {
				return RouteLocal, true
			}
		}
	}
	return "", false
}

// toUnicodeLower converts s to lowercase using Unicode case-folding rules
// (issue #422). Unlike the prior toLowerASCII, this handles all scripts
// (Chinese, Arabic, Greek, etc.). The allocation is proportional to the
// number of uppercase runes in s; prompts without uppercase return s
// unchanged (zero allocation).
func toUnicodeLower(s string) string {
	if !hasUpperUnicode(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// hasUpperUnicode returns true if s contains any uppercase Unicode rune.
// Used to skip the toUnicodeLower allocation for already-lowercase strings.
func hasUpperUnicode(s string) bool {
	for _, r := range s {
		if r != unicode.ToLower(r) {
			return true
		}
	}
	return false
}

// toLowerASCII lowercases ASCII letters only. Kept for backward compatibility
// with confidence.go. Use toUnicodeLower for Unicode-aware lowercasing.
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
