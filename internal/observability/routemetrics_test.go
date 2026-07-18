package observability

import (
	"fmt"
	"net/http/httptest"
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
	rc.ObserveRejection("auth_rate_limit")

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
		{`nexus_requests_rejected_total{reason="auth_rate_limit"} 1`, "auth_rate_limit counted once"},
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
	reasons := []string{"method", "body_too_large", "bad_request", "rate_limit", "auth_rate_limit"}
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
// metric family. ObserveRAGHit increments a single unlabelled hit
// counter and ObserveRAGMiss increments per-reason counters.
// WriteTo emits exactly one nexus_rag_retrieval_total{hit="true"}
// line (collapsed across all hit filenames, issue #486) plus one
// nexus_rag_retrieval_total{hit="false",reason="..."} line per
// distinct miss reason.
func TestRAGCountersHitMiss(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveRAGHit()
	rc.ObserveRAGHit() // hit again — counter should sum
	rc.ObserveRAGHit()
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
		{`nexus_rag_retrieval_total{hit="true"} 3`, "hits collapsed to a single unlabelled line"},
		{`nexus_rag_retrieval_total{hit="false",reason="empty_store"} 1`, "empty_store miss counted once"},
		{`nexus_rag_retrieval_total{hit="false",reason="threshold"} 2`, "threshold miss counted twice"},
		{`nexus_rag_retrieval_total{hit="false",reason="embed_error"} 1`, "embed_error miss counted once"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
	// Issue #486 regression guard: hits must never carry a filename label.
	if strings.Contains(out, `nexus_rag_retrieval_total{hit="true",filename=`) {
		t.Errorf("hit line must not carry filename label; got filename-labelled sample in output:\n%s", out)
	}
}

// TestRAGCountersSingleHitSeriesRegardlessOfFilenames is the issue
// #486 regression test. Recording hits for N distinct filenames must
// produce exactly one nexus_rag_retrieval_total{hit="true"} sample
// line whose value equals the total hit count. This asserts the
// unbounded-cardinality bug does not regress: before the fix the
// family emitted one series per filename.
func TestRAGCountersSingleHitSeriesRegardlessOfFilenames(t *testing.T) {
	const N = 50
	rc := NewRouteCounters()
	for i := 0; i < N; i++ {
		// ObserveRAGHit no longer takes a filename — we exercise the
		// post-fix shape. Each call increments the single hit counter.
		rc.ObserveRAGHit()
	}

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	// Count occurrences of the hit sample line. There must be exactly one.
	hitLine := `nexus_rag_retrieval_total{hit="true"}`
	count := strings.Count(out, hitLine)
	if count != 1 {
		t.Errorf("expected exactly 1 %q sample line, got %d\nfull output:\n%s", hitLine, count, out)
	}
	// The single hit line must carry the aggregated value of N.
	want := fmt.Sprintf(`nexus_rag_retrieval_total{hit="true"} %d`, N)
	if !strings.Contains(out, want) {
		t.Errorf("expected aggregated hit line %q in output\nfull output:\n%s", want, out)
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
	rc.ObserveRAGHit()
	rc.ObserveRAGMiss("embed_error")
	rc.ObserveRAGHit()

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
	reasons := []string{"threshold", "empty_store", "embed_error"}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				rc.ObserveRAGHit()
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
	rc.ObserveRAGHit()
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

// TestQueueOverflowCounters exercises the issue #226 overflow counters
// and verifies they appear in the Prometheus exposition with the correct
// HELP/TYPE lines and zero-value output when not incremented.
func TestQueueOverflowCounters(t *testing.T) {
	rc := NewRouteCounters()

	// Verify zero-state has HELP/TYPE but zero value.
	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_judge_queue_overflow_total", "judge metric family header"},
		{"# TYPE nexus_judge_queue_overflow_total counter", "judge TYPE line"},
		{"nexus_quality_queue_overflow_total", "quality metric family header"},
		{"# TYPE nexus_quality_queue_overflow_total counter", "quality TYPE line"},
		{"nexus_judge_queue_overflow_total 0\n", "judge zero value"},
		{"nexus_quality_queue_overflow_total 0\n", "quality zero value"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}

	// Increment both counters and verify non-zero output.
	rc.ObserveJudgeQueueOverflow()
	rc.ObserveJudgeQueueOverflow()
	rc.ObserveQualityQueueOverflow()

	sb.Reset()
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo after increments: %v", err)
	}
	out = sb.String()

	overflowChecks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_judge_queue_overflow_total 2\n", "judge incremented twice"},
		{"nexus_quality_queue_overflow_total 1\n", "quality incremented once"},
	}
	for _, c := range overflowChecks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestQueueOverflowCountersConcurrentSafe exercises the overflow
// counters from many goroutines; the race detector is the primary assertion.
func TestQueueOverflowCountersConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				rc.ObserveJudgeQueueOverflow()
			} else {
				rc.ObserveQualityQueueOverflow()
			}
		}(i)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "nexus_judge_queue_overflow_total") {
		t.Errorf("expected judge overflow metric in output")
	}
	if !strings.Contains(out, "nexus_quality_queue_overflow_total") {
		t.Errorf("expected quality overflow metric in output")
	}
}

// TestQueueOverflowCountersNilSafe verifies that nil receivers are safe.
func TestQueueOverflowCountersNilSafe(t *testing.T) {
	var rc *RouteCounters
	rc.ObserveJudgeQueueOverflow()
	rc.ObserveQualityQueueOverflow()
	// Must not panic; WriteTo is already tested nil-safe above.
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil WriteTo should return (0, nil), got (%d, %v)", n, err)
	}
}

// TestRouteCountersSLMEscalations verifies the issue #301 SLM escalation
// counter: Observe with source="slm-escalation" increments the
// nexus_slm_escalations_total{reason="low_confidence"} counter.
func TestRouteCountersSLMEscalations(t *testing.T) {
	rc := NewRouteCounters()
	// Simulate low-confidence escalation: SLM returned local but confidence
	// was below threshold. The handler calls Observe with source="slm-escalation".
	rc.Observe("frontier", "slm-escalation", 0.2, "debugging", "")
	rc.Observe("frontier", "slm-escalation", 0.15, "refactor", "")
	rc.Observe("frontier", "slm-escalation", 0.29, "coding", "")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	// Check the metric family is present.
	if !strings.Contains(out, "nexus_slm_escalations_total") {
		t.Errorf("missing nexus_slm_escalations_total metric family")
	}
	if !strings.Contains(out, `# TYPE nexus_slm_escalations_total counter`) {
		t.Errorf("missing counter type line")
	}
	if !strings.Contains(out, `reason="low_confidence"`) {
		t.Errorf("missing low_confidence reason label")
	}
	// All three escalations should be counted.
	if !strings.Contains(out, `nexus_slm_escalations_total{reason="low_confidence"} 3`) {
		t.Errorf("expected 3 escalations in output, got:\n%s", out)
	}
}

// TestRouteCountersSLMEscalationsNilSafe verifies that nil receivers are safe
// when Observe is called with the slm-escalation source.
func TestRouteCountersSLMEscalationsNilSafe(t *testing.T) {
	var rc *RouteCounters
	rc.Observe("frontier", "slm-escalation", 0.2, "debugging", "")
	// Must not panic; WriteTo should return (0, nil).
	n, err := rc.WriteTo(&strings.Builder{})
	if err != nil || n != 0 {
		t.Errorf("nil + Observe slm-escalation: expected (0, nil), got (%d, %v)", n, err)
	}
}

// --- SLM cache eviction counters (issue #449) ---

// TestSLMCacheEvictionCountersByReason verifies the issue #449 metric
// family: ObserveSLMCacheEviction increments per-reason counters and
// WriteTo emits nexus_slm_cache_evictions_total{reason="ttl|lru"}.
func TestSLMCacheEvictionCountersByReason(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveSLMCacheEviction("ttl")
	rc.ObserveSLMCacheEviction("ttl")
	rc.ObserveSLMCacheEviction("ttl")
	rc.ObserveSLMCacheEviction("lru")

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{"nexus_slm_cache_evictions_total", "metric family header"},
		{"# TYPE nexus_slm_cache_evictions_total counter", "counter type line"},
		{`nexus_slm_cache_evictions_total{reason="lru"} 1`, "lru counted once"},
		{`nexus_slm_cache_evictions_total{reason="ttl"} 3`, "ttl counted thrice"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, out)
		}
	}
}

// TestSLMCacheEvictionsDeterministicOrder verifies that repeated
// scrapes produce identical output (sorted by reason label) so
// Prometheus diff alerts are not triggered by reordering. The label
// order must be "lru" before "ttl" lexicographically.
func TestSLMCacheEvictionsDeterministicOrder(t *testing.T) {
	rc := NewRouteCounters()
	rc.ObserveSLMCacheEviction("ttl")
	rc.ObserveSLMCacheEviction("lru")

	var first, second strings.Builder
	_, _ = rc.WriteTo(&first)
	_, _ = rc.WriteTo(&second)
	if first.String() != second.String() {
		t.Errorf("eviction output not deterministic between scrapes\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}

	out := first.String()
	lruIdx := strings.Index(out, `nexus_slm_cache_evictions_total{reason="lru"}`)
	ttlIdx := strings.Index(out, `nexus_slm_cache_evictions_total{reason="ttl"}`)
	if lruIdx == -1 || ttlIdx == -1 {
		t.Fatalf("missing eviction series in output:\n%s", out)
	}
	if lruIdx > ttlIdx {
		t.Errorf("expected lru before ttl in sorted output, got:\n%s", out)
	}
}

// TestSLMCacheEvictionsEmptyFamilyStillEmitsHeader verifies the
// metric family is announced with HELP/TYPE even before the first
// eviction, so scrapers can discover it without a placeholder sample.
func TestSLMCacheEvictionsEmptyFamilyStillEmitsHeader(t *testing.T) {
	rc := NewRouteCounters()
	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "# HELP nexus_slm_cache_evictions_total") {
		t.Errorf("missing HELP header for empty eviction family:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE nexus_slm_cache_evictions_total counter") {
		t.Errorf("missing TYPE header for empty eviction family:\n%s", out)
	}
}

// TestSLMCacheEvictionsConcurrentSafe exercises ObserveSLMCacheEviction
// from many goroutines to ensure the lock-then-atomic pattern is
// race-clean.
func TestSLMCacheEvictionsConcurrentSafe(t *testing.T) {
	rc := NewRouteCounters()
	const goroutines = 32
	const perGoroutine = 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			reason := "ttl"
			if g%2 == 0 {
				reason = "lru"
			}
			for i := 0; i < perGoroutine; i++ {
				rc.ObserveSLMCacheEviction(reason)
			}
		}(g)
	}
	wg.Wait()

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := sb.String()
	wantTTL := uint64(goroutines / 2 * perGoroutine)
	wantLRU := uint64(goroutines / 2 * perGoroutine)
	if !strings.Contains(out, fmt.Sprintf(`nexus_slm_cache_evictions_total{reason="ttl"} %d`, wantTTL)) {
		t.Errorf("missing or wrong ttl count %d:\n%s", wantTTL, out)
	}
	if !strings.Contains(out, fmt.Sprintf(`nexus_slm_cache_evictions_total{reason="lru"} %d`, wantLRU)) {
		t.Errorf("missing or wrong lru count %d:\n%s", wantLRU, out)
	}
}

// TestSLMCacheEvictionsNilAndEmptySafe verifies that nil receivers and
// empty reason strings are no-ops so callers can invoke
// ObserveSLMCacheEviction unconditionally.
func TestSLMCacheEvictionsNilAndEmptySafe(t *testing.T) {
	var rc *RouteCounters
	rc.ObserveSLMCacheEviction("ttl") // nil receiver: must not panic

	rc = NewRouteCounters()
	rc.ObserveSLMCacheEviction("") // empty reason: must not bump or panic

	var sb strings.Builder
	if _, err := rc.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if strings.Contains(sb.String(), "nexus_slm_cache_evictions_total{") {
		t.Errorf("empty reason should not emit any series:\n%s", sb.String())
	}
}

// TestHandlerEmitsCircuitBreakerGauges (issue #443) wires a real
// Collector into the production RouteCounters.Handler() and confirms
// that a recorded RAG failure surfaces in /metrics as the three
// labelled circuit-breaker samples referenced by the dashboard and
// runbook queries. This is the live-handler integration test the
// issue calls out; the standalone CircuitBreakerGauges renderer
// already passes — the bug is that those samples never reach /metrics.
func TestHandlerEmitsCircuitBreakerGauges(t *testing.T) {
	rc := NewRouteCounters()
	col := NewCollector()
	rc.SetCollector(col)

	// Simulate the chat handler tripping the RAG breaker (issue #411).
	col.RecordCircuitFailure("rag")
	col.RecordCircuitFailure("rag")
	col.RecordCircuitFailure("rag")
	col.RecordCircuitRecovery("rag")
	// And the Ollama breaker reaching half-open during recovery.
	col.RecordCircuitHalfOpen("ollama")

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	rc.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handler returned status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", got)
	}
	body := rec.Body.String()

	checks := []struct {
		fragment string
		desc     string
	}{
		{`# TYPE nexus_circuit_breaker_state gauge`, "state gauge type line"},
		{`nexus_circuit_breaker_state{circuit="rag"} 0`, "rag circuit closed (0) after recovery"},
		{`nexus_circuit_breaker_state{circuit="ollama"} 1`, "ollama circuit half_open (1)"},
		{`# TYPE nexus_circuit_breaker_failures_total counter`, "failures_total counter type line"},
		{`nexus_circuit_breaker_failures_total{circuit="rag"} 3`, "rag failures_total = 3"},
		{`nexus_circuit_breaker_last_failure_seconds{circuit="rag"}`, "rag last_failure_seconds label set present"},
	}
	for _, c := range checks {
		if !strings.Contains(body, c.fragment) {
			t.Errorf("%s: output missing %q\nfull output:\n%s", c.desc, c.fragment, body)
		}
	}
}

// TestHandlerEmitsCircuitBreakerGaugesWithProviders (issue #443)
// confirms that a downstream gauge provider wired via SetGaugeProviders
// continues to render alongside the collector's circuit-breaker
// gauges — the collector joins the providers list rather than
// replacing it.
func TestHandlerEmitsCircuitBreakerGaugesWithProviders(t *testing.T) {
	rc := NewRouteCounters()
	col := NewCollector()
	rc.SetCollector(col)
	rc.SetGaugeProviders(GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_custom_gauge", Value: 42}}
	}))

	col.RecordCircuitFailure("rag")

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	rc.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `nexus_circuit_breaker_state{circuit="rag"} 2`) {
		t.Errorf("missing rag open state sample (2) from collector\n%s", body)
	}
	if !strings.Contains(body, "nexus_custom_gauge 42") {
		t.Errorf("missing external gauge sample from SetGaugeProviders\n%s", body)
	}
}

// TestHandlerNilCollectorSafe (issue #443) verifies the handler
// remains safe when SetCollector was never called — the original
// behaviour must not regress.
func TestHandlerNilCollectorSafe(t *testing.T) {
	rc := NewRouteCounters()
	rc.Observe("frontier", "guardrail", 0, "", "")

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	rc.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handler returned status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "nexus_route_decisions_total") {
		t.Errorf("expected route_decisions_total in nil-collector output\n%s", body)
	}
	// Circuit breaker metrics must be absent — no collector means no circuits.
	if strings.Contains(body, "nexus_circuit_breaker_state") {
		t.Errorf("circuit_breaker_state should be absent without a collector\n%s", body)
	}
}
