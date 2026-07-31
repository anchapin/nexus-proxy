package router

import (
	"bytes"
	"os"
	"os/exec"
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
		// Inputs > maxAccurateEncodeLen (8192) use the len(s)/4 heuristic:
		// 24000 chars / 4 = 6000 tokens — exactly at the limit, NOT over it.
		{"exactly at limit", strings.Repeat("a", 24000), 6000, "", false},
		// 24004 chars / 4 = 6001 tokens > 6000 budget — just over the limit.
		{"over limit", strings.Repeat("a", 24004), 6000, RouteFrontier, true},
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
	// Fusion patterns (architecture keywords — issue #305)
	fusionPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(architectural design|system architecture)\b`),
	}
	// Formatting patterns (simple, non-logic tasks)
	formattingPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate|regex|api endpoint)\b`),
	}
	// Local patterns (common coding tasks — issue #230 additions merged with prior #202 entries)
	localPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis|debug|fix bug|git commit|sql query|parse json|validate input|test|optimize|readme)\b`),
	}
	// Unicode patterns (issue #422)
	unicodePatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\p{Han}`),    // Chinese characters
		regexp.MustCompile(`(?i)\p{Arabic}`), // Arabic characters
	}
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
		// New patterns from issue #230
		{"debug hit", "debug this memory leak", RouteLocal, true},
		{"fix bug hit", "fix bug in the authentication", RouteLocal, true},
		{"git commit hit", "git commit with a descriptive message", RouteLocal, true},
		{"sql query hit", "write a sql query to find duplicates", RouteLocal, true},
		{"parse json hit", "parse json response", RouteLocal, true},
		{"validate input hit", "validate input fields", RouteLocal, true},
		{"regex hit", "regex to match email addresses", RouteLocal, true},
		{"api endpoint hit", "create an api endpoint", RouteLocal, true},
		{"test hit", "write a test for this function", RouteLocal, true},
		{"optimize hit", "optimize the database queries", RouteLocal, true},
		{"readme hit", "update the readme file", RouteLocal, true},
		// ASCII regression (issue #422) — /* format this JSON */ still routes to local
		{"ascii format comment", " /* format this JSON */", RouteLocal, true},
		{"ascii refactor comment", " /* refactor this code */", RouteLocal, true},
		// Unicode: Chinese prompt (issue #422)
		{"chinese请解释这个函数的用法", "请解释这个函数的用法", RouteLocal, true},
		{"chinese explain mixed", "请 explain this code", RouteLocal, true},
		// Unicode: emoji-spam (no match, not local)
		{"emoji spam", "🎉🎊🎈💯🔥💯🎉💯", "", false},
		// Unicode: Arabic (issue #422)
		{"arabic explain", "اشرح هذا الكود", RouteLocal, true},
		{"arabic mixed ascii", "please refactor هذا الكود", RouteLocal, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, hit := DSL(tc.prompt, fusionPatterns, formattingPatterns, localPatterns, unicodePatterns)
			if got != tc.want || hit != tc.wantHit {
				t.Errorf("DSL(%q) = (%q,%v), want (%q,%v)",
					tc.prompt, got, hit, tc.want, tc.wantHit)
			}
		})
	}
}

func TestToUnicodeLower(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"abc":        "abc",
		"ABC":        "abc",
		"Hello, 世界":  "hello, 世界",
		"  MIX  ":    "  mix  ",
		"café":       "café",       // no-op: no uppercase
		"ΑΛΦΑ":       "αλφα",       // Greek uppercase (simplified case fold)
		"ΕΛΛΑΔΑ":     "ελλαδα",     // Greek uppercase (diacritics stripped by unicode.ToLower)
		"ΠΑΡΑΔΕΙΓΜΑ": "παραδειγμα", // Greek uppercase (diacritics stripped)
		"REFACTOR":   "refactor",   // ASCII uppercase
		"RÉFACTOR":   "réfactor",   // Latin-1 uppercase with accent
		"請解釋這個函數的用法": "請解釋這個函數的用法", // Chinese: no case, unchanged
		"🎉🎊":         "🎉🎊",         // emoji: no case, unchanged
	}
	for in, want := range cases {
		if got := toUnicodeLower(in); got != want {
			t.Errorf("toUnicodeLower(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCompileDefaultPattern verifies that an invalid default DSL pattern
// surfaces as a descriptive error rather than a panic (issue #588). The
// boot guard (mustCompileDefaultPattern) calls log.Fatalf on this error;
// here we exercise the testable core directly.
func TestCompileDefaultPattern(t *testing.T) {
	t.Run("valid pattern compiles", func(t *testing.T) {
		re, err := compileDefaultPattern("formatting", `(?i)\b(css|format)\b`)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if re == nil {
			t.Fatal("expected non-nil regexp")
		}
		if !re.MatchString("fix the css") {
			t.Error("compiled regexp should match")
		}
	})

	invalidCases := []struct {
		name string
		// group is the DSL pattern group name passed to compileDefaultPattern.
		group string
	}{
		{"formatting", "formatting"},
		{"fusion", "fusion"},
		{"local", "local"},
		{"unicode", "unicode"},
	}
	for _, tc := range invalidCases {
		t.Run("invalid "+tc.name+" pattern returns error without panic", func(t *testing.T) {
			re, err := compileDefaultPattern(tc.group, "[invalid")
			if err == nil {
				t.Fatal("expected error for invalid pattern, got nil")
			}
			if re != nil {
				t.Errorf("expected nil regexp on error, got %v", re)
			}
			msg := err.Error()
			// Error message must identify the specific pattern group.
			if !strings.Contains(msg, tc.group) {
				t.Errorf("error %q should identify pattern group %q", msg, tc.group)
			}
			// Error message must echo the offending expression so the
			// operator can locate the bad pattern.
			if !strings.Contains(msg, "[invalid") {
				t.Errorf("error %q should contain the invalid expression", msg)
			}
		})
	}
}

// TestDefaultPatternsCompiled verifies the package-level defaults are
// populated and functional after the move off regexp.MustCompile (issue #588).
// This is a regression guard: if a default silently failed to compile the
// slice would be empty and these assertions would catch it.
func TestDefaultPatternsCompiled(t *testing.T) {
	checks := []struct {
		name string
		re   []*regexp.Regexp
	}{
		{"DefaultFormattingPatterns", DefaultFormattingPatterns},
		{"DefaultFusionPatterns", DefaultFusionPatterns},
		{"DefaultLocalPatterns", DefaultLocalPatterns},
		{"DefaultUnicodePatterns", DefaultUnicodePatterns},
	}
	for _, c := range checks {
		if len(c.re) == 0 {
			t.Errorf("%s is empty; package init failed", c.name)
		}
		for i, re := range c.re {
			if re == nil {
				t.Errorf("%s[%d] is nil; package init failed", c.name, i)
			}
		}
	}
}

// TestMustCompileDefaultPatternFatal verifies the boot guard's runtime
// behaviour (issue #588): an invalid default pattern causes the process to
// exit non-zero with a descriptive logged message, NOT a panic. The test
// re-invokes its own binary with DSL_FATAL_PATTERN_GROUP set; the child
// triggers mustCompileDefaultPattern on an invalid expression.
func TestMustCompileDefaultPatternFatal(t *testing.T) {
	group := os.Getenv("DSL_FATAL_PATTERN_GROUP")
	if group != "" {
		// Child process: trigger the boot guard. log.Fatalf will os.Exit(1).
		_ = mustCompileDefaultPattern(group, "[invalid")
		return
	}

	groups := []string{"formatting", "fusion", "local", "unicode"}
	for _, g := range groups {
		t.Run("fatal "+g, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestMustCompileDefaultPatternFatal")
			cmd.Env = append(os.Environ(), "DSL_FATAL_PATTERN_GROUP="+g)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()

			// Must exit non-zero (log.Fatalf → os.Exit(1)).
			if err == nil {
				t.Fatal("expected non-zero exit from log.Fatalf, got nil")
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
			}
			if exitErr.Success() {
				t.Fatalf("expected non-zero exit code, got %v", exitErr)
			}

			msg := stderr.String()
			// The fatal log line must identify the offending pattern group.
			if !strings.Contains(msg, g) {
				t.Errorf("stderr %q should mention pattern group %q", msg, g)
			}
			// It must echo the offending expression.
			if !strings.Contains(msg, "[invalid") {
				t.Errorf("stderr %q should contain the invalid expression", msg)
			}
		})
	}
}

