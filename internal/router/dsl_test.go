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
		{"exactly at limit", strings.Repeat("a", 24000), 6000, "", false},
		{"over limit", strings.Repeat("a", 24005), 6000, RouteFrontier, true},
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
	re := regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`)
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
		{"unrelated", "explain goroutines", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := DSL(tc.prompt, re)
			if got != tc.want || hit != tc.wantHit {
				t.Errorf("DSL(%q) = (%q,%v), want (%q,%v)",
					tc.prompt, got, hit, tc.want, tc.wantHit)
			}
		})
	}
}

func TestToLowerASCII(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"abc":      "abc",
		"ABC":      "abc",
		"Hello, 世界": "hello, 世界",
		"  MIX  ":   "  mix  ",
	}
	for in, want := range cases {
		if got := toLowerASCII(in); got != want {
			t.Errorf("toLowerASCII(%q) = %q, want %q", in, got, want)
		}
	}
}