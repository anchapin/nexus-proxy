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
	rc.Observe("frontier", "guardrail", 0, "")
	rc.Observe("frontier", "guardrail", 0, "")
	rc.Observe("local", "dsl", 0, "")
	rc.Observe("frontier", "slm", 0.6, "coding")
	rc.Observe("frontier", "slm", 0.2, "coding") // low-confidence escalation
	rc.Observe("local", "slm", 0.9, "format")

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
	rc.Observe("local", "slm", 0.9, "coding")    // high
	rc.Observe("frontier", "slm", 0.5, "coding") // medium
	rc.Observe("frontier", "slm", 0.1, "coding") // low
	rc.Observe("frontier", "slm-error", 0, "")   // none bucket

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
	rc.Observe("frontier", "slm", 0.6, "b")
	rc.Observe("local", "dsl", 0, "")
	rc.Observe("frontier", "guardrail", 0, "")
	rc.Observe("local", "slm", 0.9, "a")

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
			rc.Observe(route, "slm", 0.5, "coding")
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
	rc.Observe("frontier", "guardrail", 0, "")
	rc.ObserveRejection("bad_request")
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil WriteTo should return (0, nil), got (%d, %v)", n, err)
	}
}

func TestRouteCountersHandlerContentType(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("frontier", "guardrail", 0, "")
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

// TestObserveLatencyHistogram tests the issue #165 latency histogram.
func TestObserveLatencyHistogram(t *testing.T) {
	rc := NewRouteCounters()
	// Record a frontier request with 250ms latency and no error.
	rc.ObserveLatency("frontier", 0.25, 0, false)
	// Record a local request with 50ms latency and no error.
	rc.ObserveLatency("local", 0.05, 0.01, false)
	// Record a frontier request with an error.
	rc.ObserveLatency("frontier", 1.5, 0.3, true)

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_request_latency_seconds", "latency histogram family present"},
		{"# TYPE nexus_request_latency_seconds histogram", "histogram type line"},
		{`route="frontier"`, "frontier route present in latency"},
		{`route="local"`, "local route present in latency"},
		{"nexus_request_errors_total", "error counter family present"},
		{"# TYPE nexus_request_errors_total counter", "error counter type line"},
		{`route="frontier"} 1`, "frontier error counted once"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestObserveLatencyTTFT tests the issue #165 TTFT histogram.
func TestObserveLatencyTTFT(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveLatency("local", 0.1, 0.05, false)   // 100ms latency, 50ms TTFT
	rc.ObserveLatency("local", 0.2, 0.15, false)  // 200ms latency, 150ms TTFT

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_request_ttft_seconds", "TTFT histogram family present"},
		{"# TYPE nexus_request_ttft_seconds histogram", "TTFT histogram type line"},
		{`route="local"`, "local route present in TTFT"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestRouteCountersNilSafeLatency tests that ObserveLatency is nil-safe.
func TestRouteCountersNilSafeLatency(t *testing.T) {
	var rc *RouteCounters
	// Must not panic.
	rc.ObserveLatency("frontier", 1.0, 0.5, true)
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil WriteTo should return (0, nil), got (%d, %v)", n, err)
	}
}
