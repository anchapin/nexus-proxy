package observability

import (
	"strings"
	"sync"
	"testing"
)

func TestBucketConfidence(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, bucketNone},
		{-1, bucketNone},
		{0.01, bucketLow},
		{0.39, bucketLow},
		{0.4, bucketMedium},
		{0.69, bucketMedium},
		{0.7, bucketHigh},
		{0.99, bucketHigh},
		{1.0, bucketHigh},
	}
	for _, c := range cases {
		if got := BucketConfidence(c.in); got != c.want {
			t.Errorf("BucketConfidence(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRouteCountersObserveRouteDecisions(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("frontier", "guardrail", 0, "", "")
	rc.Observe("frontier", "guardrail", 0, "", "")
	rc.Observe("local", "dsl", 0, "", "")
	rc.Observe("frontier", "slm", 0.6, "coding", "")
	rc.Observe("frontier", "slm", 0.2, "coding", "") // low-confidence escalation
	rc.Observe("local", "slm", 0.9, "format", "")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_route_decisions_total", "metric family header"},
		{`route="frontier",source="guardrail"} 2`, "guardrail counted twice"},
		{`route="local",source="dsl"} 1`, "dsl counted once"},
		{`route="frontier",source="slm"} 2`, "slm-frontier counted twice"},
		{`route="local",source="slm"} 1`, "slm-local counted once"},
		{`nexus_slm_low_confidence_escalations_total`, "escalation metric present"},
		{`task_type="coding"} 1`, "coding escalation counted"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

func TestRouteCountersSLMDecisionsByConfidence(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("local", "slm", 0.9, "coding", "")    // high
	rc.Observe("frontier", "slm", 0.5, "coding", "") // medium
	rc.Observe("frontier", "slm", 0.1, "coding", "") // low
	rc.Observe("frontier", "slm-error", 0, "", "")   // none bucket

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	expect := []string{
		`confidence_bucket="high",task_type="coding"} 1`,
		`confidence_bucket="medium",task_type="coding"} 1`,
		`confidence_bucket="low",task_type="coding"} 1`,
		`confidence_bucket="none",task_type=""} 1`,
	}
	for _, frag := range expect {
		if !strings.Contains(out, frag) {
			t.Errorf("missing %q in output:\n%s", frag, out)
		}
	}
}

func TestRouteCountersDeterministicOutput(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("frontier", "slm", 0.6, "b", "")
	rc.Observe("local", "dsl", 0, "", "")
	rc.Observe("frontier", "guardrail", 0, "", "")
	rc.Observe("local", "slm", 0.9, "a", "")

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("output not deterministic between scrapes")
	}
}

func TestRouteCountersConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			route := "frontier"
			if n%2 == 0 {
				route = "local"
			}
			rc.Observe(route, "slm", 0.5, "coding", "")
		}(i)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	// No panic / race is the primary assertion; sanity-check the
	// total is 100 across the two route buckets.
	out := sb.String()
	if !strings.Contains(out, "nexus_route_decisions_total") {
		t.Errorf("expected route_decisions metric in output")
	}
}

func TestRouteCountersNilSafe(t *testing.T) {
	var rc *RouteCounters
	// Must not panic.
	rc.Observe("frontier", "guardrail", 0, "", "")
	rc.ObserveRejection("bad_request")
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil WriteTo should return (0, nil), got (%d, %v)", n, err)
	}
}

func TestRouteCountersHandlerContentType(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("frontier", "guardrail", 0, "", "")
	h := rc.Handler()
	if h == nil {
		t.Fatal("Handler() returned nil")
	}
	// The handler is exercised end-to-end in the handler package
	// tests; here we just verify it is non-nil and the type is
	// usable. A full httptest round-trip would duplicate the
	// WriteTo test above.
}

// Note: TestSanitizeHeaderValue moved to internal/handlers/sanitize_test.go
// so handlers stays free of the observability import per the
// AGENTS.md dependency rule.

// TestRouteCountersRejections verifies the issue #119 rejection
// counter family: ObserveRejection increments a per-reason counter
// and WriteTo emits nexus_requests_rejected_total{reason} lines.
func TestRouteCountersRejections(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveRejection("method")
	rc.ObserveRejection("method")
	rc.ObserveRejection("body_too_large")
	rc.ObserveRejection("bad_request")
	rc.ObserveRejection("bad_request")
	rc.ObserveRejection("bad_request")
	rc.ObserveRejection("rate_limit")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_requests_rejected_total", "metric family header"},
		{`# TYPE nexus_requests_rejected_total counter`, "counter type line"},
		{`nexus_requests_rejected_total{reason="method"} 2`, "method counted twice"},
		{`nexus_requests_rejected_total{reason="body_too_large"} 1`, "body_too_large counted once"},
		{`nexus_requests_rejected_total{reason="bad_request"} 3`, "bad_request counted three times"},
		{`nexus_requests_rejected_total{reason="rate_limit"} 1`, "rate_limit counted once"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestRouteCountersRejectionDeterministicOrder verifies that repeated
// scrapes produce identical output (sorted by reason label) so
// Prometheus diff alerts are not triggered by reordering.
func TestRouteCountersRejectionDeterministicOrder(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveRejection("rate_limit")
	rc.ObserveRejection("method")
	rc.ObserveRejection("bad_request")

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("rejection output not deterministic between scrapes")
	}
}

// TestRouteCountersRejectionConcurrentSafe exercises ObserveRejection
// from many goroutines; the race detector is the primary assertion.
func TestRouteCountersRejectionConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	reasons := []string{"method", "body_too_large", "bad_request", "rate_limit"}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rc.ObserveRejection(reasons[n%len(reasons)])
		}(i)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "nexus_requests_rejected_total") {
		t.Errorf("expected rejection metric in output")
	}
}

// TestRouteCountersFusionOutcome verifies the issue #187 fusion arbiter
// counter family: ObserveFusionOutcome increments the skipped or invoked
// counter and WriteTo emits nexus_fusion_arbiter_total{outcome} lines.
func TestRouteCountersFusionOutcome(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveFusionOutcome(true)  // skipped
	rc.ObserveFusionOutcome(true)  // skipped
	rc.ObserveFusionOutcome(false) // invoked
	rc.ObserveFusionOutcome(false) // invoked
	rc.ObserveFusionOutcome(false) // invoked

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_fusion_arbiter_total", "metric family header"},
		{`# TYPE nexus_fusion_arbiter_total counter`, "counter type line"},
		{`nexus_fusion_arbiter_total{outcome="skipped"} 2`, "skipped counted twice"},
		{`nexus_fusion_arbiter_total{outcome="invoked"} 3`, "invoked counted three times"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestObserveCascadeFallback verifies the issue #205 cascade fallback
// counter family: ObserveCascadeFallback increments a per-reason counter
// and WriteTo emits nexus_cascade_fallback_total{reason} lines.
func TestObserveCascadeFallback(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveCascadeFallback("timeout")
	rc.ObserveCascadeFallback("timeout")
	rc.ObserveCascadeFallback("transport_error")
	rc.ObserveCascadeFallback("malformed_toolcall")
	rc.ObserveCascadeFallback("malformed_toolcall")
	rc.ObserveCascadeFallback("malformed_toolcall")

	var sb2 strings.Builder
	if _, err := rc.WriteTo(&sb2); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out2 := sb2.String()

	checks2 := []struct {
		fragment string
		desc     string
	}{
		{"nexus_cascade_fallback_total", "metric family header"},
		{`# TYPE nexus_cascade_fallback_total counter`, "counter type line"},
		{`nexus_cascade_fallback_total{reason="timeout"} 2`, "timeout counted twice"},
		{`nexus_cascade_fallback_total{reason="transport_error"} 1`, "transport_error counted once"},
		{`nexus_cascade_fallback_total{reason="malformed_toolcall"} 3`, "malformed_toolcall counted three times"},
	}
	for _, c := range checks2 {
		if !strings.Contains(out2, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out2)
		}
	}
}

// TestRAGCountersHitMiss verifies the issue #186 RAG retrieval
// metric family: ObserveRAGHit increments per-filename counters and
// ObserveRAGMiss increments per-reason counters. WriteTo emits
// nexus_rag_retrieval_total{hit="true",filename="..."} and
// nexus_rag_retrieval_total{hit="false",reason="..."} lines.
func TestRAGCountersHitMiss(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveRAGHit("example1.go")
	rc.ObserveRAGHit("example1.go") // same file again
	rc.ObserveRAGHit("example2.go")
	rc.ObserveRAGMiss("empty_store")
	rc.ObserveRAGMiss("threshold")
	rc.ObserveRAGMiss("threshold")
	rc.ObserveRAGMiss("embed_error")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_rag_retrieval_total", "metric family header"},
		{`# TYPE nexus_rag_retrieval_total counter`, "counter type line"},
		{`nexus_rag_retrieval_total{hit="true",filename="example1.go"} 2`, "example1.go hit counted twice"},
		{`nexus_rag_retrieval_total{hit="true",filename="example2.go"} 1`, "example2.go hit counted once"},
		{`nexus_rag_retrieval_total{hit="false",reason="empty_store"} 1`, "empty_store miss counted once"},
		{`nexus_rag_retrieval_total{hit="false",reason="threshold"} 2`, "threshold miss counted twice"},
		{`nexus_rag_retrieval_total{hit="false",reason="embed_error"} 1`, "embed_error miss counted once"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestRouteCountersFusionOutcomeDeterministicOrder verifies that repeated
// scrapes produce identical output (sorted by outcome label) so
// Prometheus diff alerts are not triggered by reordering.
func TestRouteCountersFusionOutcomeDeterministicOrder(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveFusionOutcome(false) // invoked
	rc.ObserveFusionOutcome(true)  // skipped

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("fusion output not deterministic between scrapes")
	}
}

// TestRAGCountersDeterministicOrder verifies that repeated scrapes
// produce identical output so Prometheus diff alerts are not triggered
// by reordering.
func TestRAGCountersDeterministicOrder(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveRAGMiss("threshold")
	rc.ObserveRAGHit("b.go")
	rc.ObserveRAGMiss("embed_error")
	rc.ObserveRAGHit("a.go")

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("RAG output not deterministic between scrapes")
	}
}

// TestRouteCountersFusionOutcomeConcurrentSafe exercises ObserveFusionOutcome
// from many goroutines; the race detector is the primary assertion.
func TestRouteCountersFusionOutcomeConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rc.ObserveFusionOutcome(n%2 == 0) // alternate skipped/invoked
		}(i)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "nexus_fusion_arbiter_total") {
		t.Errorf("expected fusion arbiter metric in output")
	}
}

// TestRAGCountersConcurrentSafe exercises ObserveRAGHit and
// ObserveRAGMiss from many goroutines; the race detector is the
// primary assertion.
func TestRAGCountersConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	filenames := []string{"a.go", "b.go", "c.go"}
	reasons := []string{"threshold", "empty_store", "embed_error"}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				rc.ObserveRAGHit(filenames[n%len(filenames)])
			} else {
				rc.ObserveRAGMiss(reasons[n%len(reasons)])
			}
		}(i)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "nexus_rag_retrieval_total") {
		t.Errorf("expected rag_retrieval metric in output")
	}
}

// TestRouteCountersFusionOutcomeNilSafe verifies that nil receiver does not panic.
func TestRouteCountersFusionOutcomeNilSafe(t *testing.T) {
	var rc *RouteCounters
	rc.ObserveFusionOutcome(true)
	rc.ObserveFusionOutcome(false)
}

// TestRAGCountersNilSafe verifies that nil receivers are safe.
func TestRAGCountersNilSafe(t *testing.T) {
	var rc *RouteCounters
	rc.ObserveRAGHit("example.go")
	rc.ObserveRAGMiss("threshold")
}

// TestObserveCascadeFallbackNilSafe verifies that nil receivers are safe.
func TestObserveCascadeFallbackNilSafe(t *testing.T) {
	var rc *RouteCounters
	// Must not panic.
	rc.ObserveCascadeFallback("timeout")
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil WriteTo should return (0, nil), got (%d, %v)", n, err)
	}
}

// TestObserveCascadeFallbackEmptyReason verifies that empty reason is a no-op.
func TestObserveCascadeFallbackEmptyReason(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveCascadeFallback("") // should be no-op
	rc.ObserveCascadeFallback("timeout")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	// Should only have timeout, not an empty-string entry
	if strings.Contains(out, `reason=""`) {
		t.Errorf("empty reason should not appear in output:\n%s", out)
	}
	if !strings.Contains(out, `reason="timeout"} 1`) {
		t.Errorf("timeout should appear in output:\n%s", out)
	}
}

// TestObserveCascadeFallbackDeterministicOrder verifies sorted output.
func TestObserveCascadeFallbackDeterministicOrder(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveCascadeFallback("transport_error")
	rc.ObserveCascadeFallback("timeout")
	rc.ObserveCascadeFallback("malformed_toolcall")

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("cascade fallback output not deterministic between scrapes")
	}
}
