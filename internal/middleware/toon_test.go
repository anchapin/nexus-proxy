package middleware

import (
	"strings"
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
	if !CompressJSONBlocks(msgs) {
		t.Fatal("expected rewrote = true")
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
	if CompressJSONBlocks(msgs) {
		t.Error("should not touch system/tool messages")
	}
}

func TestCompressJSONBlocksNoMatch(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "no blocks here"},
	}
	if CompressJSONBlocks(msgs) {
		t.Error("expected no-op when no fences present")
	}
}

// TestCompressJSONBlocksDoesNotTouchBareArrays is a regression guard for
// issue #123: the fenced path must ignore bare (unfenced) arrays so the
// new unfenced pass is the single owner of that behaviour. If this test
// fails, CompressJSONBlocks has grown a second detection mode that
// belongs in CompressUnfencedJSONArrays instead.
func TestCompressJSONBlocksDoesNotTouchBareArrays(t *testing.T) {
	const bare = `[{"a":1},{"a":2}]`
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "data: " + bare + " end"},
	}
	if CompressJSONBlocks(msgs) {
		t.Error("fenced path must not rewrite bare arrays")
	}
	if got := msgs[0].(map[string]interface{})["content"]; got != "data: "+bare+" end" {
		t.Errorf("content mutated by fenced path: %q", got)
	}
}

// --- issue #123: unfenced / nested-array compression -------------------

func TestCompressUnfencedRewritesBareArray(t *testing.T) {
	// A realistically-sized tabular payload (5 keys, 4 rows) is the
	// case the issue calls out — TOON's columnar header only pays off
	// once there are enough rows that the repeated-key overhead of raw
	// JSON outweighs the schema line. A 2-row array with short values
	// can actually expand, which totalTokenSavings (chat.go:1634)
	// clamps to zero — so this test uses a payload large enough that
	// the rewrite provably shrinks, exercising the positive-savings
	// acceptance criterion end-to-end.
	const bare = `[
		{"id":1,"name":"alpha","lang":"go","cat":"handler","val":"result-one"},
		{"id":2,"name":"beta","lang":"go","cat":"handler","val":"result-two"},
		{"id":3,"name":"gamma","lang":"go","cat":"handler","val":"result-three"},
		{"id":4,"name":"delta","lang":"go","cat":"handler","val":"result-four"}
	]`
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": bare},
	}
	if !CompressUnfencedJSONArrays(msgs) {
		t.Fatal("expected unfenced rewrite = true")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "items[4]{cat,id,lang,name,val}:") {
		t.Errorf("TOON header not present in %q", content)
	}
	// Rows emit values in sorted-key order (cat,id,lang,name,val).
	if !contains(content, "  handler,1,go,alpha,result-one") {
		t.Errorf("first TOON row not present in %q", content)
	}
	// Mirror the handler's savings heuristic (chat.go:1634): the
	// rewrite must shorten the content so TOONSavingsTokens is > 0.
	if len(content) >= len(bare) {
		t.Errorf("unfenced rewrite did not shorten content: got %d bytes, input %d bytes (%s)", len(content), len(bare), content)
	}
}

func TestCompressUnfencedPreservesSurroundingProse(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Here is the data: [{\"a\":1},{\"a\":2}] please analyze.",
		},
	}
	if !CompressUnfencedJSONArrays(msgs) {
		t.Fatal("expected unfenced rewrite = true")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	const prefix = "Here is the data: "
	const suffix = " please analyze."
	if !strings.HasPrefix(content, prefix) {
		t.Errorf("leading prose lost: %q", content)
	}
	if !strings.HasSuffix(content, suffix) {
		t.Errorf("trailing prose lost: %q", content)
	}
	if !contains(content, "```text\nitems[2]{a}:\n  1\n  2\n```") {
		t.Errorf("array not compressed in %q", content)
	}
}

func TestCompressUnfencedSkipsPrimitiveArrays(t *testing.T) {
	// Arrays of primitives gain nothing from TOON's columnar shape
	// (documented at toon.go:19) and must be left byte-identical.
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user", "content": "nums: [1, 2, 3, 4] and strings [\"x\", \"y\"]",
		},
	}
	if CompressUnfencedJSONArrays(msgs) {
		t.Error("must not compress arrays of primitives")
	}
}

func TestCompressUnfencedSkipsSingleElementArray(t *testing.T) {
	// A single-row array does not amortise the TOON schema header;
	// the >=2-row floor (MinUnfencedRows) protects against that.
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user", "content": `[{"only":1}]`,
		},
	}
	if CompressUnfencedJSONArrays(msgs) {
		t.Error("must not compress single-element arrays")
	}
}

func TestCompressUnfencedHandlesMultipleArrays(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "first [{\"a\":1},{\"a\":2}] then [{\"b\":3},{\"b\":4}] done",
		},
	}
	if !CompressUnfencedJSONArrays(msgs) {
		t.Fatal("expected unfenced rewrite = true")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "items[2]{a}:\n  1\n  2") {
		t.Errorf("first array missing in %q", content)
	}
	if !contains(content, "items[2]{b}:\n  3\n  4") {
		t.Errorf("second array missing in %q", content)
	}
	if !strings.HasPrefix(content, "first ") || !strings.HasSuffix(content, " done") {
		t.Errorf("surrounding prose corrupted: %q", content)
	}
}

// TestCompressUnfencedBracketsInsideStrings guards the scanner's string
// handling: a ']' that appears inside a JSON string value must not be
// mistaken for the close of the array.
func TestCompressUnfencedBracketsInsideStrings(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": `[{"q":"a]b"},{"q":"c]d"}]`,
		},
	}
	if !CompressUnfencedJSONArrays(msgs) {
		t.Fatal("expected unfenced rewrite = true despite brackets in strings")
	}
	content := msgs[0].(map[string]interface{})["content"].(string)
	if !contains(content, "items[2]{q}:\n  a]b\n  c]d\n") {
		t.Errorf("bracket-in-string array not compressed correctly: %q", content)
	}
}

func TestCompressUnfencedIgnoresNonUserAssistant(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": `[{"a":1},{"a":2}]`},
		map[string]interface{}{"role": "tool", "content": `[{"a":1},{"a":2}]`},
	}
	if CompressUnfencedJSONArrays(msgs) {
		t.Error("must not touch system/tool messages")
	}
}

func TestCompressUnfencedNoMatch(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "plain prose, no arrays"},
	}
	if CompressUnfencedJSONArrays(msgs) {
		t.Error("expected no-op on prose without arrays")
	}
}

// TestCompressUnfencedSkipsMalformedJSON ensures a structurally balanced
// but semantically invalid candidate is left untouched — the parser is
// the authority and a trailing comma must abort the rewrite rather than
// emit half-compressed output.
func TestCompressUnfencedSkipsMalformedJSON(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "broken [{\"a\":1},{\"a\":2,}] end",
		},
	}
	if CompressUnfencedJSONArrays(msgs) {
		t.Error("must not rewrite malformed JSON")
	}
}

// TestScanJSONArrayEnd is a focused unit test on the bracket-balancing
// scanner that backs candidate detection. It documents the structural
// contract: strings escape their contents, nesting is honoured, and
// unbalanced input is rejected.
func TestScanJSONArrayEnd(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantEnd int  // expected return value (index just past closing ])
		wantOk  bool // expected ok
	}{
		{"simple", `[{"a":1}]`, 9, true},
		{"nested braces", `[{"a":{"b":1}}]`, 15, true},
		{"nested array value", `[{"a":[1,2]}]`, 13, true},
		{"bracket in string", `[{"a":"]"}]`, 11, true},
		{"escaped quote in string", `[{"a":"x\"y"}]`, 14, true},
		{"unbalanced", `[{"a":1`, 0, false},
		{"unterminated string", `[{"a":"abc}]`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEnd, gotOk := scanJSONArrayEnd(tc.input, 0)
			if gotOk != tc.wantOk {
				t.Fatalf("ok = %v, want %v", gotOk, tc.wantOk)
			}
			if gotOk && gotEnd != tc.wantEnd {
				t.Errorf("end = %d, want %d", gotEnd, tc.wantEnd)
			}
		})
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
