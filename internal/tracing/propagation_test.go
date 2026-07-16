package tracing

import (
	"context"
	"testing"
)

// TestPropagationRoundTrip is the canonical W3C interop test:
// every traceparent produced by FormatTraceparent must parse via
// ParseTraceparent, and every InjectTraceparent result must round
// trip through ParseTraceparent.
func TestPropagationRoundTrip(t *testing.T) {
	cases := []struct {
		traceID string
		spanID  string
	}{
		{"0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"},
		{"deadbeefdeadbeefdeadbeefdeadbeef", "cafebabecafebabe"},
		{"1234567890abcdef1234567890abcdef", "fedcba0987654321"},
	}
	for _, tc := range cases {
		hdr := InjectTraceparent(Context{TraceID: tc.traceID, SpanID: tc.spanID})
		if hdr == "" {
			t.Fatalf("InjectTraceparent empty for %+v", tc)
		}
		gotTrace, gotSpan, ok := ParseTraceparent(hdr)
		if !ok {
			t.Fatalf("ParseTraceparent(%q) rejected injected header", hdr)
		}
		if gotTrace != tc.traceID || gotSpan != tc.spanID {
			t.Errorf("round trip mismatch: got %q/%q, want %q/%q",
				gotTrace, gotSpan, tc.traceID, tc.spanID)
		}
	}
}

// TestPropagationEmpty makes sure the inject path stays a no-op
// (returns "") when the context is incomplete. Downstream code
// tests for "" to decide whether to set the header at all.
func TestPropagationEmpty(t *testing.T) {
	cases := []Context{
		{},
		{TraceID: "0af7651916cd43dd8448eb211c80319c"}, // no span
		{SpanID: "b7ad6b7169203331"},                  // no trace
	}
	for i, c := range cases {
		if got := InjectTraceparent(c); got != "" {
			t.Errorf("case %d: InjectTraceparent = %q, want empty", i, got)
		}
	}
}

// TestTraceparentFromContext tests the outbound-context helper used by
// the upstream package to propagate trace context (issue #299).
func TestTraceparentFromContext(t *testing.T) {
	// No tracing context at all — returns "".
	if got := TraceparentFromContext(context.Background()); got != "" {
		t.Errorf("no context: got %q, want empty", got)
	}

	// Context with valid tracing Context — returns formatted header.
	tc := Context{
		TraceID: "0af7651916cd43dd8448eb211c80319c",
		SpanID:  "b7ad6b7169203331",
	}
	ctx := WithSpanContext(context.Background(), tc)
	got := TraceparentFromContext(ctx)
	if got == "" {
		t.Fatal("expected non-empty traceparent")
	}
	// Verify it round-trips.
	traceID, spanID, ok := ParseTraceparent(got)
	if !ok {
		t.Fatalf("TraceparentFromContext returned unparseable header: %q", got)
	}
	if traceID != tc.TraceID || spanID != tc.SpanID {
		t.Errorf("round trip mismatch: got %q/%q, want %q/%q",
			traceID, spanID, tc.TraceID, tc.SpanID)
	}
}
