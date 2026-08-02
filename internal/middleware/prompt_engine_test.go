package middleware

import "testing"

func TestApplyPromptEngineeringAppendsToExistingSystem(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "Original."},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out := ApplyPromptEngineering(msgs, " BOOST")
	sys := out[0].(map[string]interface{})
	if got := sys["content"]; got != "Original.\n BOOST" {
		t.Errorf("got %q", got)
	}
}

func TestApplyPromptEngineeringCreatesSystemWhenMissing(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out := ApplyPromptEngineering(msgs, " BOOST")
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].(map[string]interface{})["role"] != "system" {
		t.Errorf("first should be system, got %v", out[0])
	}
	if got := out[0].(map[string]interface{})["content"]; got != " BOOST" {
		t.Errorf("content = %q", got)
	}
}

func TestApplyPromptEngineeringNoOpOnEmptyMessages(t *testing.T) {
	out := ApplyPromptEngineering(nil, " BOOST")
	if len(out) != 1 {
		t.Fatalf("len=%d, want 1", len(out))
	}
	if out[0].(map[string]interface{})["role"] != "system" {
		t.Errorf("got %v", out[0])
	}
}

// TestInjectRAG_BackwardCompatible confirms the legacy wrapper still
// injects unconditionally (maxBytes == 0 disables the guard).
func TestInjectRAG_BackwardCompatible(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out := InjectRAG(msgs, " CTX")
	if got := out[0].(map[string]interface{})["content"]; got != "hi CTX" {
		t.Errorf("content = %q, want %q", got, "hi CTX")
	}
}

// TestInjectRAGWithLimit_SkipsOversizedBlock covers issue #594: a context
// block that would push the user message past maxBytes is skipped.
func TestInjectRAGWithLimit_SkipsOversizedBlock(t *testing.T) {
	const maxBytes = 1 << 20 // 1 MiB (NEXUS_MAX_BODY_BYTES default)
	// 1-byte prompt + 10 MiB context block must be skipped.
	large := make([]byte, 10<<20)
	for i := range large {
		large[i] = 'x'
	}
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "x"},
	}
	out, injected := InjectRAGWithLimit(msgs, string(large), maxBytes)
	if injected {
		t.Fatal("injected = true, want false for oversized block")
	}
	if got := out[0].(map[string]interface{})["content"]; got != "x" {
		t.Errorf("content mutated to len %d, want unchanged 1-byte prompt", len(got.(string)))
	}
}

// TestInjectRAGWithLimit_InjectsSmallBlock covers issue #594: a context
// block that fits under maxBytes is injected normally.
func TestInjectRAGWithLimit_InjectsSmallBlock(t *testing.T) {
	const maxBytes = 1 << 20 // 1 MiB
	// 1-byte prompt + 100-byte context block fits comfortably.
	block := make([]byte, 100)
	for i := range block {
		block[i] = 'y'
	}
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "x"},
	}
	out, injected := InjectRAGWithLimit(msgs, string(block), maxBytes)
	if !injected {
		t.Fatal("injected = false, want true for small block")
	}
	got, _ := out[0].(map[string]interface{})["content"].(string)
	if got != "x"+string(block) {
		t.Errorf("content len = %d, want %d", len(got), 1+len(block))
	}
}

// TestInjectRAGWithLimit_NoGuardWhenZero confirms maxBytes <= 0 disables
// the guard (pre-#594 behaviour) so any block is injected.
func TestInjectRAGWithLimit_NoGuardWhenZero(t *testing.T) {
	large := make([]byte, 10<<20)
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "x"},
	}
	out, injected := InjectRAGWithLimit(msgs, string(large), 0)
	if !injected {
		t.Fatal("injected = false, want true when guard disabled")
	}
	got, _ := out[0].(map[string]interface{})["content"].(string)
	if len(got) != 1+len(large) {
		t.Errorf("content len = %d, want %d", len(got), 1+len(large))
	}
}

// TestInjectRAGWithLimit_NoUserMessage returns the slice untouched with
// injected=false when there is nothing to append onto.
func TestInjectRAGWithLimit_NoUserMessage(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
	}
	out, injected := InjectRAGWithLimit(msgs, " CTX", 1<<20)
	if injected {
		t.Fatal("injected = true, want false with no user message")
	}
	if len(out) != len(msgs) {
		t.Errorf("len = %d, want %d", len(out), len(msgs))
	}
}

// --- BuildConversationContext tests (issue #1147) ---

func TestBuildConversationContext_DisabledWhenTurnsZero(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "hi"},
	}
	got := BuildConversationContext(msgs, 0, 2000)
	if got != "" {
		t.Errorf("turns=0 should disable; got %q", got)
	}
}

func TestBuildConversationContext_DisabledWhenCharsZero(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
	}
	got := BuildConversationContext(msgs, 3, 0)
	if got != "" {
		t.Errorf("maxChars=0 should disable; got %q", got)
	}
}

func TestBuildConversationContext_BasicWindow(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
		map[string]interface{}{"role": "user", "content": "review the architecture"},
		map[string]interface{}{"role": "assistant", "content": "here is my review"},
		map[string]interface{}{"role": "user", "content": "fix it"}, // latest, excluded
	}
	got := BuildConversationContext(msgs, 3, 2000)
	// Window is the 3 messages before the latest user msg.
	want := "user: review the architecture\nassistant: here is my review"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildConversationContext_TurnCapTruncation(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "m1"},
		map[string]interface{}{"role": "assistant", "content": "a1"},
		map[string]interface{}{"role": "user", "content": "m2"},
		map[string]interface{}{"role": "assistant", "content": "a2"},
		map[string]interface{}{"role": "user", "content": "latest"},
	}
	// Only 2 turns before the latest user message.
	got := BuildConversationContext(msgs, 2, 2000)
	want := "user: m2\nassistant: a2"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildConversationContext_TurnCapCappedAtTen(t *testing.T) {
	msgs := make([]interface{}, 20)
	for i := range msgs {
		msgs[i] = map[string]interface{}{"role": "user", "content": "m"}
	}
	// turns=100 should be capped at 10; window is 10 messages before the last.
	got := BuildConversationContext(msgs, 100, 10000)
	count := 0
	for _, c := range got {
		if c == '\n' {
			count++
		}
	}
	// 10 lines → 9 newlines.
	if count != 9 {
		t.Errorf("expected 10 lines (9 newlines), got %d newlines", count)
	}
}

func TestBuildConversationContext_CharCapTruncation(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "abcdefghij"}, // 10 chars
		map[string]interface{}{"role": "assistant", "content": "xyz"},
		map[string]interface{}{"role": "user", "content": "latest"},
	}
	// Cap at 15 chars. "user: abcdefghij" = 16 chars (with "user: " prefix).
	// Only the first 15 fit.
	got := BuildConversationContext(msgs, 3, 15)
	if len(got) > 15 {
		t.Errorf("result len = %d, want <= 15", len(got))
	}
	if got != "user: abcdefghij"[:15] {
		t.Errorf("got %q (len %d)", got, len(got))
	}
}

func TestBuildConversationContext_NoUserMessage(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
		map[string]interface{}{"role": "assistant", "content": "hi"},
	}
	// No user message → window is the last `turns` messages.
	// System messages are skipped, so only the assistant turn remains.
	got := BuildConversationContext(msgs, 3, 2000)
	want := "assistant: hi"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildConversationContext_SkipsNonMapEntries(t *testing.T) {
	msgs := []interface{}{
		"raw string",
		map[string]interface{}{"role": "user", "content": "first"},
		map[string]interface{}{"content": "no role"}, // role missing
		map[string]interface{}{"role": "user", "content": "latest"},
	}
	got := BuildConversationContext(msgs, 5, 2000)
	// "raw string" is not a map → skipped. "no role" has empty role → skipped.
	want := "user: first"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
