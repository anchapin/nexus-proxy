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

// JSONArrayBlock matches a fenced ```json ... ``` block whose body is a JSON
// array of objects. We only compress the array-of-objects shape because TOON
// is columnar — primitives and arrays of primitives don't benefit.
var JSONArrayBlock = regexp.MustCompile("(?s)" + "```" + `json\s*(\[\s*\{.*?\}\s*\])\s*` + "```")

// CompressJSONBlocks rewrites every ```json [ {...}, ... ] ``` block in the
// user/assistant message content into a TOON text block. Returns true if any
// block was rewritten. Schema is inferred from the first object's keys (sorted
// lexicographically for stable column order).
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
			rewrote = true
		}
		if rewrote {
			msg["content"] = content
		}
	}
	return rewrote
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
