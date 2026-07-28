// Package router decides where a request should be executed: local Ollama,
// frontier API, or both (fusion). Routing is two-tier: a cheap regex DSL
// fast-pass that handles obvious cases, and an SLM fallback that asks a
// small local model to judge complexity when the DSL has no opinion.
package router

import (
	"fmt"
	"log"
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
//
// The patterns are compiled via mustCompileDefaultPattern (issue #588) so that
// an invalid default surfaces as a structured boot error (log.Fatalf) rather
// than a runtime panic from regexp.MustCompile.
var (
	DefaultFormattingPatterns = []*regexp.Regexp{
		mustCompileDefaultPattern("formatting", `(?i)\b(css|format|docstring|lint|typo|boilerplate|debug|fix bug|git commit|sql query|parse json|validate input|regex|api endpoint|test|optimize|readme)\b`),
	}
	DefaultFusionPatterns = []*regexp.Regexp{
		mustCompileDefaultPattern("fusion", `(?i)\b(architectural design|system architecture)\b`),
	}
	DefaultLocalPatterns = []*regexp.Regexp{
		mustCompileDefaultPattern("local", `(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis)\b`),
	}
	// DefaultUnicodePatterns matches non-ASCII text categories (issue #422).
	// Operators can override via NEXUS_DSL_UNICODE_PATTERNS.
	DefaultUnicodePatterns = []*regexp.Regexp{
		mustCompileDefaultPattern("unicode", `(?i)\p{Han}`),    // Chinese characters
		mustCompileDefaultPattern("unicode", `(?i)\p{Arabic}`), // Arabic characters
	}
)

// compileDefaultPattern compiles a single default DSL regex expression and
// returns a descriptive error identifying the pattern group (formatting,
// fusion, local, or unicode) if the syntax is invalid. It never panics.
// This is the testable core of mustCompileDefaultPattern (issue #588).
func compileDefaultPattern(name, expr string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("dsl: invalid default %s pattern %q: %w", name, expr, err)
	}
	return re, nil
}

// mustCompileDefaultPattern compiles a default DSL regex expression and is
// intended for package-level var initialization. On invalid syntax it logs a
// descriptive message (identifying the pattern group and the parse error) and
// calls log.Fatalf, so the proxy exits with a clear boot error instead of a
// panic (issue #588).
func mustCompileDefaultPattern(name, expr string) *regexp.Regexp {
	re, err := compileDefaultPattern(name, expr)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return re
}

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
// fusionPatterns, formattingPatterns, and localPatterns are matched against
// the lowercase prompt (via toUnicodeLower) so that keywords like "REFACTOR"
// and "refactor" are treated identically. unicodePatterns is matched against
// the raw prompt because Unicode property escapes (\p{Han}, \p{Arabic}, etc.)
// are inherently case-invariant — lowercasing a Chinese or Arabic character
// is a no-op, and using the raw prompt avoids an unnecessary allocation.
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

// containsWord returns true if kw appears in s as a whole word/phrase,
// using \b word-boundary matching so that e.g. "test" does not match
// inside "contest". The keyword kw is already lowercased by the caller.
func containsWord(s, kw string) bool {
	if kw == "" {
		return true
	}
	// regexp.QuoteMeta escapes all regex metacharacters, then we wrap with \b.
	pattern := `(?i)\b` + regexp.QuoteMeta(kw) + `\b`
	matched, _ := regexp.MatchString(pattern, s)
	return matched
}
