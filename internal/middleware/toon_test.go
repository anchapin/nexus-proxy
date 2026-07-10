package middleware

import "testing"

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