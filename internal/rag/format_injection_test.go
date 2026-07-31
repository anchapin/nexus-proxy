package rag

import (
	"strings"
	"testing"
)

// TestFormatInjection verifies the exact output format of FormatInjection:
// the filename is interpolated into the header line, the content is wrapped
// in a fenced triple-backtick block, and the result ends with the standard
// instruction sentence. This is the format contract the rest of the proxy
// and downstream models depend on, so any drift must surface here first.
func TestFormatInjection(t *testing.T) {
	ex := &FewShotExample{
		Filename: "internal/router/guardrail.go",
		Content:  "package router\n\nfunc Guardrail(s string) bool { return len(s) > 0 }",
	}
	got := FormatInjection(ex)

	// 1. Header is present and interpolates the filename.
	header := "[PROXY RETRIEVAL CONTEXT]: Here is a highly relevant, validated few-shot example from the local codebase (" + ex.Filename + "):"
	if !strings.HasPrefix(got, "\n\n"+header+"\n") {
		t.Errorf("header line not found or filename not interpolated.\nwant prefix: %q\ngot prefix:         %q",
			"\n\n"+header+"\n", truncateForLog(got, len(header)+10))
	}

	// 2. The content is wrapped in a single fenced triple-backtick block.
	fence := "```"
	fenceCount := strings.Count(got, fence)
	if fenceCount != 2 {
		t.Fatalf("expected exactly 2 triple-backtick fences, got %d (output=%q)", fenceCount, got)
	}
	wantContentBlock := fence + "\n" + ex.Content + "\n" + fence
	if !strings.Contains(got, wantContentBlock) {
		t.Errorf("content block not wrapped correctly.\nwant substring: %q\ngot:             %q",
			wantContentBlock, got)
	}

	// 3. Ends with the instruction sentence (no trailing whitespace).
	wantSuffix := "Analyze its architecture and apply its patterns if relevant to this task."
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("output does not end with instruction sentence.\nwant suffix: %q\ngot suffix:  %q",
			wantSuffix, truncateForLog(reverse(got), len(wantSuffix)))
	}

	// 4. The instruction sentence is followed by the closing fence and a
	//    single newline — verify the structural ordering: header → fence
	//    → content → fence → instruction.
	wantFull := "\n\n" + header + "\n" + wantContentBlock + "\n" + wantSuffix
	if got != wantFull {
		t.Errorf("output does not match exact format contract.\nwant: %q\ngot:  %q", wantFull, got)
	}
}

// TestFormatInjectionEmptyContent verifies that FormatInjection handles an
// empty (or effectively nil) Content field without crashing and still
// produces the structural format contract: header, fences, instruction.
func TestFormatInjectionEmptyContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty string", ""},
		{"single newline", "\n"},
		{"only whitespace", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &FewShotExample{
				Filename: "empty.go",
				Content:  tc.content,
			}
			// Must not panic on empty content.
			got := FormatInjection(ex)

			// Filename still interpolated.
			if !strings.Contains(got, ex.Filename) {
				t.Errorf("filename %q not present in output: %q", ex.Filename, got)
			}
			// Two fences still present (the block structure is intact).
			if c := strings.Count(got, "```"); c != 2 {
				t.Errorf("expected 2 fences, got %d", c)
			}
			// Instruction sentence still terminates the block.
			if !strings.HasSuffix(got, "Analyze its architecture and apply its patterns if relevant to this task.") {
				t.Errorf("instruction sentence missing; got: %q", got)
			}
		})
	}
}

// TestFormatInjectionVeryLongFilename verifies FormatInjection does not crash
// on hostile or unusual filenames (spaces, Unicode, shell metacharacters,
// backticks, percent signs) and treats them as literal text — no shell
// interpolation, no %-format expansion, no truncation.
func TestFormatInjectionVeryLongFilename(t *testing.T) {
	cases := []struct {
		name     string
		filename string
	}{
		{"spaces", "my dir/my file.go"},
		{"unicode", "中文/路径/文件.go"},
		{"emoji", "pkg/🚀/launch.go"},
		{"shell metachars", "pkg/$(whoami).go"},
		{"backticks", "pkg/`echo hi`.go"},
		{"percent signs", "pkg/%s/%d.go"},
		{"very long", strings.Repeat("a/", 200) + "deep.go"},
		{"null byte adjacent", "pkg/x\x00adjacent.go"},
		{"newlines in name", "pkg/foo\nevil.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &FewShotExample{
				Filename: tc.filename,
				Content:  "// payload",
			}
			// Must not panic.
			got := FormatInjection(ex)

			// Filename appears verbatim — no shell/% expansion.
			if !strings.Contains(got, tc.filename) {
				t.Errorf("filename not interpolated verbatim.\nwant substring: %q\ngot: %q",
					tc.filename, got)
			}

			// Sanity: structural contract still holds.
			if c := strings.Count(got, "```"); c != 2 {
				t.Errorf("expected 2 fences, got %d (output=%q)", c, got)
			}
			if !strings.HasSuffix(got, "Analyze its architecture and apply its patterns if relevant to this task.") {
				t.Errorf("instruction sentence missing; got suffix: %q",
					truncateForLog(reverse(got), 80))
			}
		})
	}
}

// truncateForLog returns s truncated to at most n runes with an ellipsis.
func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// reverse returns its input string reversed rune-wise.
func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
