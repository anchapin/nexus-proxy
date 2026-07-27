package middleware

import (
	"fmt"
	"testing"
)

func TestSerializeToTOON(t *testing.T) {
	in := []byte(`[{"id":1,"name":"alpha"},{"id":2,"name":"beta, comma"}]`)
	got, err := SerializeToTOON(in)
	if err != nil {
		t.Fatalf("SerializeToTOON: %v", err)
	}
	want := "items[2]{id,name}:\n  1,alpha\n  2,beta， comma\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSerializeToTOONEmpty(t *testing.T) {
	got, err := SerializeToTOON([]byte(`[]`))
	if err != nil {
		t.Fatalf("SerializeToTOON: %v", err)
	}
	if got != "items[0]{}:\n" {
		t.Errorf("got %q", got)
	}
}

func TestSerializeToTOONInvalid(t *testing.T) {
	if _, err := SerializeToTOON([]byte(`not json`)); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestSerializeToTOONNewlineLossy(t *testing.T) {
	in := []byte(`[{"a":"line1\nline2"}]`)
	got, err := SerializeToTOON(in)
	if err != nil {
		t.Fatalf("SerializeToTOON: %v", err)
	}
	if want := "items[1]{a}:\n  line1 line2\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestCompressJSONBlocksRewritesUserMessage(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user", "content": "Here:\n```json\n[{\"a\":1},{\"a\":2}]\n```\nDone.",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodFenced {
		t.Fatal("expected rewrote = fenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "```text\nitems[2]{a}:\n  1\n  2\n```") {
		t.Errorf("TOON block not present in %q", content)
	}
	if contains(content, "```json") {
		t.Errorf("original json fence should be gone, got %q", content)
	}
}

func TestCompressJSONBlocksIgnoresNonUserAssistant(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "```json\n[{\"a\":1}]\n```"},
		map[string]interface{}{"role": "tool", "content": "```json\n[{\"a\":1}]\n```"},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNone {
		t.Error("should not touch system/tool messages")
	}
}

func TestCompressJSONBlocksNoMatch(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "no blocks here"},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNone {
		t.Error("expected no-op when no fences present")
	}
}

func TestCompressJSONBlocksNestedArray(t *testing.T) {
	// Issue #203: nested JSON arrays like {"files": [...]} should be compressed.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `Here are the results: {"files": [{"name": "a.txt"}, {"name": "b.txt"}]}`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNested {
		t.Fatal("expected rewrote = nested")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{name}`) {
		t.Errorf("TOON not present in %q", content)
	}
	// The original array format should be replaced
	if contains(content, `{"files": [{"name": "a.txt"}]`) {
		t.Errorf("original array should be compressed, got %q", content)
	}
}

func TestCompressJSONBlocksNestedArrayMultipleObjects(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `{"results": [{"id": 1, "name": "alpha"}, {"id": 2, "name": "beta"}]}`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNested {
		t.Fatal("expected rewrote = nested")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{id,name}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksNestedArrayDifferentKeys(t *testing.T) {
	// Issue #244: expanded to cover "data", "entries", "records" keys.
	for _, key := range []string{"items", "objects", "data", "entries", "records"} {
		msgs := []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": fmt.Sprintf(`{"%s": [{"x": 1}, {"x": 2}]}`, key),
			},
		}
		if CompressJSONBlocks(msgs, true) != CompressionMethodNested {
			t.Errorf("expected rewrote = nested for key %q", key)
		}
		content := msgs[0].(map[string]interface{})["content"].(string)
		if !contains(content, `items[2]{x}`) {
			t.Errorf("TOON not present for key %q: %q", key, content)
		}
	}
}

func TestCompressJSONBlocksUnfencedArray(t *testing.T) {
	// Issue #244: unfenced JSON arrays should be compressed.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Results:\n[{\"name\": \"a.txt\"}, {\"name\": \"b.txt\"}]\n",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{name}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedArrayMultipleObjects(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"id": 1, "name": "alpha"}, {"id": 2, "name": "beta"}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{id,name}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedArrayWithNewlinePrefix(t *testing.T) {
	// Array at start of content (no preceding context).
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "assistant",
			"content": "[{\"a\": 1}, {\"a\": 2}]",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{a}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedArrayPreservesOtherContent(t *testing.T) {
	// Content outside the unfenced array should be preserved.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Before\n[{\"x\": 1}, {\"x\": 2}]\nAfter",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "Before\n") {
		t.Errorf("prefix lost, got %q", content)
	}
	if !contains(content, "\nAfter") {
		t.Errorf("suffix lost, got %q", content)
	}
}

func TestCompressJSONBlocksUnfencedArrayNoMatchOnCasualBrackets(t *testing.T) {
	// Casual bracket usage like "[ see above ]" should not be matched.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "The results are [see above] for the analysis.",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNone {
		t.Errorf("should not match casual brackets, got %q", msgs[0].(map[string]interface{})["content"])
	}
}

func TestCompressJSONBlocksUnfencedDisabled(t *testing.T) {
	// Issue #535: when unfenced=false, bare JSON arrays must NOT be compressed.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Results:\n[{\"name\": \"a.txt\"}, {\"name\": \"b.txt\"}]\n",
		},
	}
	if CompressJSONBlocks(msgs, false) != CompressionMethodNone {
		t.Error("unfenced=false: bare array should NOT be compressed")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if content != "Results:\n[{\"name\": \"a.txt\"}, {\"name\": \"b.txt\"}]\n" {
		t.Errorf("bare array was modified, got: %q", content)
	}
}

func TestCompressJSONBlocksUnfencedDisabledFencedStillWorks(t *testing.T) {
	// Issue #535: fenced blocks must still be compressed when unfenced=false.
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user", "content": "Here:\n```json\n[{\"a\":1},{\"a\":2}]\n```\nDone.",
		},
	}
	if CompressJSONBlocks(msgs, false) != CompressionMethodFenced {
		t.Fatal("fenced blocks must still work when unfenced=false")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "```text\nitems[2]{a}:\n  1\n  2\n```") {
		t.Errorf("TOON block not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedDisabledNestedStillWorks(t *testing.T) {
	// Issue #535: nested arrays must still be compressed when unfenced=false.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `Here are the results: {"files": [{"name": "a.txt"}, {"name": "b.txt"}]}`,
		},
	}
	if CompressJSONBlocks(msgs, false) != CompressionMethodNested {
		t.Fatal("nested arrays must still work when unfenced=false")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{name}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedNestedObjects(t *testing.T) {
	// Issue #772: arrays with nested objects should be compressed.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"outer": {"inner": 1}}, {"outer": {"inner": 2}}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for nested objects")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{outer}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedNestedObjectsDeep(t *testing.T) {
	// Deeply nested objects.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"a": {"b": {"c": 1}}}, {"a": {"b": {"c": 2}}}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for deeply nested objects")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{a}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedBracketInString(t *testing.T) {
	// Issue #772: strings containing bracket characters should not break detection.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"name": "[test]"}, {"name": "[foo]"}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for bracket-containing strings")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{name}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedCurlyBraceInString(t *testing.T) {
	// Strings containing curly braces should not break detection.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"code": "const x = {};"}, {"code": "const y = {a:1};"}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for curly-brace-containing strings")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{code}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedMultiLine(t *testing.T) {
	// Issue #772: multi-line arrays should be compressed.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "[\n  {\"a\": 1},\n  {\"a\": 2}\n]",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for multi-line arrays")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{a}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedParagraphEnd(t *testing.T) {
	// Issue #772: array at end of paragraph (no trailing context) should compress.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Some text describing the data:\n[{\"x\": 1}, {\"x\": 2}]",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for paragraph-end array")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{x}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedParagraphStart(t *testing.T) {
	// Issue #772: array at start of paragraph should compress.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "[{\"x\": 1}, {\"x\": 2}]\nMore text after.",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for paragraph-start array")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{x}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedMixedNesting(t *testing.T) {
	// Mixed nested and flat objects in the same array.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"flat": 1, "nested": {"a": 1}}, {"flat": 2, "nested": {"a": 2}}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for mixed nesting")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{flat,nested}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedMultipleInContent(t *testing.T) {
	// Multiple unfenced arrays in the same message.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "[{\"a\": 1}]\nContent\n[{\"b\": 2}]",
		},
	}
	// Single-element arrays should not be compressed (requires >= 2 objects).
	if CompressJSONBlocks(msgs, true) != CompressionMethodNone {
		t.Error("single-element arrays should not be compressed")
	}
}

func TestCompressJSONBlocksUnfencedMultipleBothCompressed(t *testing.T) {
	// Two separate valid unfenced arrays in the same message.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "[{\"a\": 1}, {\"a\": 2}]\nBetween\n[{\"b\": 3}, {\"b\": 4}]",
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{a}`) {
		t.Errorf("first array not compressed in %q", content)
	}
	if !contains(content, `items[2]{b}`) {
		t.Errorf("second array not compressed in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedEscapedQuote(t *testing.T) {
	// Strings with escaped quotes containing brackets.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"s": "a \"[b]\""}, {"s": "c \"[d]\""}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for escaped-quote strings")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{s}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedNullAndBool(t *testing.T) {
	// Objects with null and boolean values alongside nested objects.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"active": true, "meta": null, "nested": {"x": 1}}, {"active": false, "meta": null, "nested": {"x": 2}}]`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodUnfenced {
		t.Fatal("expected rewrote = unfenced for mixed type values")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `items[2]{active,meta,nested}`) {
		t.Errorf("TOON not present in %q", content)
	}
}

func TestCompressJSONBlocksUnfencedCasualBracketNoMatch(t *testing.T) {
	// Casual bracket pairs that superficially resemble arrays must not match.
	casualPairs := []string{
		"[see above]",
		"[this is a list]",
		"[item1, item2]", // comma-separated not JSON objects
		" [{x}] ",        // single object - requires >= 2
	}
	for _, content := range casualPairs {
		msgs := []interface{}{
			map[string]interface{}{"role": "user", "content": content},
		}
		if CompressJSONBlocks(msgs, true) != CompressionMethodNone {
			t.Errorf("should not match casual bracket %q", content)
		}
	}
}

func TestCompressJSONBlocksNestedArrayPreservesOtherContent(t *testing.T) {
	// Content outside the nested array should be preserved.
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `Prefix {"files": [{"a": 1}]} Suffix`,
		},
	}
	if CompressJSONBlocks(msgs, true) != CompressionMethodNested {
		t.Fatal("expected rewrote = nested")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "Prefix ") {
		t.Errorf("prefix lost, got %q", content)
	}
	if !contains(content, " Suffix") {
		t.Errorf("suffix lost, got %q", content)
	}
}

func TestAppendSystemNoteExisting(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "hello"},
		map[string]interface{}{"role": "user", "content": "x"},
	}
	out := AppendSystemNote(msgs, " NOTICE")
	if got := out[0].(map[string]interface{})["content"]; got != "hello NOTICE" {
		t.Errorf("got %q", got)
	}
	if len(out) != 2 {
		t.Errorf("should not add a new system msg, len=%d", len(out))
	}
}

func TestAppendSystemNoteCreates(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "x"},
	}
	out := AppendSystemNote(msgs, " NOTICE")
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].(map[string]interface{})["role"] != "system" {
		t.Error("first message should now be system")
	}
}

func TestLatestSystemIndex(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "u"},
		map[string]interface{}{"role": "system", "content": "s"},
	}
	if got := LatestSystemIndex(msgs); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
	if got := LatestSystemIndex([]interface{}{}); got != -1 {
		t.Errorf("empty: got %d, want -1", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
