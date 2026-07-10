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
