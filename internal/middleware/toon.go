// Package middleware contains the prompt-transformation passes that run
// before routing: meta-prompt injection, TOON compression, and RAG lookup.
//
// Each pass takes and returns []interface{} (the heterogeneous OpenAI
// message shape) so they can be chained. Passes must not depend on global
// state — they receive their configuration through their constructor or
// per-call arguments.
package middleware

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
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
var UnfencedArrayBlock = regexp.MustCompile(
	`(?:^|[\n\r\t ])` + // start of string or preceded by whitespace/newline
		`(\[\s*\{.*?\}(?:\s*,\s*\{.*?\})*\s*\])` + // the array itself
		`[\n\r\t ]?`, // optional trailing whitespace/newline (consumed to avoid leaving it)
)

// CompressionMethod indicates which TOON compression pattern was applied
// to a request's messages (issue #247).
type CompressionMethod string

const (
	CompressionMethodFenced   CompressionMethod = "fenced"
	CompressionMethodNested   CompressionMethod = "nested"
	CompressionMethodUnfenced CompressionMethod = "unfenced"
	CompressionMethodNone     CompressionMethod = ""
)

var ErrHeterogeneousKeys = errors.New("toon: heterogeneous keys in JSON array")

// CompressJSONBlocks rewrites every ```json [ {...}, ... ] ``` block in the
// user/assistant message content into a TOON text block. Returns the
// compression method used: "fenced" for ```json [...] ``` blocks, "nested"
// for {"files": [...]} object-nested arrays, "unfenced" for standalone
// [...] arrays, or "" when no compression was applied. Schema is inferred
// from the first object's keys (sorted lexicographically for stable column order).
//
// Gotchas to be aware of when re-parsing TOON output downstream:
//   - Commas inside string values are replaced with the full-width U+FF0C
//     so they cannot collide with the column separator.
//   - Newlines inside string values are replaced with spaces — multi-line
//     strings round-trip lossy.
func CompressJSONBlocks(messages []interface{}) CompressionMethod {
	fenced, nested, unfenced := false, false, false
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
				if errors.Is(err, ErrHeterogeneousKeys) {
					slog.Debug("toon: skipping fenced JSON array with heterogeneous keys", "block", truncate(m[1], 64))
				}
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
				if errors.Is(err, ErrHeterogeneousKeys) {
					slog.Debug("toon: skipping nested JSON array with heterogeneous keys", "array", truncate(arrayMatch, 64))
				}
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
		unfencedMatches := UnfencedArrayBlock.FindAllStringSubmatchIndex(content, -1)
		for _, m := range unfencedMatches {
			if len(m) < 4 {
				continue
			}
			// m[0], m[1]: full match (leading context + array + optional trailing ws)
			// m[2], m[3]: captured group (the array itself)
			arrayMatch := content[m[2]:m[3]]
			toon, err := SerializeToTOON([]byte(arrayMatch))
			if err != nil {
				if errors.Is(err, ErrHeterogeneousKeys) {
					slog.Debug("toon: skipping unfenced JSON array with heterogeneous keys", "array", truncate(arrayMatch, 64))
				}
				continue
			}
			// Preserve leading context (newline/whitespace) by replacing only
			// from end of leading context to end of full match with the TOON block.
			// m[1] is the end of leading context (start of captured array).
			leadingContext := content[m[0]:m[2]] // e.g., "\n"
			replacement := leadingContext + toon
			fullMatch := content[m[0]:m[1]]
			content = strings.Replace(content, fullMatch, replacement, 1)
			unfenced = true
		}

		if fenced || nested || unfenced {
			msg["content"] = content
		}
	}
	if fenced {
		return CompressionMethodFenced
	}
	if nested {
		return CompressionMethodNested
	}
	if unfenced {
		return CompressionMethodUnfenced
	}
	return CompressionMethodNone
}

// allSameKeys returns true if every object in data has the same set of keys.
// Used to detect heterogeneous JSON arrays which must not be compressed.
func allSameKeys(data []map[string]interface{}) bool {
	if len(data) <= 1 {
		return true
	}
	base := data[0]
	for _, item := range data[1:] {
		if !reflect.DeepEqual(keysOf(base), keysOf(item)) {
			return false
		}
	}
	return true
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// SerializeToTOON converts a JSON array of objects into the canonical TOON
// shape: a header line `items[N]{k1,k2,...}:` followed by indented rows of
// comma-joined values. Returns "items[0]{}:\n" for an empty array so the
// downstream model still sees a well-formed block.
// Returns ErrHeterogeneousKeys if objects in the array have different keys,
// causing the caller to skip compression and fall back to the original JSON.
func SerializeToTOON(jsonBytes []byte) (string, error) {
	var data []map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return "", fmt.Errorf("toon: unmarshal: %w", err)
	}
	if len(data) == 0 {
		return "items[0]{}:\n", nil
	}

	if !allSameKeys(data) {
		return "", fmt.Errorf("%w: objects in array have different keys", ErrHeterogeneousKeys)
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
