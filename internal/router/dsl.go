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
		regexp.MustCompile(`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis|code review|review code|review pr|pull request review)\b`),
		regexp.MustCompile(`(?i)\b(migrate|migration|db migration|package migration)\b`),
		regexp.MustCompile(`(?i)\b(write sql|design schema|database schema)\b`),
		regexp.MustCompile(`(?i)\b(docker|containerize|deploy|kubernetes|k8s)\b`),
		regexp.MustCompile(`(?i)\b(git rebase|git merge|resolve conflicts|cherry-pick)\b`),
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
// explain this code, code review, migrations, SQL/DB, docker, git, etc.).
// Each pattern slice may be nil or empty in which case that branch is skipped.
func DSL(prompt string, fusionPatterns, formattingPatterns, localPatterns []*regexp.Regexp) (Route, bool) {
	lower := toLowerASCII(prompt)

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
