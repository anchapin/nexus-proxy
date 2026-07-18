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
