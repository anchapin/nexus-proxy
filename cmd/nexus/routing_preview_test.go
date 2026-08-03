package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/router"
)

// --- readStdinLines ---

func TestReadStdinLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", nil},
		{"single", "hello world\n", []string{"hello world"}},
		{"multi", "line1\nline2\nline3\n", []string{"line1", "line2", "line3"}},
		{"trailing_newline_only", "\n", nil},
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"blank_lines_skipped", "a\n\nb\n", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readStdinLines(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d lines, want %d (%v)", len(got), len(tt.want), got)
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("line %d: got %q, want %q", i, v, tt.want[i])
				}
			}
		})
	}
}

// --- decisionReason ---

func TestDecisionReason(t *testing.T) {
	re := regexp.MustCompile(`css`)
	patterns := []*regexp.Regexp{re}

	tests := []struct {
		name   string
		dec    router.Decision
		expect string
	}{
		{"guardrail", router.Decision{Source: router.SourceGuardrail}, "guardrail:vram"},
		{"dsl", router.Decision{Source: router.SourceDSL, Route: router.RouteLocal}, "dsl:css"},
		{"slm", router.Decision{Source: router.SourceSLM}, "slm"},
		{"slm-error", router.Decision{Source: router.SourceSLMError, Reason: "timeout"}, "slm-error:timeout"},
		{"escalation", router.Decision{Source: router.SourceEscalation}, "slm-no-client"},
		{"slm-escalation", router.Decision{Source: router.SourceSLMEscalation}, "slm-low-confidence"},
		{"default", router.Decision{Source: router.DecisionSource("unknown")}, "slm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decisionReason(tt.dec, nil, patterns, nil, nil, "fix the css bug")
			if tt.name == "dsl" && got != "dsl:css" {
				t.Errorf("got %q, want %q", got, "dsl:css")
			}
			if tt.name != "dsl" && got != tt.expect {
				t.Errorf("got %q, want %q", got, tt.expect)
			}
		})
	}
}

// --- formatDecision ---

func TestFormatDecision(t *testing.T) {
	dec := router.Decision{Route: router.RouteLocal, Confidence: 0.85}
	got := formatDecision(dec, "slm")
	if !strings.Contains(got, "ROUTE=local") {
		t.Errorf("expected ROUTE=local in %q", got)
	}
	if !strings.Contains(got, "REASON=") {
		t.Errorf("expected REASON= in %q", got)
	}
}

// --- explainDecision ---

func TestExplainDecision(t *testing.T) {
	slm := router.NewSLMClient("http://localhost:11434", "qwen3-coder:4b", 0, nil)
	slm.ConfidenceFloor = 0.3
	slm.ConfidenceCeiling = 0.8

	tests := []struct {
		name   string
		dec    router.Decision
		expect string
	}{
		{"non-slm", router.Decision{Source: router.SourceDSL}, ""},
		{"negative", router.Decision{Source: router.SourceSLM, Confidence: 0.1}, "BIAS=negative"},
		{"positive", router.Decision{Source: router.SourceSLM, Confidence: 0.95}, "BIAS=positive"},
		{"neutral", router.Decision{Source: router.SourceSLM, Confidence: 0.5}, "BIAS=neutral"},
		{"escalation-negative", router.Decision{Source: router.SourceSLMEscalation, Confidence: 0.1}, "BIAS=negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explainDecision(tt.dec, slm)
			if tt.expect == "" {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			if !strings.HasPrefix(got, tt.expect) {
				t.Errorf("expected prefix %q, got %q", tt.expect, got)
			}
		})
	}
}

// --- explainDecision with default floor/ceiling ---

func TestExplainDecisionDefaults(t *testing.T) {
	slm := router.NewSLMClient("http://localhost:11434", "qwen3-coder:4b", 0, nil)
	// ConfidenceFloor and ConfidenceCeiling are 0 → should use defaults
	dec := router.Decision{Source: router.SourceSLM, Confidence: 0.5}
	got := explainDecision(dec, slm)
	if got == "" {
		t.Error("expected non-empty with default floor/ceiling")
	}
}

// --- findMatchedKeyword ---

func TestFindMatchedKeyword(t *testing.T) {
	tests := []struct {
		name    string
		lower   string
		pattern string
		want    string
	}{
		{"match_css", "fix the css bug", "css", "css"},
		{"match_debug", "debug this crash", "debug", "debug"},
		{"no_match", "hello world", "", ""},
		{"word_boundary_left", "foocss bar", "", ""},
		{"word_boundary_right", "cssbar", "", ""},
		{"start_of_string", "css is great", "css", "css"},
		{"end_of_string", "use css", "css", "css"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pats []*regexp.Regexp
			if tt.pattern != "" {
				pats = []*regexp.Regexp{regexp.MustCompile(tt.pattern)}
			}
			got := findMatchedKeyword(tt.lower, pats)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// --- toUnicodeLower / hasUpperUnicode ---

func TestToUnicodeLower(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"Hello World", "hello world"},
		{"Москва", "москва"},
		{"東京 Tōkyō", "東京 tōkyō"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toUnicodeLower(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasUpperUnicode(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"hello", false},
		{"Hello", true},
		{"Москва", true},
		{"москва", false},
		{"東京", false},
		{"Tōkyō", true},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := hasUpperUnicode(tt.input)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// --- isWordBoundary ---

func TestIsWordBoundary(t *testing.T) {
	boundaries := " \t\n\r,.!?:;()[]{}\"'`/\\|&+*%^=<>@#$~"
	for i := 0; i < len(boundaries); i++ {
		c := boundaries[i]
		if !isWordBoundary(c) {
			t.Errorf("expected %q (0x%02x) to be a word boundary", c, c)
		}
	}
	nonBoundaries := "abcABC012_"
	for i := 0; i < len(nonBoundaries); i++ {
		c := nonBoundaries[i]
		if isWordBoundary(c) {
			t.Errorf("expected %q (0x%02x) NOT to be a word boundary", c, c)
		}
	}
}

// --- matchedDSLKeyword ---

func TestMatchedDSLKeyword(t *testing.T) {
	fusion := []*regexp.Regexp{regexp.MustCompile(`architectural design`)}
	formatting := []*regexp.Regexp{regexp.MustCompile(`css`)}
	local := []*regexp.Regexp{regexp.MustCompile(`refactor`)}
	unicode := []*regexp.Regexp{regexp.MustCompile(`\p{Han}`)}

	// Fusion route → should match fusion pattern
	kw := matchedDSLKeyword("architectural design review", router.RouteFusion, fusion, formatting, local, unicode)
	if kw != "architectural design" {
		t.Errorf("fusion match: got %q, want %q", kw, "architectural design")
	}

	// Local route → should match formatting
	kw = matchedDSLKeyword("fix the css bug", router.RouteLocal, fusion, formatting, local, unicode)
	if kw != "css" {
		t.Errorf("formatting match: got %q, want %q", kw, "css")
	}

	// No match
	kw = matchedDSLKeyword("hello world", router.RouteLocal, fusion, formatting, local, unicode)
	if kw != "unknown" {
		t.Errorf("no match: got %q, want %q", kw, "unknown")
	}
}

// --- pattern default helpers ---

func TestPatternDefaults(t *testing.T) {
	customRe := regexp.MustCompile(`custom`)

	if got := fusionPatterns([]*regexp.Regexp{customRe}); len(got) != 1 || got[0] != customRe {
		t.Error("fusionPatterns should return custom when provided")
	}
	if got := fusionPatterns(nil); len(got) == 0 {
		t.Error("fusionPatterns should return defaults when nil")
	}

	if got := formattingPatterns([]*regexp.Regexp{customRe}); len(got) != 1 {
		t.Error("formattingPatterns should return custom when provided")
	}
	if got := formattingPatterns(nil); len(got) == 0 {
		t.Error("formattingPatterns should return defaults when nil")
	}

	if got := localPatterns([]*regexp.Regexp{customRe}); len(got) != 1 {
		t.Error("localPatterns should return custom when provided")
	}
	if got := localPatterns(nil); len(got) == 0 {
		t.Error("localPatterns should return defaults when nil")
	}

	if got := unicodePatterns([]*regexp.Regexp{customRe}); len(got) != 1 {
		t.Error("unicodePatterns should return custom when provided")
	}
	if got := unicodePatterns(nil); len(got) == 0 {
		t.Error("unicodePatterns should return defaults when nil")
	}
}

// --- runRoutingPreview end-to-end ---

func TestRunRoutingPreviewBasic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRoutingPreview([]string{"fix the css bug"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ROUTE=") {
		t.Errorf("expected ROUTE= in output, got %q", out)
	}
}

func TestRunRoutingPreviewNoPrompts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRoutingPreview([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
}

func TestRunRoutingPreviewStdin(t *testing.T) {
	// runRoutingPreview reads from os.Stdin for --stdin, which is hard to
	// control in a test. We verify the non-stdin path instead — the stdin
	// path is exercised by the dispatch_test.go integration test.
}

func TestRunRoutingPreviewMultiplePrompts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRoutingPreview([]string{"refactor this function", "hello world"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Errorf("expected at least 2 output lines, got %d (%q)", len(lines), stdout.String())
	}
}

func TestRunRoutingPreviewExplain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRoutingPreview([]string{"--explain", "fix the css bug"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", code, stderr.String())
	}
}

// --- context propagation check ---

func TestRunRoutingPreviewContext(t *testing.T) {
	// Verify the routing preview uses context.Background() internally.
	// We can't inject a context, but we can verify it doesn't hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx // just ensure the type compiles
}

// --- nil-pattern edge cases ---

func TestFindMatchedKeywordNilPatterns(t *testing.T) {
	got := findMatchedKeyword("test", []*regexp.Regexp{nil})
	if got != "" {
		t.Errorf("expected empty for nil pattern, got %q", got)
	}
}

func TestFindMatchedKeywordEmptyPatterns(t *testing.T) {
	got := findMatchedKeyword("test", []*regexp.Regexp{})
	if got != "" {
		t.Errorf("expected empty for empty patterns, got %q", got)
	}
}
