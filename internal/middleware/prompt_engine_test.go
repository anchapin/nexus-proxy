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
