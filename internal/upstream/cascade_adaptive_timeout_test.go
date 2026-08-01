package upstream

import (
	"testing"
	"time"
)

// makePayload builds a chat-completion payload with a single user message
// of the given textual content.
func makePayload(content string) map[string]interface{} {
	return map[string]interface{}{
		"model": "test-model",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": content,
			},
		},
	}
}

// repeat builds a string of n copies of s.
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestEffectiveTimeoutAdaptiveDisabledUsesFixed(t *testing.T) {
	c := &Cascade{
		Timeout:            42 * time.Second,
		TimeoutPer1kTokens: 0, // disabled
	}
	got := c.effectiveTimeout(makePayload("anything"))
	if got != 42*time.Second {
		t.Errorf("got %v, want 42s (fixed Timeout when adaptive disabled)", got)
	}
}

func TestEffectiveTimeoutAdaptiveDisabledFallsBackToDefault(t *testing.T) {
	c := &Cascade{
		Timeout:            0, // unset
		TimeoutPer1kTokens: 0, // disabled
	}
	got := c.effectiveTimeout(makePayload("anything"))
	if got != cascadeDefaultTimeout {
		t.Errorf("got %v, want cascadeDefaultTimeout %v", got, cascadeDefaultTimeout)
	}
}

func TestEffectiveTimeoutClampsToFloor(t *testing.T) {
	floor := 5 * time.Second
	c := &Cascade{
		TimeoutFloor:       floor,
		TimeoutCeiling:     120 * time.Second,
		TimeoutPer1kTokens: 1500 * time.Millisecond,
	}
	// A near-empty prompt should sit at or just above the floor (the
	// message separator adds ~1 token, yielding a sub-millisecond
	// increment that is well within tolerance).
	got := c.effectiveTimeout(makePayload(""))
	if got < floor || got > floor+50*time.Millisecond {
		t.Errorf("got %v, want ~floor %v for empty prompt", got, floor)
	}
}

func TestEffectiveTimeoutClampsToCeiling(t *testing.T) {
	ceiling := 120 * time.Second
	c := &Cascade{
		TimeoutFloor:       5 * time.Second,
		TimeoutCeiling:     ceiling,
		TimeoutPer1kTokens: 1500 * time.Millisecond,
	}
	// A huge prompt should clamp to the ceiling. Use a large string so
	// the token estimate is well above what per1k * tokens/1000 can reach.
	huge := repeat("word ", 200000) // ~250k tokens
	got := c.effectiveTimeout(makePayload(huge))
	if got != ceiling {
		t.Errorf("got %v, want ceiling %v for huge prompt", got, ceiling)
	}
}

// TestEffectiveTimeoutMonotonicAndClamped covers acceptance criterion #1:
// a 1k-token prompt yields a timeout within [floor, ceiling] and strictly
// less than the 32k-token case.
func TestEffectiveTimeoutMonotonicAndClamped(t *testing.T) {
	floor := 5 * time.Second
	ceiling := 120 * time.Second
	per1k := 1500 * time.Millisecond
	c := &Cascade{
		TimeoutFloor:       floor,
		TimeoutCeiling:     ceiling,
		TimeoutPer1kTokens: per1k,
	}

	// Build prompts whose token counts we control via estimatePromptTokens.
	// Use ~1000 and ~32000 tokens worth of text. estimatePromptTokens uses
	// the real tokenizer so we measure the actual counts.
	small := repeat("a ", 1000)  // ~1000 tokens
	large := repeat("a ", 32000) // ~32000 tokens

	smallTokens := estimatePromptTokens(makePayload(small))
	largeTokens := estimatePromptTokens(makePayload(large))

	smallTimeout := c.effectiveTimeout(makePayload(small))
	largeTimeout := c.effectiveTimeout(makePayload(large))

	if smallTimeout < floor {
		t.Errorf("small timeout %v < floor %v", smallTimeout, floor)
	}
	if smallTimeout > ceiling {
		t.Errorf("small timeout %v > ceiling %v", smallTimeout, ceiling)
	}
	if largeTimeout < floor {
		t.Errorf("large timeout %v < floor %v", largeTimeout, floor)
	}
	if largeTimeout > ceiling {
		t.Errorf("large timeout %v > ceiling %v", largeTimeout, ceiling)
	}
	if smallTimeout >= largeTimeout {
		t.Errorf("expected small (%v, %d tokens) < large (%v, %d tokens)",
			smallTimeout, smallTokens, largeTimeout, largeTokens)
	}

	// Verify the formula directly for the small case.
	wantSmall := floor + time.Duration(int64(per1k)*int64(smallTokens)/1000)
	if wantSmall > ceiling {
		wantSmall = ceiling
	}
	if smallTimeout != wantSmall {
		t.Errorf("small timeout %v, want formula %v (tokens=%d)", smallTimeout, wantSmall, smallTokens)
	}
}

// TestEffectiveTimeoutBackwardCompatZeroPer1k covers acceptance criterion #2:
// with PER_1K_TOKENS=0 the effective timeout equals the legacy fixed
// NEXUS_CASCADE_TIMEOUT.
func TestEffectiveTimeoutBackwardCompatZeroPer1k(t *testing.T) {
	legacy := 30 * time.Second
	c := &Cascade{
		Timeout:            legacy,
		TimeoutFloor:       5 * time.Second,
		TimeoutCeiling:     120 * time.Second,
		TimeoutPer1kTokens: 0, // disabled
	}
	got := c.effectiveTimeout(makePayload(repeat("word ", 50000)))
	if got != legacy {
		t.Errorf("got %v, want legacy fixed %v when PER_1K_TOKENS=0", got, legacy)
	}
}

func TestEffectiveTimeoutDefaultConstantsWhenUnset(t *testing.T) {
	// All adaptive fields zero — should use default floor/ceiling/per1k.
	c := &Cascade{TimeoutPer1kTokens: -1} // negative = disabled, but floor path skipped
	// When per1k <= 0 it's disabled regardless of sign, so falls to fixed.
	got := c.effectiveTimeout(makePayload("hi"))
	if got != cascadeDefaultTimeout {
		t.Errorf("got %v, want default fixed %v", got, cascadeDefaultTimeout)
	}
}

func TestEffectiveTimeoutDefaultFloorCeilingWhenEnabledButUnset(t *testing.T) {
	// per1k enabled but floor/ceiling unset → defaults kick in.
	c := &Cascade{TimeoutPer1kTokens: 1500 * time.Millisecond}
	got := c.effectiveTimeout(makePayload(""))
	// Empty prompt → at or just above default floor (message separator
	// adds ~1 token → sub-millisecond increment within tolerance).
	if got < cascadeDefaultFloor || got > cascadeDefaultFloor+50*time.Millisecond {
		t.Errorf("got %v, want ~cascadeDefaultFloor %v", got, cascadeDefaultFloor)
	}
}

func TestEstimatePromptTokensStringContent(t *testing.T) {
	payload := makePayload("hello world this is a test")
	n := estimatePromptTokens(payload)
	if n <= 0 {
		t.Errorf("expected positive token count, got %d", n)
	}
}

func TestEstimatePromptTokensMultipartContent(t *testing.T) {
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "describe this image"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]string{"url": "http://x/y.png"}},
				},
			},
		},
	}
	n := estimatePromptTokens(payload)
	if n <= 0 {
		t.Errorf("expected positive token count for multipart, got %d", n)
	}
}

func TestEstimatePromptTokensNoMessages(t *testing.T) {
	payload := map[string]interface{}{"model": "x"}
	n := estimatePromptTokens(payload)
	if n < 0 {
		t.Errorf("expected non-negative fallback, got %d", n)
	}
}

// TestBuildLocalCascadeThreadsAdaptiveFields verifies BuildLocalCascade
// copies the adaptive fields into the resulting Cascade.
func TestBuildLocalCascadeThreadsAdaptiveFields(t *testing.T) {
	cas := BuildLocalCascade(CascadeConfig{
		LocalURL:           "http://local",
		LocalModel:         "m",
		FrontierURL:        "http://frontier",
		FrontierModel:      "fm",
		FrontierKey:        "sk",
		Timeout:            30 * time.Second,
		TimeoutFloor:       5 * time.Second,
		TimeoutCeiling:     120 * time.Second,
		TimeoutPer1kTokens: 1500 * time.Millisecond,
	})
	if cas.TimeoutFloor != 5*time.Second {
		t.Errorf("TimeoutFloor = %v", cas.TimeoutFloor)
	}
	if cas.TimeoutCeiling != 120*time.Second {
		t.Errorf("TimeoutCeiling = %v", cas.TimeoutCeiling)
	}
	if cas.TimeoutPer1kTokens != 1500*time.Millisecond {
		t.Errorf("TimeoutPer1kTokens = %v", cas.TimeoutPer1kTokens)
	}
}
