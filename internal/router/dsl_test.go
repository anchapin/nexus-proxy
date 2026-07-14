package router

import (
	"regexp"
	"strings"
	"testing"
)

func TestGuardrail(t *testing.T) {
	cases := []struct {
		name      string
		prompt    string
		maxTokens int
		want      Route
		wantHit   bool
	}{
		{"small prompt", "hello world", 6000, "", false},
		// tiktoken BPE compresses repeated 'a's: 48000 'a' chars = 6000 tokens.
		{"exactly at limit", strings.Repeat("a", 48000), 6000, "", false},
		// 49000 'a' chars = 6125 tokens > 6000 budget.
		{"over limit", strings.Repeat("a", 49000), 6000, RouteFrontier, true},
		{"zero maxTokens means no guardrail", "anything", 0, "", false},
		{"negative maxTokens means no guardrail", "anything", -1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := Guardrail(tc.prompt, tc.maxTokens)
			if got != tc.want || hit != tc.wantHit {
				t.Errorf("Guardrail(%q,%d) = (%q,%v), want (%q,%v)",
					tc.prompt, tc.maxTokens, got, hit, tc.want, tc.wantHit)
			}
		})
	}
}

func TestDSL(t *testing.T) {
	formattingRegex := regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`)
	localPatternsRegex := regexp.MustCompile(`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis)\b`)
	cases := []struct {
		name    string
		prompt  string
		want    Route
		wantHit bool
	}{
		{"formatting hit", "fix the css", RouteLocal, true},
		{"formatting uppercase", "REWRITE THE DOCSTRING", RouteLocal, true},
		{"boilerplate hit", "generate boilerplate", RouteLocal, true},
		{"format substring inside larger word should NOT match (word boundary)",
			"reformation needed", "", false},
		{"architecture fusion", "design the system architecture for us", RouteFusion, true},
		{"architectural design fusion", "make an architectural design", RouteFusion, true},
		{"refactor keyword local (issue #202)", "refactor this module", RouteLocal, true},
		{"security scan keyword local (issue #202)", "run a security scan", RouteLocal, true},
		{"generate tests keyword local (issue #202)", "generate tests for this file", RouteLocal, true},
		{"explain this code keyword local (issue #202)", "explain this code", RouteLocal, true},
		{"performance analysis keyword local (issue #202)", "run a performance analysis", RouteLocal, true},
		{"security scan uppercase local (issue #202)", "RUN SECURITY SCAN", RouteLocal, true},
		{"refactor substring inside larger word should NOT match (issue #202)",
			"refactoring is needed", "", false},
		{"unrelated", "explain goroutines", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := DSL(tc.prompt, formattingRegex, localPatternsRegex)
			if got != tc.want || hit != tc.wantHit {
				t.Errorf("DSL(%q) = (%q,%v), want (%q,%v)",
					tc.prompt, got, hit, tc.want, tc.wantHit)
			}
		})
	}
}

func TestToLowerASCII(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"abc":       "abc",
		"ABC":       "abc",
		"Hello, 世界": "hello, 世界",
		"  MIX  ":   "  mix  ",
	}
	for in, want := range cases {
		if got := toLowerASCII(in); got != want {
			t.Errorf("toLowerASCII(%q) = %q, want %q", in, got, want)
		}
	}
}
