// Package middleware contains the prompt-transformation passes that run
// before routing: meta-prompt injection, TOON compression, and RAG lookup.
//
// Each pass takes and returns []interface{} (the heterogeneous OpenAI
// message shape) so they can be chained. Passes must not depend on global
// state — they receive their configuration through their constructor or
// per-call arguments.
package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// JSONArrayBlock matches a fenced ```json ... ``` block whose body is a JSON
// array of objects. We only compress the array-of-objects shape because TOON
// is columnar — primitives and arrays of primitives don't benefit.
var JSONArrayBlock = regexp.MustCompile("(?s)" + "```" + `json\s*(\[\s*\{.*?\}\s*\])\s*` + "```")

// ObjectArrayBlock matches a JSON object containing a key whose value is a JSON
// array of objects. This handles the common "files", "results", "items", "data",
// "entries", "records" key patterns seen in tool results and multi-file diffs.
// The pattern handles simple objects (no deeply nested structures) which covers
// the vast majority of structured data in prompts.
var ObjectArrayBlock = regexp.MustCompile(
	`"` + `(?:files|results|items|objects|data|entries|records)` + `"` + `\s*:\s*` +
		`(\[\s*\{.*?\}(?:\s*,\s*\{.*?\})*\s*\])`,
)

// UnfencedArrayBlock matches a standalone JSON array of objects that appears
// without code fences in user/assistant content. This captures compressible
// arrays from tool results, search hits, and file listings that developers
// paste inline. The leading newline/whitespace guard prevents matching casual
// bracket pairs in prose. The trailing [\n\r\t ] is captured as part of the
// match so the replacement can remove trailing context cleanly.
//
// Deprecated: replaced by state-machine scanning in scanUnfencedArrays which
// correctly handles nested objects and bracket-containing strings.
var UnfencedArrayBlock = regexp.MustCompile(
	`(?:^|[\n\r\t ])` + // start of string or preceded by whitespace/newline
		`(\[\s*\{.*?\}(?:\s*,\s*\{.*?\})*\s*\])` + // the array itself
		`[\n\r\t ]?`, // optional trailing whitespace/newline (consumed to avoid leaving it)
)

// findArrayEnd scans content[i:] looking for the matching ] that closes the
// top-level [ at position i. It uses a bracket-depth state machine that tracks
// { } [ ] and " states, ignoring nested structure. It correctly handles nested
// objects and strings containing bracket characters. Returns the index of the
// closing ] (exclusive end), or -1 if no valid array boundary is found.
func findArrayEnd(content string, i int) int {
	if i >= len(content) || content[i] != '[' {
		return -1
	}
	depth := 1
	inString := false
	i++
	for i < len(content) {
		c := content[i]
		if c == '\\' && i+1 < len(content) {
			i += 2 // skip escaped character
			continue
		}
		if c == '"' {
			inString = !inString
			i++
			continue
		}
		if inString {
			i++
			continue
		}
		switch c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1 // exclusive end
			}
		}
		i++
	}
	return -1
}

// parseObjectArray validates that the trimmed content is a JSON array of objects
// with at least 2 elements. It returns the unmarshaled data on success or false.
func parseObjectArray(content []byte) ([]map[string]interface{}, bool) {
	content = bytes.TrimSpace(content)
	if len(content) < 2 || content[0] != '[' || content[len(content)-1] != ']' {
		return nil, false
	}
	var data []map[string]interface{}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, false
	}
	if len(data) < 2 {
		return nil, false
	}
	return data, true
}

// serializeToTOONData converts already-unmarshaled JSON array of objects into the
// canonical TOON shape. Exposed so scanUnfencedArrays can reuse the unmarshaled
// data from parseObjectArray instead of double-parsing.
func serializeToTOONData(data []map[string]interface{}) (string, error) {
	if len(data) == 0 {
		return "items[0]{}:\n", nil
	}

	keys := make([]string, 0, len(data[0]))
	for k := range data[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	fmt.Fprintf(&sb, "items[%d]{%s}:\n", len(data), strings.Join(keys, ","))
	for _, item := range data {
		vals := make([]string, len(keys))
		for i, k := range keys {
			v := fmt.Sprintf("%v", item[k])
			v = strings.ReplaceAll(v, ",", "，") // protect column separator
			v = strings.ReplaceAll(v, "\n", " ")
			vals[i] = v
		}
		sb.WriteString("  " + strings.Join(vals, ",") + "\n")
	}
	return sb.String(), nil
}

// scanUnfencedArrays scans content for unfenced JSON arrays of objects using
// a state-machine bracket counter. It handles nested objects and strings
// containing brackets. Returns the (possibly modified) content string.
func scanUnfencedArrays(content string, didUnfenced *bool) string {
	skipEnd := -1 // last position to skip (exclusive end of a previously replaced array)
	for i := 0; i < len(content); i++ {
		if i >= skipEnd {
			skipEnd = -1
		}
		if content[i] != '[' {
			continue
		}
		if skipEnd != -1 && i < skipEnd {
			continue
		}
		if !isValidArrayStart(content, i) {
			continue
		}
		end := findArrayEnd(content, i)
		if end == -1 {
			continue
		}
		arrayContent := bytes.TrimSpace([]byte(content[i:end]))
		data, ok := parseObjectArray(arrayContent)
		if !ok {
			continue
		}
		toon, err := serializeToTOONData(data)
		if err != nil {
			continue
		}
		content = content[:i] + toon + content[end:]
		*didUnfenced = true
		// Mark the replaced region so we skip any [ within the TOON output.
		skipEnd = i + len(toon)
		// Continue scanning after the TOON output.
		i = skipEnd - 1
	}
	return content
}

// isValidArrayStart returns true if content[i] is a '[' that could be the
// start of an unfenced array. It must be at the start of content or preceded
// by whitespace, and NOT inside a string.
func isValidArrayStart(content string, i int) bool {
	if i == 0 {
		return true
	}
	c := content[i-1]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// CompressionMethod indicates which TOON compression pattern was applied
// to a request's messages (issue #247).
type CompressionMethod string

const (
	CompressionMethodFenced   CompressionMethod = "fenced"
	CompressionMethodNested   CompressionMethod = "nested"
	CompressionMethodUnfenced CompressionMethod = "unfenced"
	CompressionMethodNone     CompressionMethod = ""
)

// CompressJSONBlocks rewrites every ```json [ {...}, ... ] ``` block in the
// user/assistant message content into a TOON text block. Returns the
// compression method used: "fenced" for ```json [...] ``` blocks, "nested"
// for {"files": [...]} object-nested arrays, "unfenced" for standalone
// [...] arrays, or "" when no compression was applied. Schema is inferred
// from the first object's keys (sorted lexicographically for stable column order).
// When unfenced is false, the unfenced array pass is skipped entirely.
//
// Gotchas to be aware of when re-parsing TOON output downstream:
//   - Commas inside string values are replaced with the full-width U+FF0C
//     so they cannot collide with the column separator.
//   - Newlines inside string values are replaced with spaces — multi-line
//     strings round-trip lossy.
func CompressJSONBlocks(messages []interface{}, unfenced bool) CompressionMethod {
	fenced, nested, didUnfenced := false, false, false
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		content, _ := msg["content"].(string)

		// Handle fenced ```json [...] ``` blocks.
		matches := JSONArrayBlock.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			toon, err := SerializeToTOON([]byte(m[1]))
			if err != nil {
				continue
			}
			block := "```text\n" + toon + "```"
			content = strings.Replace(content, m[0], block, 1)
			fenced = true
		}

		// Handle JSON arrays nested inside objects (e.g., {"files": [...]}).
		objMatches := ObjectArrayBlock.FindAllStringSubmatchIndex(content, -1)
		for _, m := range objMatches {
			if len(m) < 4 {
				continue
			}
			// m[0], m[1]: full match (from opening " of key to closing ] of array)
			// m[2], m[3]: captured group (the array itself)
			fullMatch := content[m[0]:m[1]]
			arrayMatch := content[m[2]:m[3]]
			toon, err := SerializeToTOON([]byte(arrayMatch))
			if err != nil {
				continue
			}
			// Extract key from full match: "key": [...] -> "key": <toon>
			colonIdx := strings.Index(fullMatch, ":")
			if colonIdx == -1 {
				continue
			}
			keyPart := fullMatch[:colonIdx+1]
			replacement := keyPart + " " + strings.TrimSpace(toon)
			// If preceded by {, include it in the replacement to avoid leaving it orphaned
			if m[0] > 0 && content[m[0]-1] == '{' {
				content = strings.Replace(content, "{"+fullMatch, "{"+replacement, 1)
			} else {
				content = strings.Replace(content, fullMatch, replacement, 1)
			}
			nested = true
		}

		// Handle unfenced standalone JSON arrays (no code fences).
		// Uses a state-machine scanner instead of regex to correctly handle
		// nested objects and strings containing bracket characters.
		if unfenced {
			content = scanUnfencedArrays(content, &didUnfenced)
		}

		if fenced || nested || didUnfenced {
			msg["content"] = content
		}
	}
	if fenced {
		return CompressionMethodFenced
	}
	if nested {
		return CompressionMethodNested
	}
	if didUnfenced {
		return CompressionMethodUnfenced
	}
	return CompressionMethodNone
}

// SerializeToTOON converts a JSON array of objects into the canonical TOON
// shape: a header line `items[N]{k1,k2,...}:` followed by indented rows of
// comma-joined values. Returns "items[0]{}:\n" for an empty array so the
// downstream model still sees a well-formed block.
func SerializeToTOON(jsonBytes []byte) (string, error) {
	var data []map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return "", fmt.Errorf("toon: unmarshal: %w", err)
	}
	return serializeToTOONData(data)
}

// AppendSystemNote adds a trailing notice to the first system message,
// prepending a new system message if none exists. Returns the (possibly
// modified) messages slice.
func AppendSystemNote(messages []interface{}, notice string) []interface{} {
	for i, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			content, _ := msg["content"].(string)
			msg["content"] = content + notice
			return messages
		}
		_ = i
	}
	newSys := map[string]interface{}{"role": "system", "content": notice}
	return append([]interface{}{newSys}, messages...)
}

// LatestSystemIndex returns the index of the first system message in msgs,
// or -1 if none. Exposed for callers that want to mutate the system slot
// directly (rare; prefer AppendSystemNote).
func LatestSystemIndex(msgs []interface{}) int {
	for i, raw := range msgs {
		if msg, ok := raw.(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "system" {
				return i
			}
		}
	}
	return -1
}
