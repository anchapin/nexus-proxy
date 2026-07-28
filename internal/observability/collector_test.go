package observability

import (
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSubmitIncrementsRouteCounters verifies the acceptance criterion
// "5 test requests → nexus_requests_total incremented by 5 with correct
// route labels". Each route lands in its own labelled bucket.
func TestSubmitIncrementsRouteCounters(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 5; i++ {
		c.Submit(ObservabilityEvent{Route: "local"})
	}
	c.Submit(ObservabilityEvent{Route: "frontier"})
	c.Submit(ObservabilityEvent{Route: "frontier"})
	c.Submit(ObservabilityEvent{Route: "fusion"})

	if got := c.RequestsLocal(); got != 5 {
		t.Errorf("RequestsLocal = %d, want 5", got)
	}
	if got := c.RequestsFrontier(); got != 2 {
		t.Errorf("RequestsFrontier = %d, want 2", got)
	}
	if got := c.RequestsFusion(); got != 1 {
		t.Errorf("RequestsFusion = %d, want 1", got)
	}
}

// TestSubmitUnknownRouteCountsAsFrontier guards the safe-default
// behaviour: an unrecognised route string must not be silently
// dropped, it accumulates into the frontier bucket (the proxy's
// universal safe default).
func TestSubmitUnknownRouteCountsAsFrontier(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "supervisor"})
	if got := c.RequestsFrontier(); got != 1 {
		t.Errorf("unknown route counted as frontier = %d, want 1", got)
	}
}

// TestSubmitCountsErrorsRagToonDegraded checks the boolean-flag
// counters increment exactly when their flag is set.
func TestSubmitCountsErrorsRagToonDegraded(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", Error: "boom"})
	c.Submit(ObservabilityEvent{Route: "local", RAGInjected: true})
	c.Submit(ObservabilityEvent{Route: "local"}) // RAG miss, no flags
	c.Submit(ObservabilityEvent{Route: "local", TOONCompressed: true})
	c.Submit(ObservabilityEvent{Route: "local", Degraded: true})

	if got := c.errorsLocal.Load(); got != 1 {
		t.Errorf("errorsLocal = %d, want 1", got)
	}
	if got := c.errorsFrontier.Load(); got != 0 {
		t.Errorf("errorsFrontier = %d, want 0", got)
	}
	if got := c.errorsFusion.Load(); got != 0 {
		t.Errorf("errorsFusion = %d, want 0", got)
	}
	if got := c.ragHitsTotal.Load(); got != 1 {
		t.Errorf("ragHitsTotal = %d, want 1", got)
	}
	if got := c.ragMissesTotal.Load(); got != 4 {
		t.Errorf("ragMissesTotal = %d, want 4 (one hit + four unflagged)", got)
	}
	if got := c.toonCompressedTotal.Load(); got != 1 {
		t.Errorf("toonCompressedTotal = %d, want 1", got)
	}
	if got := c.degradedTotal.Load(); got != 1 {
		t.Errorf("degradedTotal = %d, want 1", got)
	}
}

// TestSubmitAccumulatesTokensAndCost verifies the cumulative-sum
// counters (tokens + cost) add across multiple submissions rather than
// being overwritten.
func TestSubmitAccumulatesTokensAndCost(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{
		Route:             "frontier",
		InputTokens:       1000,
		OutputTokens:      500,
		TOONSavingsTokens: 200,
		EstimatedCostUSD:  0.01,
	})
	c.Submit(ObservabilityEvent{
		Route:             "frontier",
		InputTokens:       250,
		OutputTokens:      50,
		TOONSavingsTokens: 30,
		EstimatedCostUSD:  0.005,
	})

	if got := c.inputTokensTotal.Load(); got != 1250 {
		t.Errorf("inputTokensTotal = %d, want 1250", got)
	}
	if got := c.outputTokensTotal.Load(); got != 550 {
		t.Errorf("outputTokensTotal = %d, want 550", got)
	}
	if got := c.toonSavingsTokensTotal.Load(); got != 230 {
		t.Errorf("toonSavingsTokensTotal = %d, want 230", got)
	}
	if got, want := c.EstimatedCostUSD(), 0.015; math.Abs(got-want) > 1e-9 {
		t.Errorf("EstimatedCostUSD = %v, want %v", got, want)
	}
}

// TestSubmitNegativeTokensIgnored guards against a negative estimate
// (which the savings heuristic clamps elsewhere) silently underflowing
// the uint64 counter.
func TestSubmitNegativeTokensIgnored(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", InputTokens: -5, OutputTokens: -1, TOONSavingsTokens: -10})
	if got := c.inputTokensTotal.Load(); got != 0 {
		t.Errorf("inputTokensTotal = %d, want 0 (negative ignored)", got)
	}
	if c.EstimatedCostUSD() != 0 {
		t.Errorf("EstimatedCostUSD = %v, want 0", c.EstimatedCostUSD())
	}
}

// TestHistogramBucketPlacement is the core histogram correctness test:
// each observation lands in the smallest bucket whose upper bound it
// fits under, the cumulative counts are monotonic, and the +Inf bucket
// equals the total count.
func TestHistogramBucketPlacement(t *testing.T) {
	h := NewHistogram([]float64{10, 50, 100})
	// Observations: 5 and 10 -> bucket[0] (<=10); 75 -> bucket[2] (<=100);
	// 999 -> +Inf bucket.
	h.Observe(5)
	h.Observe(10)
	h.Observe(75)
	h.Observe(999)

	cum, upperBounds, sum, count := h.Snapshot()
	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}
	// cum = [2 (<=10), 2 (<=50), 3 (<=100), 4 (+Inf)]
	wantCum := []uint64{2, 2, 3, 4}
	if len(cum) != len(wantCum) {
		t.Fatalf("cumulative len = %d, want %d", len(cum), len(wantCum))
	}
	for i, want := range wantCum {
		if cum[i] != want {
			t.Errorf("cumulative[%d] (le=%v) = %d, want %d", i, upperBounds[i], cum[i], want)
		}
	}
	if sum != 5+10+75+999 {
		t.Errorf("sum = %v, want %v", sum, 1089)
	}
}

// TestHistogramZeroValue renders an empty histogram without panicking
// and produces a valid +Inf bucket of 0.
func TestHistogramZeroValue(t *testing.T) {
	h := NewHistogram(DefaultBuckets)
	cum, _, _, count := h.Snapshot()
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	// Last cumulative entry is the +Inf bucket; must be 0.
	if cum[len(cum)-1] != 0 {
		t.Errorf("+Inf bucket = %d, want 0", cum[len(cum)-1])
	}
}

// TestSubmitRecordsLatencyAndTTFT confirms Submit feeds both
// histograms when the latency fields are positive.
func TestSubmitRecordsLatencyAndTTFT(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 42, TTFTMs: 12})
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 600, TTFTMs: 80})

	_, _, latSum, latCount := c.Latency().Snapshot()
	if latCount != 2 {
		t.Errorf("latency count = %d, want 2", latCount)
	}
	if latSum != 642 {
		t.Errorf("latency sum = %v, want 642", latSum)
	}

	_, _, ttftSum, ttftCount := c.TTFT().Snapshot()
	if ttftCount != 2 {
		t.Errorf("ttft count = %d, want 2", ttftCount)
	}
	if ttftSum != 92 {
		t.Errorf("ttft sum = %v, want 92", ttftSum)
	}
}

// TestSubmitZeroLatencyNotObserved guards the contract that a
// non-streaming response (TTFTMs == 0) must not pollute the TTFT
// histogram with a spurious 0-ms observation.
func TestSubmitZeroLatencyNotObserved(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 0, TTFTMs: 0})
	_, _, _, count := c.Latency().Snapshot()
	if count != 0 {
		t.Errorf("latency count after 0-ms submit = %d, want 0", count)
	}
}

// TestConcurrentSubmit exercises the lock-free collector under
// concurrent writers to flush out any data race (-race catches the
// rest). 100 goroutines × 100 submissions = 10,000 events.
func TestConcurrentSubmit(t *testing.T) {
	c := NewCollector()
	const goroutines, perG = 100, 100
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perG; i++ {
				c.Submit(ObservabilityEvent{
					Route:            "local",
					InputTokens:      1,
					EstimatedCostUSD: 0.001,
					TotalLatencyMs:   5,
					TTFTMs:           2,
				})
			}
		}()
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}
	want := uint64(goroutines * perG)
	if got := c.RequestsLocal(); got != want {
		t.Errorf("RequestsLocal = %d, want %d", got, want)
	}
	if got := c.inputTokensTotal.Load(); got != want {
		t.Errorf("inputTokensTotal = %d, want %d", got, want)
	}
	_, _, _, count := c.Latency().Snapshot()
	if count != want {
		t.Errorf("latency count = %d, want %d", count, want)
	}
}

// TestNilHistogramRenderSafe ensures writeHistogram on a nil histogram
// does not panic (defensive: a misconfigured collector must never crash
// the scrape handler).
func TestNilHistogramRenderSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeHistogram panicked on nil histogram: %v", r)
		}
	}()
	var buf strings.Builder
	writeHistogram(&buf, "x", "help", nil)
}

// TestNilCollectorRenderSafe ensures RenderPrometheus on a nil
// collector writes nothing and does not panic.
func TestNilCollectorRenderSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RenderPrometheus panicked on nil collector: %v", r)
		}
	}()
	var buf strings.Builder
	RenderPrometheus(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("nil collector wrote %d bytes, want 0", buf.Len())
	}
}

// --- Middleware instrumentation (issue #70) --------------------------------

// TestIncAuthCounters verifies each of the three per-decision auth
// counters increments independently and that AuthAuthenticatedClients
// mirrors the accepted counter for the gauge surface.
func TestIncAuthCounters(t *testing.T) {
	c := NewCollector()

	c.IncAuthAccepted()
	c.IncAuthAccepted()
	c.IncAuthRejectedInvalid()
	c.IncAuthRejectedMissing()
	c.IncAuthRejectedMissing()

	if got := c.authAccepted.Load(); got != 2 {
		t.Errorf("authAccepted = %d, want 2", got)
	}
	if got := c.authRejectedInvalid.Load(); got != 1 {
		t.Errorf("authRejectedInvalid = %d, want 1", got)
	}
	if got := c.authRejectedMissing.Load(); got != 2 {
		t.Errorf("authRejectedMissing = %d, want 2", got)
	}
	if got := c.AuthAuthenticatedClients(); got != 2 {
		t.Errorf("AuthAuthenticatedClients = %d, want 2", got)
	}
}

// TestIncAuthBlocked verifies IncAuthBlocked increments the per-reason
// counter (issues #831/#937).
func TestIncAuthBlocked(t *testing.T) {
	c := NewCollector()
	if got := c.authBlockedTotal["missing"].Load(); got != 0 {
		t.Errorf("initial authBlockedTotal[missing] = %d, want 0", got)
	}
	if got := c.authBlockedTotal["invalid"].Load(); got != 0 {
		t.Errorf("initial authBlockedTotal[invalid] = %d, want 0", got)
	}
	// 3 missing-token blocks
	c.IncAuthBlocked("missing")
	c.IncAuthBlocked("missing")
	c.IncAuthBlocked("missing")
	// 2 invalid-token blocks
	c.IncAuthBlocked("invalid")
	c.IncAuthBlocked("invalid")
	if got := c.authBlockedTotal["missing"].Load(); got != 3 {
		t.Errorf("after 3 IncAuthBlocked(missing): authBlockedTotal[missing] = %d, want 3", got)
	}
	if got := c.authBlockedTotal["invalid"].Load(); got != 2 {
		t.Errorf("after 2 IncAuthBlocked(invalid): authBlockedTotal[invalid] = %d, want 2", got)
	}
	// Remaining 5 blocks are missing
	for i := 0; i < 5; i++ {
		c.IncAuthBlocked("missing")
	}
	if got := c.authBlockedTotal["missing"].Load(); got != 8 {
		t.Errorf("after 8 IncAuthBlocked(missing): authBlockedTotal[missing] = %d, want 8", got)
	}
	if got := c.authBlockedTotal["invalid"].Load(); got != 2 {
		t.Errorf("after 2 IncAuthBlocked(invalid): authBlockedTotal[invalid] = %d, want 2", got)
	}
}

// TestIncRateLimitScopes verifies the IncRateLimit(scope, allowed)
// helper routes to the correct counter for both recognised scopes
// and silently drops unknown ones (so a wiring bug is visible in
// logs rather than silently masked as a default bucket).
func TestIncRateLimitScopes(t *testing.T) {
	c := NewCollector()

	c.IncRateLimit("global", true)
	c.IncRateLimit("global", true)
	c.IncRateLimit("global", false)
	c.IncRateLimit("per_client", true)
	c.IncRateLimit("per_client", false)
	c.IncRateLimit("per_client", false)
	c.IncRateLimit("per_client", false)
	// Unknown scope: must not panic and must not affect known buckets.
	c.IncRateLimit("nonexistent", true)

	if got := c.rateLimitAllowedGlobal.Load(); got != 2 {
		t.Errorf("rateLimitAllowedGlobal = %d, want 2", got)
	}
	if got := c.rateLimitRejectedGlobal.Load(); got != 1 {
		t.Errorf("rateLimitRejectedGlobal = %d, want 1", got)
	}
	if got := c.rateLimitAllowedPerClient.Load(); got != 1 {
		t.Errorf("rateLimitAllowedPerClient = %d, want 1", got)
	}
	if got := c.rateLimitRejectedPerClient.Load(); got != 3 {
		t.Errorf("rateLimitRejectedPerClient = %d, want 3", got)
	}
}

// TestBudgetRecorder verifies AddBudgetRecorded accumulates the float
// total lock-free and BudgetRecordedUSD returns the same value.
// Non-positive amounts are ignored to match SpendTracker.Record.
func TestBudgetRecorder(t *testing.T) {
	c := NewCollector()

	c.AddBudgetRecorded(0.01)
	c.AddBudgetRecorded(0.02)
	c.AddBudgetRecorded(0)    // dropped
	c.AddBudgetRecorded(-0.5) // dropped (negative)

	got := c.BudgetRecordedUSD()
	if math.Abs(got-0.03) > 1e-9 {
		t.Errorf("BudgetRecordedUSD = %v, want 0.03", got)
	}

	// Exceeded counter increments independently.
	c.IncBudgetExceeded()
	c.IncBudgetExceeded()
	if got := c.BudgetExceeded(); got != 2 {
		t.Errorf("BudgetExceeded = %d, want 2", got)
	}
}

// TestTLSCounters verifies IncTLSAccepted / IncTLSRejected
// increment independently. The wiring layer in main.go drives the
// "accepted" signal from tls.Config.VerifyConnection, so the helper
// must be reachable from the test without going through net/http.
func TestTLSCounters(t *testing.T) {
	c := NewCollector()

	c.IncTLSAccepted()
	c.IncTLSAccepted()
	c.IncTLSRejected()

	if got := c.tlsConnectionsAccepted.Load(); got != 2 {
		t.Errorf("tlsConnectionsAccepted = %d, want 2", got)
	}
	if got := c.tlsConnectionsRejected.Load(); got != 1 {
		t.Errorf("tlsConnectionsRejected = %d, want 1", got)
	}
}

// TestCollectorCountersConcurrent hammers all the new counters from
// many goroutines under -race to confirm the lock-free guarantee.
func TestCollectorCountersConcurrent(t *testing.T) {
	c := NewCollector()
	const goroutines = 16
	const iters = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c.IncAuthAccepted()
				c.IncAuthRejectedInvalid()
				c.IncAuthRejectedMissing()
				if id%2 == 0 {
					c.IncRateLimit("global", true)
				} else {
					c.IncRateLimit("per_client", false)
				}
				c.AddBudgetRecorded(0.001)
				c.IncBudgetExceeded()
				c.IncTLSAccepted()
				c.IncTLSRejected()
			}
		}(g)
	}
	wg.Wait()

	const wantAuth = goroutines * iters
	if got := c.authAccepted.Load(); got != wantAuth {
		t.Errorf("authAccepted = %d, want %d", got, wantAuth)
	}
	if got := c.authRejectedInvalid.Load(); got != wantAuth {
		t.Errorf("authRejectedInvalid = %d, want %d", got, wantAuth)
	}
	if got := c.authRejectedMissing.Load(); got != wantAuth {
		t.Errorf("authRejectedMissing = %d, want %d", got, wantAuth)
	}
	if got := c.rateLimitAllowedGlobal.Load(); got != uint64(goroutines/2*iters) {
		t.Errorf("rateLimitAllowedGlobal = %d, want %d", got, goroutines/2*iters)
	}
	if got := c.rateLimitRejectedPerClient.Load(); got != uint64((goroutines-goroutines/2)*iters) {
		t.Errorf("rateLimitRejectedPerClient = %d, want %d", got, (goroutines-goroutines/2)*iters)
	}
	if got := c.tlsConnectionsAccepted.Load(); got != wantAuth {
		t.Errorf("tlsConnectionsAccepted = %d, want %d", got, wantAuth)
	}
	wantBudgetUSD := 0.001 * float64(wantAuth)
	if got := c.BudgetRecordedUSD(); math.Abs(got-wantBudgetUSD) > 1e-9 {
		t.Errorf("BudgetRecordedUSD = %v, want %v", got, wantBudgetUSD)
	}
}

// TestObservePipelineStage verifies each stage histogram accepts observations
// and the buckets are populated correctly (issue #300).
func TestObservePipelineStage(t *testing.T) {
	c := NewCollector()

	// Emit one observation per stage.
	c.ObservePipelineStage(PipelineStageEvent{
		RAGRetrievalMs:      15,
		PromptEngineeringMs: 3,
		TOONCompressionMs:   2,
		SLMRoutingMs:        8,
		UpstreamFirstByteMs: 250,
	})

	// Verify each stage histogram received exactly one observation.
	for _, tc := range []struct {
		name      string
		hist      *Histogram
		wantCount uint64
	}{
		{"stageRAG", c.stageRAG, 1},
		{"stagePromptEng", c.stagePromptEng, 1},
		{"stageTOON", c.stageTOON, 1},
		{"stageSLM", c.stageSLM, 1},
		{"stageUpstream", c.stageUpstream, 1},
	} {
		if got := tc.hist.count.Load(); got != tc.wantCount {
			t.Errorf("%s count = %d, want %d", tc.name, got, tc.wantCount)
		}
	}
}

// TestObservePipelineStageZeroValues verifies zero-valued fields do not
// produce observations (the handler skips zero values to keep metrics clean).
func TestObservePipelineStageZeroValues(t *testing.T) {
	c := NewCollector()

	// Only upstream has a non-zero value.
	c.ObservePipelineStage(PipelineStageEvent{
		RAGRetrievalMs:      0,
		PromptEngineeringMs: 0,
		TOONCompressionMs:   0,
		SLMRoutingMs:        0,
		UpstreamFirstByteMs: 50,
	})

	if got := c.stageUpstream.count.Load(); got != 1 {
		t.Errorf("stageUpstream count = %d, want 1", got)
	}
	// All other histograms should be at zero.
	for _, tc := range []struct {
		name string
		hist *Histogram
	}{
		{"stageRAG", c.stageRAG},
		{"stagePromptEng", c.stagePromptEng},
		{"stageTOON", c.stageTOON},
		{"stageSLM", c.stageSLM},
	} {
		if got := tc.hist.count.Load(); got != 0 {
			t.Errorf("%s count = %d, want 0", tc.name, got)
		}
	}
}

// TestPipelineStageHandlerContentType verifies the Handler returns the
// correct Prometheus Content-Type header.
func TestPipelineStageHandlerContentType(t *testing.T) {
	c := NewCollector()
	c.ObservePipelineStage(PipelineStageEvent{UpstreamFirstByteMs: 100})

	req := httptest.NewRequest("GET", "/metrics/stages", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got == "" {
		t.Errorf("Content-Type header is empty, want text/plain")
	}
}

// TestPipelineStageHandlerReturnsPrometheusFormat verifies the handler
// output contains the expected stage metric name.
func TestPipelineStageHandlerReturnsPrometheusFormat(t *testing.T) {
	c := NewCollector()
	c.ObservePipelineStage(PipelineStageEvent{
		RAGRetrievalMs:      10,
		PromptEngineeringMs: 5,
		TOONCompressionMs:   2,
		SLMRoutingMs:        7,
		UpstreamFirstByteMs: 150,
	})

	req := httptest.NewRequest("GET", "/metrics/stages", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	metricName := "nexus_pipeline_stage_latency_ms"
	if !strings.Contains(body, metricName) {
		t.Errorf("handler output does not contain %q, got:\n%s", metricName, body)
	}
}

// TestObservePipelineStageConcurrent verifies ObservePipelineStage is safe
// for concurrent calls under the race detector.
func TestObservePipelineStageConcurrent(t *testing.T) {
	c := NewCollector()
	const goroutines = 16
	const iters = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Add 1 to each modulo to ensure all values are > 0
				// so the handler records every observation.
				c.ObservePipelineStage(PipelineStageEvent{
					RAGRetrievalMs:      int64(i%20) + 1,
					PromptEngineeringMs: int64(i%10) + 1,
					TOONCompressionMs:   int64(i%5) + 1,
					SLMRoutingMs:        int64(i%15) + 1,
					UpstreamFirstByteMs: int64(i%50) + 100,
				})
			}
		}()
	}
	wg.Wait()

	// All histograms should have received exactly goroutines * iters observations.
	wantCount := uint64(goroutines * iters)
	for _, tc := range []struct {
		name string
		hist *Histogram
	}{
		{"stageRAG", c.stageRAG},
		{"stagePromptEng", c.stagePromptEng},
		{"stageTOON", c.stageTOON},
		{"stageSLM", c.stageSLM},
		{"stageUpstream", c.stageUpstream},
	} {
		if got := tc.hist.count.Load(); got != wantCount {
			t.Errorf("%s count = %d, want %d", tc.name, got, wantCount)
		}
	}
}

// TestCollectorSatisfiesGaugeProvider (issue #443) is a compile-time
// guard that *Collector implements GaugeProvider so the RouteCounters
// handler can pass it directly to RenderPrometheus. If Gauges() ever
// drifts in signature the build fails here rather than at runtime.
func TestCollectorSatisfiesGaugeProvider(t *testing.T) {
	var _ GaugeProvider = (*Collector)(nil)
}

// TestCollectorGaugesReturnsCircuitState (issue #443) verifies that a
// fresh collector returns an empty slice, then transitions and reports
// three labelled samples per known circuit.
func TestCollectorGaugesReturnsCircuitState(t *testing.T) {
	c := NewCollector()

	if got := c.Gauges(); len(got) != 0 {
		t.Errorf("fresh collector Gauges() = %d samples, want 0", len(got))
	}

	c.RecordCircuitFailure("rag")
	c.RecordCircuitFailure("rag")
	c.RecordCircuitRecovery("rag")
	c.RecordCircuitHalfOpen("ollama")

	samples := c.Gauges()
	wantNames := map[string]bool{
		"nexus_circuit_breaker_state":                false,
		"nexus_circuit_breaker_failures_total":       false,
		"nexus_circuit_breaker_last_failure_seconds": false,
	}
	gotByCircuit := map[string]map[string]float64{}
	for _, s := range samples {
		wantNames[s.Name] = true
		circuit := s.Labels["circuit"]
		if gotByCircuit[circuit] == nil {
			gotByCircuit[circuit] = map[string]float64{}
		}
		gotByCircuit[circuit][s.Name] = s.Value
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("expected sample for %q in collector.Gauges()", name)
		}
	}

	rag := gotByCircuit["rag"]
	if rag == nil {
		t.Fatalf("missing rag circuit samples")
	}
	if rag["nexus_circuit_breaker_state"] != float64(circuitStateClosed) {
		t.Errorf("rag state = %v, want %d (closed after recovery)", rag["nexus_circuit_breaker_state"], circuitStateClosed)
	}
	if rag["nexus_circuit_breaker_failures_total"] != 2 {
		t.Errorf("rag failures_total = %v, want 2", rag["nexus_circuit_breaker_failures_total"])
	}
	if rag["nexus_circuit_breaker_last_failure_seconds"] == 0 {
		t.Errorf("rag last_failure_seconds should be non-zero, got 0")
	}

	ollama := gotByCircuit["ollama"]
	if ollama == nil {
		t.Fatalf("missing ollama circuit samples")
	}
	if ollama["nexus_circuit_breaker_state"] != float64(circuitStateHalfOpen) {
		t.Errorf("ollama state = %v, want %d (half_open)", ollama["nexus_circuit_breaker_state"], circuitStateHalfOpen)
	}
}

// TestCollectorGaugesNilSafe (issue #443) verifies Gauges() on a nil
// *Collector returns nil so main.go can skip the collector during boot
// or in tests without a nil-deref panic.
func TestCollectorGaugesNilSafe(t *testing.T) {
	var c *Collector
	if got := c.Gauges(); got != nil {
		t.Errorf("nil collector Gauges() = %v, want nil", got)
	}
}

// TestRAGSimilarityHistogramsPreAllocated (issue #447, #671) verifies that
// NewCollector pre-allocates one Histogram per (path, outcome) pair
// with threshold=0.00 (the default/global threshold placeholder) so
// ObserveRAGSimilarity never needs to allocate on the hot path for
// the default threshold case. Additional histograms for other thresholds
// are created lazily by ObserveRAGSimilarity.
// All four buckets must be present and empty (count == 0) until an
// observation lands.
func TestRAGSimilarityHistogramsPreAllocated(t *testing.T) {
	c := NewCollector()
	hists := c.RAGSimilarityHistograms()
	if len(hists) != 4 {
		t.Fatalf("pre-allocated histograms = %d, want 4 (one per path×outcome)", len(hists))
	}
	// Pre-allocated keys use threshold=0.00 as a placeholder for the global/default threshold
	wantKeys := []string{"hnsw|hit|0.00", "hnsw|miss|0.00", "brute_force|hit|0.00", "brute_force|miss|0.00"}
	for _, k := range wantKeys {
		h, ok := hists[k]
		if !ok || h == nil {
			t.Errorf("missing pre-allocated histogram for %q", k)
			continue
		}
		_, _, _, count := h.Snapshot()
		if count != 0 {
			t.Errorf("fresh histogram %q has count=%d, want 0", k, count)
		}
	}
}

// TestObserveRAGSimilarityAdvancesBucket (issue #447) is the bucket
// advancement check from the acceptance criteria. One observation at
// score=0.7 must land in the (path="hnsw", outcome="hit") bucket
// count == 1 and bucket le="0.8" cumulative == 1.
func TestObserveRAGSimilarityAdvancesBucket(t *testing.T) {
	c := NewCollector()
	c.ObserveRAGSimilarity("hnsw", "hit", 0.7, 0.55)

	hists := c.RAGSimilarityHistograms()
	h, ok := hists["hnsw|hit|0.55"]
	if !ok {
		t.Fatalf("missing hnsw|hit|0.55 histogram")
	}
	cum, upperBounds, _, count := h.Snapshot()
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	// Score 0.7 must fall in the (0.6, 0.8] bucket — i.e. cum[7] = 1
	// (upperBounds[7] == 0.8) and the trailing buckets remain 0.
	for i, ub := range upperBounds {
		var want uint64
		if ub >= 0.7 {
			want = 1
		}
		if cum[i] != want {
			t.Errorf("cum[%d] (le=%v) = %d, want %d", i, ub, cum[i], want)
		}
	}
	if cum[len(upperBounds)] != 1 {
		t.Errorf("+Inf bucket = %d, want 1", cum[len(upperBounds)])
	}
}

// TestObserveRAGSimilarityHitAndMissSeparate asserts the AC
// "Hits and misses record exactly once": a hit and a miss at the
// same score on the same path advance different histograms and do
// not cross-contaminate.
func TestObserveRAGSimilarityHitAndMissSeparate(t *testing.T) {
	c := NewCollector()
	c.ObserveRAGSimilarity("brute_force", "hit", 0.9, 0.55)
	c.ObserveRAGSimilarity("brute_force", "miss", 0.4, 0.55)

	hists := c.RAGSimilarityHistograms()
	hitCount := snapshotCount(t, hists["brute_force|hit|0.55"])
	missCount := snapshotCount(t, hists["brute_force|miss|0.55"])
	if hitCount != 1 {
		t.Errorf("hit count = %d, want 1", hitCount)
	}
	if missCount != 1 {
		t.Errorf("miss count = %d, want 1", missCount)
	}
	// Other paths/thresholds must remain untouched.
	if got := snapshotCount(t, hists["hnsw|hit|0.55"]); got != 0 {
		t.Errorf("hnsw|hit|0.55 count = %d, want 0 (unobserved)", got)
	}
	if got := snapshotCount(t, hists["hnsw|miss|0.55"]); got != 0 {
		t.Errorf("hnsw|miss|0.55 count = %d, want 0 (unobserved)", got)
	}
}

// TestObserveRAGSimilarityClampsOutOfRange confirms the
// documented safety net: scores outside [0, 1] are clamped rather
// than pushed into the +Inf bucket by a buggy embedder. The 1.5
// observation must land in the (0.9, 1.0] bucket, not the +Inf
// overflow.
func TestObserveRAGSimilarityClampsOutOfRange(t *testing.T) {
	c := NewCollector()
	c.ObserveRAGSimilarity("hnsw", "hit", 1.5, 0.55)
	c.ObserveRAGSimilarity("hnsw", "miss", -0.2, 0.55)

	hists := c.RAGSimilarityHistograms()
	cum, upperBounds, _, _ := hists["hnsw|hit|0.55"].Snapshot()
	// Clamped to 1.0 → bucket le="1" cumulative = 1, +Inf = 1.
	if cum[len(upperBounds)-1] != 1 {
		t.Errorf("hit bucket le=1 = %d, want 1 (clamped from 1.5)", cum[len(upperBounds)-1])
	}
	if cum[len(upperBounds)] != 1 {
		t.Errorf("hit +Inf bucket = %d, want 1 (single observation)", cum[len(upperBounds)])
	}

	cum, upperBounds, _, _ = hists["hnsw|miss|0.55"].Snapshot()
	if cum[0] != 1 {
		t.Errorf("miss bucket le=0.1 = %d, want 1 (clamped from -0.2)", cum[0])
	}
	if cum[len(upperBounds)] != 1 {
		t.Errorf("miss +Inf bucket = %d, want 1", cum[len(upperBounds)])
	}
}

// TestObserveRAGSimilarityUnknownPathIgnored confirms the
// bounded-cardinality contract: an unrecognised path or outcome
// label is silently dropped rather than landing under a third
// bucket. This is the "labels and cardinality are documented"
// half of the AC.
func TestObserveRAGSimilarityUnknownPathIgnored(t *testing.T) {
	c := NewCollector()
	c.ObserveRAGSimilarity("pinecone", "hit", 0.8, 0.55)
	c.ObserveRAGSimilarity("hnsw", "unknown", 0.8, 0.55)

	for _, k := range []string{"hnsw|hit|0.55", "hnsw|miss|0.55", "brute_force|hit|0.55", "brute_force|miss|0.55"} {
		if got := snapshotCount(t, c.RAGSimilarityHistograms()[k]); got != 0 {
			t.Errorf("%s count = %d after unknown label, want 0", k, got)
		}
	}
}

// TestObserveRAGSimilarityConcurrent (issue #447) exercises the
// histogram under concurrent Observe calls. 100 goroutines × 100
// observations = 10,000 events; -race catches any data race and
// the count assertion confirms no observation is lost.
func TestObserveRAGSimilarityConcurrent(t *testing.T) {
	c := NewCollector()
	const goroutines, perG = 100, 100
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perG; i++ {
				c.ObserveRAGSimilarity("hnsw", "hit", 0.5, 0.55)
			}
		}()
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}
	want := uint64(goroutines * perG)
	if got := snapshotCount(t, c.RAGSimilarityHistograms()["hnsw|hit|0.55"]); got != want {
		t.Errorf("concurrent hit count = %d, want %d", got, want)
	}
}

// TestObserveRateLimitUtilizationBasic (issue #746) verifies one
// observation is recorded correctly.
func TestObserveRateLimitUtilizationBasic(t *testing.T) {
	c := NewCollector()
	c.ObserveRateLimitUtilization("abc123", 0.6)
	hists := c.RateLimitUtilizationHistograms()
	h := hists["abc123"]
	if h == nil {
		t.Fatal("histogram for abc123 is nil")
	}
	_, _, sum, count := h.Snapshot()
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if sum != 0.6 {
		t.Errorf("sum = %v, want 0.6", sum)
	}
}

// TestObserveRateLimitUtilizationClamping verifies values outside [0, 1]
// are clamped to the valid range.
func TestObserveRateLimitUtilizationClamping(t *testing.T) {
	c := NewCollector()
	c.ObserveRateLimitUtilization("client1", -0.5) // clamped to 0
	c.ObserveRateLimitUtilization("client2", 1.5)  // clamped to 1
	c.ObserveRateLimitUtilization("client3", 0.75) // unchanged

	hists := c.RateLimitUtilizationHistograms()
	for _, tc := range []struct {
		bucketID string
		want     float64
	}{
		{"client1", 0.0},
		{"client2", 1.0},
		{"client3", 0.75},
	} {
		h := hists[tc.bucketID]
		if h == nil {
			t.Fatalf("histogram for %s is nil", tc.bucketID)
		}
		_, _, sum, count := h.Snapshot()
		if count != 1 {
			t.Errorf("%s: count = %d, want 1", tc.bucketID, count)
		}
		if sum != tc.want {
			t.Errorf("%s: sum = %v, want %v", tc.bucketID, sum, tc.want)
		}
	}
}

// TestObserveRateLimitUtilizationEmptyBucketID verifies an empty bucket
// ID is silently ignored.
func TestObserveRateLimitUtilizationEmptyBucketID(t *testing.T) {
	c := NewCollector()
	c.ObserveRateLimitUtilization("", 0.5)
	if h := c.RateLimitUtilizationHistograms(); len(h) != 0 {
		t.Errorf("empty bucketID created histogram, want none")
	}
}

// TestObserveRateLimitUtilizationNilCollector verifies a nil receiver
// does not panic.
func TestObserveRateLimitUtilizationNilCollector(t *testing.T) {
	var c *Collector
	c.ObserveRateLimitUtilization("abc", 0.5) // must not panic
}

// TestObserveRateLimitUtilizationHistogramsNilCollector verifies
// RateLimitUtilizationHistograms returns nil for a nil receiver.
func TestObserveRateLimitUtilizationHistogramsNilCollector(t *testing.T) {
	var c *Collector
	if h := c.RateLimitUtilizationHistograms(); h != nil {
		t.Errorf("nil collector histograms = %v, want nil", h)
	}
}

// TestObserveRateLimitUtilizationConcurrent exercises the histogram under
// concurrent observations. 100 goroutines × 100 observations = 10,000
// events; -race catches any data race.
func TestObserveRateLimitUtilizationConcurrent(t *testing.T) {
	c := NewCollector()
	const goroutines, perG = 100, 100
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perG; i++ {
				c.ObserveRateLimitUtilization("concurrent-client", 0.5)
			}
		}()
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}
	want := uint64(goroutines * perG)
	h := c.RateLimitUtilizationHistograms()["concurrent-client"]
	if got := snapshotCount(t, h); got != want {
		t.Errorf("concurrent count = %d, want %d", got, want)
	}
}

// snapshotCount is a tiny helper to keep the table-style tests
// above compact.
func snapshotCount(t *testing.T, h *Histogram) uint64 {
	t.Helper()
	if h == nil {
		return 0
	}
	_, _, _, count := h.Snapshot()
	return count
}

// --- Latency percentile ring buffer (issue #774) -----------------------------

// TestLatencyPercentileBufferBasic exercises the buffer with a uniform
// distribution and verifies p50/p95/p99 are within 5% tolerance of
// the true theoretical values for a known distribution.
func TestLatencyPercentileBufferBasic(t *testing.T) {
	buf := newLatencyPercentileBuffer(100)

	// Uniform distribution: values 1..100 (mean=50.5)
	for i := 1; i <= 100; i++ {
		buf.Observe(float64(i))
	}

	p50, p95, p99 := buf.Perc()

	// True p50 for 1..100 is 50.5; 5% tolerance → [47.975, 53.025]
	if p50 < 47.975 || p50 > 53.025 {
		t.Errorf("p50 = %v, want within 5%% of 50.5 (47.975..53.025)", p50)
	}
	// True p95 for 1..100 is 95.5; 5% tolerance → [90.725, 100.275]
	if p95 < 90.725 || p95 > 100.275 {
		t.Errorf("p95 = %v, want within 5%% of 95.5 (90.725..100.275)", p95)
	}
	// True p99 for 1..100 is 99.5; 5% tolerance → [94.525, 100.0]
	if p99 < 94.525 || p99 > 100.0 {
		t.Errorf("p99 = %v, want within 5%% of 99.5 (94.525..100.0)", p99)
	}
}

// TestLatencyPercentileBufferRingEviction verifies the buffer evicts
// oldest samples when capacity is reached.
func TestLatencyPercentileBufferRingEviction(t *testing.T) {
	buf := newLatencyPercentileBuffer(10)

	// Fill the buffer
	for i := 1; i <= 10; i++ {
		buf.Observe(float64(i))
	}
	p50Before, _, _ := buf.Perc()

	// Add 5 more — oldest values (1..5) should be evicted
	for i := 11; i <= 15; i++ {
		buf.Observe(float64(i))
	}

	// p50 should now be from the range 6..15, not including 1..5
	// The median of 6..15 is 10.5
	p50After, _, _ := buf.Perc()
	if p50After <= 5.0 {
		t.Errorf("p50 after eviction = %v, want > 5 (old values should be evicted)", p50After)
	}
	if p50After == p50Before {
		t.Errorf("p50 did not change after adding new samples: %v", p50After)
	}
}

// TestLatencyPercentileBufferZeroValue verifies empty buffer returns 0.
func TestLatencyPercentileBufferZeroValue(t *testing.T) {
	buf := newLatencyPercentileBuffer(100)
	p50, p95, p99 := buf.Perc()
	if p50 != 0 || p95 != 0 || p99 != 0 {
		t.Errorf("empty buffer: got p50=%v p95=%v p99=%v, want all 0", p50, p95, p99)
	}
}

// TestLatencyPercentileBufferSingleValue verifies buffer with one sample.
func TestLatencyPercentileBufferSingleValue(t *testing.T) {
	buf := newLatencyPercentileBuffer(100)
	buf.Observe(42.0)
	p50, p95, p99 := buf.Perc()
	if p50 != 42.0 || p95 != 42.0 || p99 != 42.0 {
		t.Errorf("single value: got p50=%v p95=%v p99=%v, want all 42.0", p50, p95, p99)
	}
}

// TestLatencyPercentileBufferCount verifies Count returns correct sample count.
func TestLatencyPercentileBufferCount(t *testing.T) {
	buf := newLatencyPercentileBuffer(5)
	if got := buf.Count(); got != 0 {
		t.Errorf("empty count = %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		buf.Observe(float64(i))
	}
	if got := buf.Count(); got != 3 {
		t.Errorf("count after 3 = %d, want 3", got)
	}
	// Fill beyond capacity
	for i := 4; i <= 10; i++ {
		buf.Observe(float64(i))
	}
	if got := buf.Count(); got != 5 {
		t.Errorf("count after overflow = %d, want 5 (capacity)", got)
	}
}

// TestLatencyPercentileBufferNegativeCapacity defaults to 1000.
func TestLatencyPercentileBufferNegativeCapacity(t *testing.T) {
	buf := newLatencyPercentileBuffer(-5)
	if buf.capacity != defaultLatencyBufferCapacity {
		t.Errorf("negative capacity: got %d, want %d", buf.capacity, defaultLatencyBufferCapacity)
	}
}

// TestLatencyPercentileBufferConcurrent exercises Observe from many goroutines
// under the race detector.
func TestLatencyPercentileBufferConcurrent(t *testing.T) {
	buf := newLatencyPercentileBuffer(5000) // large enough for all samples
	const goroutines = 16
	const iters = 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				buf.Observe(float64(id*iters + i + 1))
			}
		}(g)
	}
	wg.Wait()

	count := buf.Count()
	want := goroutines * iters
	if count != want {
		t.Errorf("count = %d, want %d", count, want)
	}
	p50, p95, p99 := buf.Perc()
	if p50 == 0 || p95 == 0 || p99 == 0 {
		t.Errorf("percentiles should be non-zero after concurrent writes: p50=%v p95=%v p99=%v", p50, p95, p99)
	}
}

// TestObserveLatencyRoutesToCorrectBuffer verifies ObserveLatency
// creates buffers per route and routes observations correctly.
func TestObserveLatencyRoutesToCorrectBuffer(t *testing.T) {
	c := NewCollector()
	c.ObserveLatency("local", 100)
	c.ObserveLatency("local", 200)
	c.ObserveLatency("frontier", 300)
	c.ObserveLatency("fusion", 400)

	gauges := c.LatencyPercentileGauges()
	if len(gauges) == 0 {
		t.Fatal("LatencyPercentileGauges returned empty")
	}

	// Group by route
	byRoute := make(map[string]map[string]float64)
	for _, g := range gauges {
		route := g.Labels["route"]
		if byRoute[route] == nil {
			byRoute[route] = make(map[string]float64)
		}
		byRoute[route][g.Name] = g.Value
	}

	local := byRoute["local"]
	if local == nil {
		t.Fatal("missing local route gauges")
	}
	// local p50 should be 150 (median of 100, 200)
	if local["nexus_upstream_request_latency_p50_seconds"] == 0 {
		t.Errorf("local p50 is 0, want non-zero")
	}

	frontier := byRoute["frontier"]
	if frontier == nil {
		t.Fatal("missing frontier route gauges")
	}

	fusion := byRoute["fusion"]
	if fusion == nil {
		t.Fatal("missing fusion route gauges")
	}
}

// TestObserveLatencyNilSafe verifies nil collector does not panic.
func TestObserveLatencyNilSafe(t *testing.T) {
	var c *Collector
	c.ObserveLatency("local", 100) // must not panic
}

// TestObserveLatencyZeroAndNegativeIgnored verifies non-positive latencies are ignored.
func TestObserveLatencyZeroAndNegativeIgnored(t *testing.T) {
	c := NewCollector()
	c.ObserveLatency("local", 0)
	c.ObserveLatency("local", -10)
	c.ObserveLatency("local", 100)

	gauges := c.LatencyPercentileGauges()
	if len(gauges) == 0 {
		t.Fatal("expected gauges after valid observation")
	}
	// p50 should be from value 100 only
	for _, g := range gauges {
		if g.Name == "nexus_upstream_request_latency_p50_seconds" && g.Labels["route"] == "local" {
			if g.Value != 0.1 { // 100ms / 1000 = 0.1s
				t.Errorf("local p50 = %v, want 0.1 (100ms in seconds)", g.Value)
			}
		}
	}
}

// TestLatencyPercentileGaugesNilSafe verifies nil collector returns nil.
func TestLatencyPercentileGaugesNilSafe(t *testing.T) {
	var c *Collector
	if got := c.LatencyPercentileGauges(); got != nil {
		t.Errorf("nil LatencyPercentileGauges() = %v, want nil", got)
	}
}

// TestLatencyPercentileGaugesEmptyBeforeFirstObservation verifies no
// gauges are emitted before any observation.
func TestLatencyPercentileGaugesEmptyBeforeFirstObservation(t *testing.T) {
	c := NewCollector()
	gauges := c.LatencyPercentileGauges()
	if len(gauges) != 0 {
		t.Errorf("empty collector gauges = %d, want 0 before first observation", len(gauges))
	}
}

// TestCollectorGaugesIncludeLatencyPercentiles verifies that the full
// Gauges() output from a collector with latency observations includes
// the three percentile metric names.
func TestCollectorGaugesIncludeLatencyPercentiles(t *testing.T) {
	c := NewCollector()
	c.ObserveLatency("local", 100)
	c.ObserveLatency("local", 200)
	c.ObserveLatency("frontier", 300)

	gauges := c.Gauges()

	wantNames := map[string]bool{
		"nexus_upstream_request_latency_p50_seconds": false,
		"nexus_upstream_request_latency_p95_seconds": false,
		"nexus_upstream_request_latency_p99_seconds": false,
	}
	for _, g := range gauges {
		wantNames[g.Name] = true
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("expected gauge %q in collector.Gauges()", name)
		}
	}
}

// TestLatencyPercentileGaugesUnitConversion verifies latency values
// are converted from milliseconds to seconds in the emitted gauges.
func TestLatencyPercentileGaugesUnitConversion(t *testing.T) {
	c := NewCollector()
	c.ObserveLatency("local", 1000) // 1000ms = 1s

	gauges := c.LatencyPercentileGauges()
	for _, g := range gauges {
		if g.Name == "nexus_upstream_request_latency_p50_seconds" && g.Labels["route"] == "local" {
			if g.Value != 1.0 {
				t.Errorf("p50 gauge value = %v, want 1.0 (1000ms converted to seconds)", g.Value)
			}
		}
	}
}

// TestLatencyPercentileGaugesSortedOutput verifies the gauges are emitted
// in a deterministic order (local, frontier, fusion) for stable scrape diffs.
func TestLatencyPercentileGaugesSortedOutput(t *testing.T) {
	c := NewCollector()
	c.ObserveLatency("fusion", 300)
	c.ObserveLatency("local", 100)
	c.ObserveLatency("frontier", 200)

	gauges := c.LatencyPercentileGauges()
	routes := make([]string, 0, len(gauges)/3)
	seen := make(map[string]bool)
	for _, g := range gauges {
		if g.Name == "nexus_upstream_request_latency_p50_seconds" && !seen[g.Labels["route"]] {
			routes = append(routes, g.Labels["route"])
			seen[g.Labels["route"]] = true
		}
	}
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d: %v", len(routes), routes)
	}
	// Must be in order: local, frontier, fusion
	wantOrder := []string{"local", "frontier", "fusion"}
	for i, want := range wantOrder {
		if routes[i] != want {
			t.Errorf("route order[%d] = %q, want %q", i, routes[i], want)
		}
	}
}

// TestLatencyPercentileBufferPreciseDistribution tests exact percentile
// values for a known distribution where we can verify mathematically.
func TestLatencyPercentileBufferPreciseDistribution(t *testing.T) {
	buf := newLatencyPercentileBuffer(1000)

	// Values 1..1000, mean = 500.5
	for i := 1; i <= 1000; i++ {
		buf.Observe(float64(i))
	}

	p50, p95, p99 := buf.Perc()

	// True values: p50=500.5, p95=950.5, p99=990.5
	// 5% tolerance: p50 [475.475, 525.525], p95 [902.975, 998.025], p99 [940.975, 1000]
	if p50 < 475.475 || p50 > 525.525 {
		t.Errorf("p50 = %v, outside 5%% tolerance of 500.5", p50)
	}
	if p95 < 902.975 || p95 > 998.025 {
		t.Errorf("p95 = %v, outside 5%% tolerance of 950.5", p95)
	}
	if p99 < 940.975 || p99 > 1000.0 {
		t.Errorf("p99 = %v, outside 5%% tolerance of 990.5", p99)
	}
}
