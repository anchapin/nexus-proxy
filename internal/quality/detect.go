package quality

import (
	"regexp"
	"strings"
)

// editToolNames is the set of tool names the issue calls out
// explicitly (write_file / edit_file / apply_patch) plus the obvious
// close cousins (create_file / patch_file / update_file / write /
// edit / patch). Detection is deliberately liberal: the goal is to
// catch any tool call that might have produced a file write, and let
// project detection at verification time filter out false positives.
var editToolNames = []string{
	"write_file",
	"edit_file",
	"apply_patch",
	"create_file",
	"patch_file",
	"update_file",
	"write",
	"edit",
	"patch",
}

// editToolNameRe matches `"name":"<one of editToolNames>"` in plain
// JSON. The path field, when nested inside `function.arguments`,
// carries escaped quotes — see cleanWindow below — so the regex
// itself stays simple.
var editToolNameRe = regexp.MustCompile(
	`"name"\s*:\s*"(?:` + strings.Join(editToolNames, "|") + `)"`,
)

// pathFieldRe matches `"<path-key>":"<value>"` after the window has
// been cleaned of JSON backslash-escapes. We support the canonical
// "path" plus the snake/camel aliases some tools emit.
var pathFieldRe = regexp.MustCompile(
	`"(?:path|filePath|file_path|filepath)"\s*:\s*"([^"]+)"`,
)

// pathWindowBytes bounds how far past a tool-name match we look for
// a path field. Most tool arg blobs are < 1 KiB; 4 KiB is comfortably
// larger than any real-world OpenCode tool call.
const pathWindowBytes = 4 * 1024

// DetectEdits scans body for tool-call patterns that look like file
// edits and returns one Event per match. The function is exported so
// the chat handler (or a future CLI tool) can run it on captured
// response bodies; the verifier itself doesn't call it.
//
// The scan is best-effort and intentionally tolerant: malformed JSON,
// missing fields, or unknown tool shapes are silently dropped. Callers
// care about coverage ("did we catch this edit?") far more than
// precision ("is this JSON-perfect?").
//
// The returned events are duplicates-deduplicated by Path within a
// single body so a long streamed response that mentions the same
// file twice yields at most one event per file.
func DetectEdits(body string) []Event {
	if body == "" {
		return nil
	}
	// Pre-clean: strip JSON backslash-escapes so the regexes can
	// match the standard form. The model output nests tool calls
	// inside `function.arguments`, a string-encoded JSON blob, so
	// the body bytes look like {\"name\":\"edit_file\"...} rather
	// than {"name":"edit_file"...}.
	cleaned := cleanWindow(body)
	seen := make(map[string]bool)
	var out []Event
	for _, idxs := range editToolNameRe.FindAllStringIndex(cleaned, -1) {
		// Pull a window that starts at the tool-name match and
		// reaches forward at most pathWindowBytes. The path
		// field that the model likely wrote lives near the
		// same name, and a tight window keeps the path regex
		// scan cheap on long streamed responses.
		start := idxs[0]
		end := start + pathWindowBytes
		if end > len(cleaned) {
			end = len(cleaned)
		}
		path := extractPath(cleaned[start:end])
		if path == "" {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, Event{
			Path:     path,
			ToolName: extractToolName(cleaned[start:end]),
		})
	}
	return out
}

// cleanWindow reverses JSON backslash-escapes per RFC 8259 §7. We
// only consume the small subset of escapes a tool-call envelope
// ever carries: \" -> ", \\ -> \, \/ -> /. Paths essentially never
// contain \n, \t, etc., so we skip those for simplicity.
//
// The function is single-pass over the window; nested escapes
// (e.g. a path value that itself contains a backslash-quote pair)
// round-trip correctly because \\ -> \ happens before \" -> ".
func cleanWindow(window string) string {
	if !strings.Contains(window, `\`) {
		return window
	}
	var b strings.Builder
	b.Grow(len(window))
	for i := 0; i < len(window); i++ {
		c := window[i]
		if c != '\\' || i+1 >= len(window) {
			b.WriteByte(c)
			continue
		}
		next := window[i+1]
		switch next {
		case '"', '\\', '/':
			b.WriteByte(next)
			i++ // skip the backslash
		default:
			// Unknown escape — keep both characters verbatim so
			// downstream regexes still see the original shape.
			b.WriteByte(c)
		}
	}
	return b.String()
}

// extractPath returns the value of the first plausible path field
// found in cleaned window, or "" when no field matches.
func extractPath(window string) string {
	if m := pathFieldRe.FindStringSubmatch(window); len(m) >= 2 && m[1] != "" {
		return m[1]
	}
	return ""
}

// extractToolName pulls the bare tool name out of a cleaned window.
// Returns "edit" as a fallback — the verifier doesn't depend on the
// label; it's just the value we observed at detection time.
func extractToolName(window string) string {
	if m := editToolNameRe.FindStringSubmatch(window); len(m) >= 1 {
		return stripJSONValue(m[0])
	}
	return "edit"
}

// stripJSONValue trims the surrounding `"..."` wrapper the regex
// matched, leaving the bare identifier. Used only for human-readable
// labelling.
func stripJSONValue(s string) string {
	s = strings.TrimPrefix(s, `"name"`)
	s = strings.TrimPrefix(s, `:`)
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, `"`)
	s = strings.TrimPrefix(s, `"`)
	return s
}
