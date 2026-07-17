package middleware

import (
	"errors"
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
	if CompressJSONBlocks(msgs) != CompressionMethodFenced {
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
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
		t.Error("should not touch system/tool messages")
	}
}

func TestCompressJSONBlocksNoMatch(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "no blocks here"},
	}
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
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
	if CompressJSONBlocks(msgs) != CompressionMethodNested {
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
	if CompressJSONBlocks(msgs) != CompressionMethodNested {
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
		if CompressJSONBlocks(msgs) != CompressionMethodNested {
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
	if CompressJSONBlocks(msgs) != CompressionMethodUnfenced {
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
	if CompressJSONBlocks(msgs) != CompressionMethodUnfenced {
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
	if CompressJSONBlocks(msgs) != CompressionMethodUnfenced {
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
			"content": "Before\n[{\"x\": 1}]\nAfter",
		},
	}
	if CompressJSONBlocks(msgs) != CompressionMethodUnfenced {
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
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
		t.Errorf("should not match casual brackets, got %q", msgs[0].(map[string]interface{})["content"])
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
	if CompressJSONBlocks(msgs) != CompressionMethodNested {
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

func TestSerializeToTOONHeterogeneousKeysExtra(t *testing.T) {
	in := []byte(`[{"a":1,"b":2}, {"a":3,"c":4}]`)
	_, err := SerializeToTOON(in)
	if err == nil {
		t.Fatal("expected error on heterogeneous keys")
	}
	if !errors.Is(err, ErrHeterogeneousKeys) {
		t.Errorf("got %v, want ErrHeterogeneousKeys", err)
	}
}

func TestSerializeToTOONHeterogeneousKeysMissing(t *testing.T) {
	in := []byte(`[{"a":1,"b":2}, {"a":3}]`)
	_, err := SerializeToTOON(in)
	if err == nil {
		t.Fatal("expected error on heterogeneous keys (missing key)")
	}
	if !errors.Is(err, ErrHeterogeneousKeys) {
		t.Errorf("got %v, want ErrHeterogeneousKeys", err)
	}
}

func TestCompressJSONBlocksHeterogeneousKeysFenced(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "```json\n[{\"a\":1,\"b\":2}, {\"a\":3,\"c\":4}]\n```",
		},
	}
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
		t.Error("expected no compression for heterogeneous fenced array")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "[{\"a\":1,\"b\":2}, {\"a\":3,\"c\":4}]") {
		t.Errorf("original JSON should be preserved, got %q", content)
	}
}

func TestCompressJSONBlocksHeterogeneousKeysNested(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `{"results": [{"id":1,"name":"alpha"}, {"id":2,"extra":"beta"}]}`,
		},
	}
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
		t.Error("expected no compression for heterogeneous nested array")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, `"results"`) {
		t.Errorf("original JSON should be preserved, got %q", content)
	}
}

func TestCompressJSONBlocksHeterogeneousKeysUnfenced(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "[{\"x\":1,\"y\":2}, {\"x\":3}]",
		},
	}
	if CompressJSONBlocks(msgs) != CompressionMethodNone {
		t.Error("expected no compression for heterogeneous unfenced array")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "[{\"x\":1,\"y\":2}, {\"x\":3}]") {
		t.Errorf("original JSON should be preserved, got %q", content)
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
