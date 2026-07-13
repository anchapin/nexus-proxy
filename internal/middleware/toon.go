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
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// JSONArrayBlockRE is a loose fenced-block extractor. It captures any ```
// wrapper (with or without a language tag) and its content. Validation that
// the content is a JSON array of objects is done via json.Unmarshal in the
// compression loop — this keeps the regex simple and avoids missing valid
// shapes due to regex edge cases.
var JSONArrayBlockRE = regexp.MustCompile("(?s)```(json)?\\s*([\\s\\S]*?)\\s*```")

// compressJSONBlock extracts a JSON array candidate from a fenced block and
// returns the TOON replacement string. Returns ("", false) if the block
// content cannot be parsed as []map[string]interface{}.
func compressJSONBlock(block string) (toon string, ok bool) {
	var arr []map[string]interface{}
	if err := json.Unmarshal([]byte(block), &arr); err != nil {
		return "", false
	}
	if len(arr) == 0 {
		// Empty array — SerializeToTOON handles this gracefully but skip
		// compressing empty blocks to keep output readable.
		return "", false
	}
	out, err := SerializeToTOON([]byte(block))
	if err != nil {
		return "", false
	}
	return out, true
}

// CompressJSONBlocks rewrites every fenced JSON array block in the
// user/assistant message content into a TOON text block. Returns true if any
// block was rewritten. Schema is inferred from the first object's keys (sorted
// lexicographically for stable column order).
//
// Unlike the old single-regex approach, this two-pass strategy accepts:
//   - Any ``` wrapper (```json`, “ `, or bare)
//   - Multi-line arrays whose objects span several lines
//
// Gotchas to be aware of when re-parsing TOON output downstream:
//   - Commas inside string values are replaced with the full-width U+FF0C
//     so they cannot collide with the column separator.
//   - Newlines inside string values are replaced with spaces — multi-line
//     strings round-trip lossy.
func CompressJSONBlocks(messages []interface{}) bool {
	rewrote := false
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

		// Pass 1: loose regex extracts all fenced candidates.
		matches := JSONArrayBlockRE.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			// m[0] = full match, m[1] = optional language, m[2] = content
			if len(m) < 3 {
				continue
			}
			cand := strings.TrimSpace(m[2])
			toon, ok := compressJSONBlock(cand)
			if !ok {
				continue
			}
			block := "```text\n" + toon + "```"
			content = strings.Replace(content, m[0], block, 1)
			rewrote = true
		}
		if rewrote {
			msg["content"] = content
		}
	}
	return rewrote
}

// MinUnfencedRows is the minimum array length the unfenced pass rewrites.
// Below this the TOON schema header costs more tokens than the columnar
// rows save. The fenced path (CompressJSONBlocks) has no such floor because
// a fenced block is already an explicit "this is data" signal from the
// sender, so even a single-row fence is honoured.
const MinUnfencedRows = 2

// CompressUnfencedJSONArrays applies TOON compression to bare (unfenced)
// JSON arrays-of-objects in user/assistant message content. Unlike
// CompressJSONBlocks (which only rewrites ```json fences), this pass
// handles arrays pasted without a fence and arrays embedded in prose —
// the issue #123 case where a user drops `[{...},{...}]` directly into
// the prompt with no code fence around it.
//
// Detection is a deliberately conservative two-stage pipeline:
//
//  1. A bracket-balancing scanner (findJSONArrayCandidates) locates
//     candidate `[...]` substrings whose first element is an object,
//     tracking nested `[]`, `{}`, and string literals with escape
//     handling so brackets inside JSON strings do not fool it.
//  2. Each candidate is validated by json.Unmarshal into
//     `[]map[string]interface{}` and only arrays with >= MinUnfencedRows
//     rows are rewritten. Candidates that fail to parse are left
//     untouched — the scanner is a candidate generator, the parser is
//     the authority, so false positives are impossible.
//
// Surrounding prose is preserved: only the matched byte range is
// replaced, and the replacement uses the same ```text fence and
// SerializeToTOON rules (full-width comma, newline -> space) as the
// fenced path so downstream consumers see one TOON shape. Returns true
// if any array was rewritten.
func CompressUnfencedJSONArrays(messages []interface{}) bool {
	rewrote := false
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
		// Cheap rejection: an array-of-objects needs both '[' and '{'.
		// This keeps the common no-JSON prompt off the scanner entirely.
		if !strings.Contains(content, "[") || !strings.Contains(content, "{") {
			continue
		}
		newContent, changed := compressUnfencedArraysInString(content)
		if changed {
			msg["content"] = newContent
			rewrote = true
		}
	}
	return rewrote
}

// compressUnfencedArraysInString rewrites every qualifying unfenced
// JSON array-of-objects in content into a fenced TOON block, leaving
// all other bytes (including prose between arrays) untouched.
func compressUnfencedArraysInString(content string) (string, bool) {
	candidates := findJSONArrayCandidates(content)
	if len(candidates) == 0 {
		return content, false
	}
	// Replace right-to-left so the byte offsets of candidates to the
	// left of the current replacement stay valid against the original
	// scan.
	out := content
	changed := false
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		sub := out[c.start:c.end]
		toon, ok := serializeUnfencedCandidate([]byte(sub))
		if !ok {
			continue
		}
		replacement := "```text\n" + toon + "```"
		out = out[:c.start] + replacement + out[c.end:]
		changed = true
	}
	return out, changed
}

// serializeUnfencedCandidate validates that b is a JSON array of >=
// MinUnfencedRows objects and returns its TOON serialization. The bool
// result is false for any non-array-of-objects shape, an array of
// primitives, a single-element array, or a parse failure — callers
// treat all of these as "leave untouched".
func serializeUnfencedCandidate(b []byte) (string, bool) {
	var data []map[string]interface{}
	if err := json.Unmarshal(b, &data); err != nil {
		return "", false
	}
	if len(data) < MinUnfencedRows {
		return "", false
	}
	toon, err := SerializeToTOON(b)
	if err != nil {
		return "", false
	}
	return toon, true
}

// unfencedCandidate marks a [start, end) byte range in the original
// content that the scanner believes may be a JSON array-of-objects.
type unfencedCandidate struct{ start, end int }

// findJSONArrayCandidates scans content for substrings that look like a
// JSON array of objects: a '[' immediately followed (after optional
// whitespace) by a '{', extended to its bracket-balanced ']'. Callers
// MUST validate candidates with json.Unmarshal — the scanner only
// balances structural delimiters, it does not enforce value shape, key
// validity, or element homogeneity, so e.g. `[{"a":1}, 2]` is emitted
// as a candidate and then rejected by the parser.
func findJSONArrayCandidates(content string) []unfencedCandidate {
	var out []unfencedCandidate
	n := len(content)
	i := 0
	for i < n {
		if content[i] != '[' {
			i++
			continue
		}
		// Peek: the next non-whitespace byte must be '{' to be a
		// plausible array-of-objects. This rejects arrays of
		// primitives (`[1,2,3]`) at the candidate stage.
		j := i + 1
		for j < n && isJSONSpace(content[j]) {
			j++
		}
		if j >= n || content[j] != '{' {
			i++
			continue
		}
		end, ok := scanJSONArrayEnd(content, i)
		if !ok {
			i++
			continue
		}
		out = append(out, unfencedCandidate{start: i, end: end})
		// Resume scanning AFTER the candidate so we do not re-emit
		// inner arrays (the outermost array is the one TOON wants).
		i = end
	}
	return out
}

// scanJSONArrayEnd returns the index just past the ']' that closes the
// array opened at content[start], tracking nested '[]' and '{}' as well
// as string literals with JSON escape handling so brackets appearing
// inside strings do not perturb the depth counters. Returns ok=false
// for unbalanced input or input that ends inside an unterminated
// string — the caller treats that as "not a candidate" and the parser
// would reject it anyway.
func scanJSONArrayEnd(content string, start int) (int, bool) {
	depth := 0  // [...] nesting
	bdepth := 0 // {...} nesting
	inStr := false
	escape := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if inStr {
			switch {
			case escape:
				escape = false
			case c == '\\':
				escape = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 && bdepth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		case '{':
			bdepth++
		case '}':
			bdepth--
			if bdepth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

// isJSONSpace reports whether b is one of the four whitespace bytes
// permitted between JSON tokens (RFC 8259 §2).
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
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
