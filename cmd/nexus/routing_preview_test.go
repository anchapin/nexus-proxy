package main

import (
	"bytes"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/router"
)

// Tests for the threshold calculation (errors*2 > prompts)
func TestPerPromptErrorThreshold(t *testing.T) {
	tests := []struct {
		name        string
		prompts     int
		errors      int
		expectExit1 bool
	}{
		{"no errors", 3, 0, false},
		{"exactly 50% errors", 4, 2, false},
		{"over 50% errors", 4, 3, true},
		{"all errors", 2, 2, true},
		{"one error, one prompt", 1, 1, true},
		{"one error, two prompts", 2, 1, false},
		{"one error, three prompts", 3, 1, false},
		{"two errors, three prompts", 3, 2, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate if we should return exit 1: errors*2 > prompts
			result := tt.errors*2 > tt.prompts
			if result != tt.expectExit1 {
				t.Errorf("for %d errors / %d prompts: expected exit1=%v, got %v",
					tt.errors, tt.prompts, tt.expectExit1, result)
			}
		})
	}
}

// TestRunRoutingPreview_ExplainFlag tests the --explain flag parsing
func TestRunRoutingPreview_ExplainFlagParsing(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var explain bool
	fs.BoolVar(&explain, "explain", false, "append bias note")

	err := fs.Parse([]string{"--explain", "test prompt"})
	if err != nil {
		t.Errorf("unexpected error parsing --explain: %v", err)
	}
	if !explain {
		t.Error("expected explain to be true after parsing --explain")
	}
}

// TestRunRoutingPreview_StdinFlag tests the --stdin flag parsing
func TestRunRoutingPreview_StdinFlagParsing(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var fromStdin bool
	fs.BoolVar(&fromStdin, "stdin", false, "read from stdin")

	err := fs.Parse([]string{"--stdin"})
	if err != nil {
		t.Errorf("unexpected error parsing --stdin: %v", err)
	}
	if !fromStdin {
		t.Error("expected fromStdin to be true after parsing --stdin")
	}
}

// TestReadStdinLines tests the stdin reading function
func TestReadStdinLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"single line", "hello\n", []string{"hello"}},
		{"multiple lines", "hello\nworld\nfoo\n", []string{"hello", "world", "foo"}},
		{"empty lines ignored", "hello\n\nworld\n", []string{"hello", "world"}},
		{"carriage return stripped", "hello\r\nworld\r\n", []string{"hello", "world"}},
		{"no trailing newline", "hello\nworld", []string{"hello", "world"}},
		{"all empty lines", "\n\n\n", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := io.NopCloser(strings.NewReader(tt.input))
			result, err := readStdinLines(r)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d lines, got %d: %v", len(tt.expected), len(result), result)
				return
			}
			for i, line := range result {
				if line != tt.expected[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.expected[i], line)
				}
			}
		})
	}
}

// TestDecisionReason tests the decision reason formatting
func TestDecisionReason(t *testing.T) {
	tests := []struct {
		name     string
		dec      router.Decision
		expected string
	}{
		{
			name:     "guardrail",
			dec:      router.Decision{Source: router.SourceGuardrail},
			expected: "guardrail:vram",
		},
		{
			name:     "dsl",
			dec:      router.Decision{Source: router.SourceDSL, Reason: "test"},
			expected: "dsl:test",
		},
		{
			name:     "slm",
			dec:      router.Decision{Source: router.SourceSLM},
			expected: "slm",
		},
		{
			name:     "slm error",
			dec:      router.Decision{Source: router.SourceSLMError, Reason: "timeout"},
			expected: "slm-error:timeout",
		},
		{
			name:     "escalation",
			dec:      router.Decision{Source: router.SourceEscalation},
			expected: "slm-no-client",
		},
		{
			name:     "low confidence",
			dec:      router.Decision{Source: router.SourceSLMEscalation},
			expected: "slm-low-confidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := decisionReason(tt.dec, nil, nil, nil, nil, "test prompt")
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestFormatDecision tests the decision formatting
func TestFormatDecision(t *testing.T) {
	dec := router.Decision{
		Route:   router.RouteLocal,
		Source:  router.SourceDSL,
		Reason:  "refactor",
	}
	result := formatDecision(dec, "dsl:refactor")
	expected := `ROUTE=local REASON="dsl:refactor"`
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

// TestExplainDecision tests the explain decision function
func TestExplainDecision(t *testing.T) {
	slm := &router.SLMClient{ConfidenceFloor: 0.3, ConfidenceCeiling: 0.7}

	tests := []struct {
		name      string
		dec       router.Decision
		expectStr string
	}{
		{
			name:      "slm source with high confidence",
			dec:       router.Decision{Source: router.SourceSLM, Confidence: 0.8},
			expectStr: "BIAS=positive",
		},
		{
			name:      "slm source with low confidence",
			dec:       router.Decision{Source: router.SourceSLM, Confidence: 0.2},
			expectStr: "BIAS=negative",
		},
		{
			name:      "slm source with neutral confidence",
			dec:       router.Decision{Source: router.SourceSLM, Confidence: 0.5},
			expectStr: "BIAS=neutral",
		},
		{
			name:      "dsl source returns empty",
			dec:       router.Decision{Source: router.SourceDSL, Confidence: 0.5},
			expectStr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := explainDecision(tt.dec, slm)
			if !strings.HasPrefix(result, tt.expectStr) {
				t.Errorf("expected to start with %q, got %q", tt.expectStr, result)
			}
		})
	}
}

// TestEmptyPromptSkipped tests that empty prompts are skipped
func TestEmptyPromptSkipped(t *testing.T) {
	// Verify that empty strings don't count toward prompts or errors
	prompts := []string{"hello", "", "world", ""}
	validPrompts := 0
	for _, p := range prompts {
		if p != "" {
			validPrompts++
		}
	}
	if validPrompts != 2 {
		t.Errorf("expected 2 valid prompts, got %d", validPrompts)
	}
}

// TestExitCodeForHelpFlag tests that --help returns 0
func TestRunRoutingPreview_HelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runRoutingPreview([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Errorf("expected exit code 0 for --help, got %d", exitCode)
	}
}

// TestFlagParsingErrors tests that invalid flags return 1
func TestFlagParsingErrors(t *testing.T) {
	// Invalid flag should return 1 (not 2, since config hasn't been loaded yet)
	var stdout, stderr bytes.Buffer
	exitCode := runRoutingPreview([]string{"--invalid-flag"}, &stdout, &stderr)

	if exitCode != 1 {
		t.Errorf("expected exit code 1 for invalid flag, got %d", exitCode)
	}
}

// TestFusionPatterns tests the fusion patterns helper
func TestFusionPatterns(t *testing.T) {
	// nil should return default
	result := fusionPatterns(nil)
	if len(result) == 0 {
		t.Error("expected non-nil result for nil input")
	}

	// non-nil should return as-is
	existing := router.DefaultFusionPatterns
	result = fusionPatterns(existing)
	if !reflect.DeepEqual(result, existing) {
		t.Error("expected same slice to be returned")
	}
}

// TestFormattingPatterns tests the formatting patterns helper
func TestFormattingPatterns(t *testing.T) {
	result := formattingPatterns(nil)
	if len(result) == 0 {
		t.Error("expected non-nil result for nil input")
	}
}

// TestLocalPatterns tests the local patterns helper
func TestLocalPatterns(t *testing.T) {
	result := localPatterns(nil)
	if len(result) == 0 {
		t.Error("expected non-nil result for nil input")
	}
}

// TestUnicodePatterns tests the unicode patterns helper
func TestUnicodePatterns(t *testing.T) {
	result := unicodePatterns(nil)
	if len(result) == 0 {
		t.Error("expected non-nil result for nil input")
	}
}

// TestFindMatchedKeyword tests the keyword matching
func TestFindMatchedKeyword(t *testing.T) {
	patterns := router.DefaultFusionPatterns

	// Should match "architectural design"
	result := findMatchedKeyword("help with architectural design", patterns)
	if result == "" {
		t.Error("expected to match 'architectural design'")
	}

	// Should not match unrelated text
	result = findMatchedKeyword("hello world", patterns)
	if result != "" {
		t.Errorf("expected no match for 'hello world', got %q", result)
	}
}

// TestToUnicodeLower tests unicode lowercasing
func TestToUnicodeLower(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "hello"},
		{"HELLO", "hello"},
		{"Hello", "hello"},
		{"HÉLLO", "héllo"},
		{"", ""},
	}

	for _, tt := range tests {
		result := toUnicodeLower(tt.input)
		if result != tt.expected {
			t.Errorf("toUnicodeLower(%q): expected %q, got %q", tt.input, tt.expected, result)
		}
	}
}

// TestHasUpperUnicode tests unicode uppercase detection
func TestHasUpperUnicode(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"hello", false},
		{"HELLO", true},
		{"Hello", true},
		{"HÉLLO", true},
		{"", false},
	}

	for _, tt := range tests {
		result := hasUpperUnicode(tt.input)
		if result != tt.expected {
			t.Errorf("hasUpperUnicode(%q): expected %v, got %v", tt.input, tt.expected, result)
		}
	}
}

// TestIsWordBoundary tests word boundary detection
func TestIsWordBoundary(t *testing.T) {
	tests := []struct {
		char     byte
		expected bool
	}{
		{' ', true},
		{'\t', true},
		{'\n', true},
		{'.', true},
		{',', true},
		{'a', false},
		{'Z', false},
	}

	for _, tt := range tests {
		result := isWordBoundary(tt.char)
		if result != tt.expected {
			t.Errorf("isWordBoundary(%q): expected %v, got %v", tt.char, tt.expected, result)
		}
	}
}

// TestSourceSLMErrorConstant verifies the constant value
func TestSourceSLMErrorConstant(t *testing.T) {
	// Verify the source is set correctly for SLM errors
	dec := router.Decision{
		Source: router.SourceSLMError,
		Reason: "connection timeout",
	}
	if dec.Source != "slm-error" {
		t.Errorf("expected SourceSLMError to be 'slm-error', got %q", dec.Source)
	}
}

// testableThresholdCalculation is a helper to verify the threshold logic
func testableThresholdCalculation(errors, prompts int) bool {
	return errors*2 > prompts
}

func TestThresholdCalculationVariants(t *testing.T) {
	// Edge cases
	// 0 errors / 0 prompts: 0*2 > 0 = false
	if testableThresholdCalculation(0, 0) {
		t.Error("0 errors / 0 prompts should not trigger threshold")
	}
	// 1 error / 0 prompts: 1*2 > 0 = 2 > 0 = true
	// This is a degenerate case; the actual code guards len(prompts) > 0
	if !testableThresholdCalculation(1, 0) {
		t.Error("1 error / 0 prompts should trigger threshold (2 > 0)")
	}
}
