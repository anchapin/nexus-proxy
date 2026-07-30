package observability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/concurrencylimit"
	"github.com/anchapin/nexus-proxy/internal/ratelimit"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// TestRenderPrometheusHasRequiredMetrics asserts every metric named in
// the issue's acceptance criteria is present in the rendered output
// with the correct TYPE line.
func TestRenderPrometheusHasRequiredMetrics(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local"})
	c.Submit(ObservabilityEvent{Route: "frontier", Error: "boom", EstimatedCostUSD: 0.02})

	var sb strings.Builder
	RenderPrometheus(&sb, c)

	out := sb.String()
	required := []string{
		"# TYPE nexus_requests_total counter",
		`nexus_requests_total{route="local"}`,
		"# TYPE nexus_errors_total counter",
		`nexus_errors_total{route="local"}`,
		"# TYPE nexus_rag_hits_total counter",
		"# TYPE nexus_toon_savings_tokens_total counter",
		"# TYPE nexus_estimated_cost_usd_total counter",
		"# TYPE nexus_request_duration_ms histogram",
		`nexus_request_duration_ms_bucket{route="local",le=`,
		"# TYPE nexus_ttft_ms histogram",
		`nexus_ttft_ms_bucket{route="local",le=`,
	}
	for _, want := range required {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusCounterValues checks the rendered counter values
// match the accumulated counts after a known sequence of submissions.
func TestRenderPrometheusCounterValues(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 5; i++ {
		c.Submit(ObservabilityEvent{Route: "frontier"})
	}

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	// requests_total{route="frontier"} must read 5.
	if !strings.Contains(out, `nexus_requests_total{route="frontier"} 5`) {
		t.Errorf("frontier counter not rendered as 5\n%s", out)
	}
	// local and fusion must read 0.
	if !strings.Contains(out, `nexus_requests_total{route="local"} 0`) {
		t.Errorf("local counter not rendered as 0\n%s", out)
	}
}

// TestRenderPrometheusHistogramFormat validates the structure of one
// histogram family: HELP/TYPE headers, a bucket line per finite bound
// in ascending order, the +Inf bucket, then _sum and _count.
func TestRenderPrometheusHistogramFormat(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 7})
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 30})

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	// The family must contain at least one finite-bucket line and
	// the +Inf tail. Exact le label values are asserted in
	// TestRenderHistogramLeLabels below.
	if !strings.Contains(out, `nexus_request_duration_ms_bucket{route="local",le=`) {
		t.Errorf("no finite bucket lines rendered\n%s", out)
	}
	if !strings.Contains(out, `nexus_request_duration_ms_bucket{route="local",le="+Inf"} 2`) {
		t.Errorf("+Inf bucket not rendered with cumulative count 2\n%s", out)
	}
	if !strings.Contains(out, `nexus_request_duration_ms_sum{route="local"} 37`) {
		t.Errorf("histogram sum not rendered as 37\n%s", out)
	}
	if !strings.Contains(out, `nexus_request_duration_ms_count{route="local"} 2`) {
		t.Errorf("histogram count not rendered as 2\n%s", out)
	}
}

// TestRenderHistogramLeLabels verifies the le label values are the
// numeric upper bounds (ascending) followed by +Inf.
func TestRenderHistogramLeLabels(t *testing.T) {
	h := NewHistogram([]float64{10, 100})
	h.Observe(5)
	h.Observe(50)
	h.Observe(999)

	var sb strings.Builder
	writeHistogramLabeled(&sb, "nexus_test_ms", "test histogram", "route", map[string]*Histogram{
		"local": h,
	})
	out := sb.String()

	wantLines := []string{
		`nexus_test_ms_bucket{route="local",le="10"} 1`,
		`nexus_test_ms_bucket{route="local",le="100"} 2`,
		`nexus_test_ms_bucket{route="local",le="+Inf"} 3`,
		`nexus_test_ms_sum{route="local"} 1054`,
		`nexus_test_ms_count{route="local"} 3`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// TestRenderPrometheusGauges confirms GaugeProvider samples appear in
// the output with HELP/TYPE headers and are sorted by name.
func TestRenderPrometheusGauges(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_vram_budget_tokens", Value: 4096},
			{Name: "nexus_ollama_healthy", Value: 1},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	// Sorted: nexus_ollama_healthy precedes nexus_vram_budget_tokens.
	healthyIdx := strings.Index(out, "nexus_ollama_healthy ")
	vramIdx := strings.Index(out, "nexus_vram_budget_tokens ")
	if healthyIdx < 0 || vramIdx < 0 {
		t.Fatalf("gauge samples missing\n%s", out)
	}
	if healthyIdx > vramIdx {
		t.Errorf("gauges not sorted by name: ollama_healthy (%d) after vram_budget (%d)", healthyIdx, vramIdx)
	}
	if !strings.Contains(out, "# TYPE nexus_ollama_healthy gauge") {
		t.Errorf("ollama_healthy TYPE line missing\n%s", out)
	}
	if !strings.Contains(out, "nexus_ollama_healthy 1") {
		t.Errorf("ollama_healthy value not rendered\n%s", out)
	}
	if !strings.Contains(out, "nexus_vram_budget_tokens 4096") {
		t.Errorf("vram_budget_tokens value not rendered\n%s", out)
	}
}

// TestRenderPrometheusDroppedCounterType verifies that gauge-supplied
// dropped counters are typed "counter" (not "gauge") in the output, so
// Prometheus does not reject them as a type flip across scrapes.
func TestRenderPrometheusDroppedCounterType(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_quality_dropped_total", Value: 7},
			{Name: "nexus_metrics_dropped_total", Value: 3},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if !strings.Contains(out, "# TYPE nexus_quality_dropped_total counter") {
		t.Errorf("quality_dropped_total not typed counter\n%s", out)
	}
	if !strings.Contains(out, "nexus_quality_dropped_total 7") {
		t.Errorf("quality_dropped_total value missing\n%s", out)
	}
	if !strings.Contains(out, "# TYPE nexus_metrics_dropped_total counter") {
		t.Errorf("metrics_dropped_total not typed counter\n%s", out)
	}
}

// TestRenderPrometheusTracingQueueDepthGauge (issue #596) verifies the
// nexus_tracing_queue_depth gauge is registered with type "gauge" and a
// HELP line, and that a supplied queue-depth value flows through to the
// rendered output. This is the registry/render contract; the end-to-end
// exporter → gauge wiring is covered by the tracing package.
func TestRenderPrometheusTracingQueueDepthGauge(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_tracing_queue_depth", Value: 0},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if !strings.Contains(out, "# HELP nexus_tracing_queue_depth ") {
		t.Errorf("tracing_queue_depth HELP line missing\n%s", out)
	}
	if !strings.Contains(out, "# TYPE nexus_tracing_queue_depth gauge") {
		t.Errorf("tracing_queue_depth not typed gauge\n%s", out)
	}
	if !strings.Contains(out, "nexus_tracing_queue_depth 0") {
		t.Errorf("tracing_queue_depth value 0 missing\n%s", out)
	}

	// A non-zero value (saturated buffer) must render identically.
	sb.Reset()
	provider = GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_tracing_queue_depth", Value: 42}}
	})
	RenderPrometheus(&sb, c, provider)
	if !strings.Contains(sb.String(), "nexus_tracing_queue_depth 42") {
		t.Errorf("tracing_queue_depth value 42 missing\n%s", sb.String())
	}
}

// TestRenderPrometheusJudgeQueueDepthGauge (issue #881) verifies the
// nexus_judge_queue_depth gauge is registered with type "gauge" and a
// HELP line, and that a supplied queue-depth value flows through to the
// rendered output.
func TestRenderPrometheusJudgeQueueDepthGauge(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_judge_queue_depth", Value: 0},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if !strings.Contains(out, "# HELP nexus_judge_queue_depth ") {
		t.Errorf("judge_queue_depth HELP line missing\n%s", out)
	}
	if !strings.Contains(out, "# TYPE nexus_judge_queue_depth gauge") {
		t.Errorf("judge_queue_depth not typed gauge\n%s", out)
	}
	if !strings.Contains(out, "nexus_judge_queue_depth 0") {
		t.Errorf("judge_queue_depth value 0 missing\n%s", out)
	}

	// A non-zero value (queue has samples) must render identically.
	sb.Reset()
	provider = GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_judge_queue_depth", Value: 7}}
	})
	RenderPrometheus(&sb, c, provider)
	if !strings.Contains(sb.String(), "nexus_judge_queue_depth 7") {
		t.Errorf("judge_queue_depth value 7 missing\n%s", sb.String())
	}
}

// TestRenderPrometheusTracingQueueDepthLiveExporter (issue #596) wires a
// real tracing.Exporter through the same GaugeProviderFunc closure shape
// that cmd/nexus/main.go uses and asserts the gauge reads 0 on a fresh
// exporter, then rises above zero once spans accumulate in the buffer
// while the collector hangs (consumer parked in a blocking flush).
func TestRenderPrometheusTracingQueueDepthLiveExporter(t *testing.T) {
	release := make(chan struct{})
	flushStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case flushStarted <- struct{}{}:
		default:
		}
		<-release // park the consumer so subsequent submits buffer up.
		w.WriteHeader(http.StatusOK)
	}))
	e := tracing.NewExporter(tracing.ExporterConfig{
		Endpoint:  srv.URL,
		QueueSize: 128,
	})
	// Deterministic teardown: unblock the collector first so the
	// exporter's blocked flush completes, then drain/close exporter,
	// then stop the test server.
	t.Cleanup(func() {
		close(release)
		if err := e.Close(); err != nil {
			t.Errorf("exporter close: %v", err)
		}
		srv.Close()
	})

	// Gauge provider closure, identical in shape to cmd/nexus/main.go.
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{
			Name:  "nexus_tracing_queue_depth",
			Value: float64(e.QueueDepth()),
		}}
	})
	render := func() string {
		var sb strings.Builder
		RenderPrometheus(&sb, NewCollector(), provider)
		return sb.String()
	}

	// Fresh exporter: no spans submitted, gauge must read 0.
	if out := render(); !strings.Contains(out, "nexus_tracing_queue_depth 0") {
		t.Fatalf("fresh exporter gauge not 0\n%s", out)
	}

	// Fill the first batch (batchCap=64) so the export goroutine enters
	// flush() and blocks on the hanging collector. This parks the only
	// consumer, so further submits accumulate in the queue channel.
	for i := 0; i < 64; i++ {
		e.Submit(&tracing.Span{
			TraceID: tracing.NewTraceID(),
			SpanID:  tracing.NewSpanID(),
			Name:    "warm",
		})
	}
	select {
	case <-flushStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first flush to reach the collector")
	}

	// Consumer is blocked; submit more spans that pile up in the buffer.
	const queued = 10
	for i := 0; i < queued; i++ {
		e.Submit(&tracing.Span{
			TraceID: tracing.NewTraceID(),
			SpanID:  tracing.NewSpanID(),
			Name:    "queued",
		})
	}

	depth := e.QueueDepth()
	if depth == 0 {
		t.Fatalf("expected buffered spans, QueueDepth=0")
	}
	out := render()
	if !strings.Contains(out, fmt.Sprintf("nexus_tracing_queue_depth %d", depth)) {
		t.Errorf("gauge does not reflect live queue depth %d\n%s", depth, out)
	}
}

// TestRenderPrometheusUnknownGaugeDefaultsToGauge confirms an unknown
// gauge name (not in the registry) still renders valid output with a
// default gauge type rather than being dropped.
func TestRenderPrometheusUnknownGaugeDefaultsToGauge(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_custom_gauge", Value: 11}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if !strings.Contains(out, "# TYPE nexus_custom_gauge gauge") {
		t.Errorf("unknown gauge not defaulted to gauge type\n%s", out)
	}
	if !strings.Contains(out, "nexus_custom_gauge 11") {
		t.Errorf("unknown gauge value missing\n%s", out)
	}
}

// TestRenderPrometheusNilProviderSkipped confirms a plain-nil provider
// in the slice does not panic and is simply ignored. (A typed-nil
// GaugeProviderFunc is a Go language footgun — main.go avoids it by
// always supplying non-nil closures that return empty slices when their
// backing source is disabled.)
func TestRenderPrometheusNilProviderSkipped(t *testing.T) {
	c := NewCollector()
	var nilProvider GaugeProvider // plain nil interface

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil provider panicked: %v", r)
		}
	}()
	var sb strings.Builder
	RenderPrometheus(&sb, c, nilProvider, GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_ollama_healthy", Value: 1}}
	}))
	if !strings.Contains(sb.String(), "nexus_requests_total") {
		t.Errorf("counter output missing despite nil provider\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "nexus_ollama_healthy 1") {
		t.Errorf("non-nil provider output missing\n%s", sb.String())
	}
}

// TestRenderPrometheusDeterministicOrder runs two scrapes back-to-back
// and asserts byte-identical output (gauges are sorted, counters are in
// a fixed order). Scrape-to-scrape diffs must be noise-free.
func TestRenderPrometheusDeterministicOrder(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "local", TotalLatencyMs: 10})
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_ollama_healthy", Value: 1},
			{Name: "nexus_vram_budget_tokens", Value: 4096},
			{Name: "nexus_judge_queue_depth", Value: 0},
		}
	})

	var first, second strings.Builder
	RenderPrometheus(&first, c, provider)
	RenderPrometheus(&second, c, provider)

	if first.String() != second.String() {
		t.Errorf("non-deterministic output across scrapes\n--- first ---\n%s\n--- second ---\n%s", first.String(), second.String())
	}
}

// TestRenderPrometheusHelpAndTypeHeaders sanity-checks the header
// convention (# HELP then # TYPE) on a representative counter.
func TestRenderPrometheusHelpAndTypeHeaders(t *testing.T) {
	c := NewCollector()
	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	helpIdx := strings.Index(out, "# HELP nexus_requests_total")
	typeIdx := strings.Index(out, "# TYPE nexus_requests_total")
	if helpIdx < 0 || typeIdx < 0 {
		t.Fatalf("HELP/TYPE headers missing for nexus_requests_total\n%s", out)
	}
	if helpIdx > typeIdx {
		t.Errorf("HELP line must precede TYPE line for nexus_requests_total")
	}
}

// TestRenderPrometheusCostFloat renders a fractional cost and confirms
// it appears as a decimal value, not truncated to integer.
func TestRenderPrometheusCostFloat(t *testing.T) {
	c := NewCollector()
	c.Submit(ObservabilityEvent{Route: "frontier", EstimatedCostUSD: 0.0042})

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	if !strings.Contains(out, "nexus_estimated_cost_usd_total 0.0042") {
		t.Errorf("fractional cost not rendered\n%s", out)
	}
}

// TestRenderPrometheusAuthCounters (issue #70/#1061) renders the auth
// counters and gauge after a known sequence of Inc* calls and
// confirms the per-outcome and per-client_ip labels and cumulative
// gauge are correct.
func TestRenderPrometheusAuthCounters(t *testing.T) {
	c := NewCollector()

	c.IncAuthAccepted("192.168.1.1")
	c.IncAuthAccepted("192.168.1.1")
	c.IncAuthAccepted("192.168.1.2")
	c.IncAuthRejectedInvalid("192.168.1.1")
	c.IncAuthRejectedMissing("192.168.1.3")
	c.IncAuthRejectedMissing("192.168.1.3")

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	wantLines := []string{
		"# TYPE nexus_auth_requests_total counter",
		`nexus_auth_requests_total{outcome="accepted",client_ip="192.168.1.1"} 2`,
		`nexus_auth_requests_total{outcome="accepted",client_ip="192.168.1.2"} 1`,
		`nexus_auth_requests_total{outcome="rejected_invalid",client_ip="192.168.1.1"} 1`,
		`nexus_auth_requests_total{outcome="rejected_missing",client_ip="192.168.1.3"} 2`,
		"# TYPE nexus_auth_authenticated_clients gauge",
		"nexus_auth_authenticated_clients 3",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output\n%s", want, out)
		}
	}
}

// TestRenderPrometheusRateLimitCounters (issue #70) renders the
// per-bucket allow/reject counters and the live bucket-count gauge.
func TestRenderPrometheusRateLimitCounters(t *testing.T) {
	c := NewCollector()

	c.IncRateLimit("global", true)
	c.IncRateLimit("global", false)
	c.IncRateLimit("global", false)
	c.IncRateLimit("per_client", true)
	c.IncRateLimit("per_client", true)

	// Live gauge from a provider so the renderer pulls it.
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_rate_limit_buckets", Value: 7}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	wantLines := []string{
		"# TYPE nexus_rate_limit_allowed_total counter",
		`nexus_rate_limit_allowed_total{scope="global"} 1`,
		`nexus_rate_limit_allowed_total{scope="per_client"} 2`,
		"# TYPE nexus_rate_limit_rejected_total counter",
		`nexus_rate_limit_rejected_total{scope="global"} 2`,
		`nexus_rate_limit_rejected_total{scope="per_client"} 0`,
		"# TYPE nexus_rate_limit_buckets gauge",
		"nexus_rate_limit_buckets 7",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output\n%s", want, out)
		}
	}
}

// TestRenderPrometheusBudgetCounters (issue #70) renders the
// recorded counter, exceeded counter, and live rolling-total gauge.
func TestRenderPrometheusBudgetCounters(t *testing.T) {
	c := NewCollector()

	c.AddBudgetRecorded(0.1234)
	c.AddBudgetRecorded(0.0566)
	c.IncBudgetExceeded()
	c.IncBudgetExceeded()
	c.IncBudgetExceeded()

	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{Name: "nexus_budget_spend_usd", Value: 0.18}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	wantLines := []string{
		"# TYPE nexus_budget_recorded_usd_total counter",
		"nexus_budget_recorded_usd_total 0.18",
		"# TYPE nexus_budget_exceeded_total counter",
		"nexus_budget_exceeded_total 3",
		"# TYPE nexus_budget_spend_usd gauge",
		"nexus_budget_spend_usd 0.18",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output\n%s", want, out)
		}
	}
}

// TestRenderPrometheusTLSCounters (issue #70) renders the
// accepted/rejected labelled family. Both samples stay at 0 unless
// the wiring layer (tls.Config.VerifyConnection) increments one,
// but the renderer must always emit the HELP/TYPE pair so the
// metric is scrapeable the moment TLS becomes active.
func TestRenderPrometheusTLSCounters(t *testing.T) {
	c := NewCollector()

	c.IncTLSAccepted()

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	wantLines := []string{
		"# TYPE nexus_tls_connections_total counter",
		`nexus_tls_connections_total{outcome="accepted"} 1`,
		`nexus_tls_connections_total{outcome="rejected"} 0`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output\n%s", want, out)
		}
	}
}

// TestRenderPrometheusRAGSimilarityHistogram (issue #447, #671) asserts the
// acceptance-criteria line "Score buckets are emitted for both index
// paths" and "Hits and misses record exactly once" — the renderer must
// emit nexus_rag_similarity_histogram buckets for each (path,
// outcome, threshold) tuple that has at least one observation, with
// cumulative counts matching the underlying histograms.
//
// Empty histograms (no observations) must be omitted from the
// rendered output so a freshly-booted scraper never sees a
// misleading 0-count series for an unobserved path.
func TestRenderPrometheusRAGSimilarityHistogram(t *testing.T) {
	c := NewCollector()
	c.ObserveRAGSimilarity("hnsw", "hit", 0.7, 0.55)
	c.ObserveRAGSimilarity("hnsw", "hit", 0.9, 0.55)
	c.ObserveRAGSimilarity("hnsw", "miss", 0.3, 0.55)
	c.ObserveRAGSimilarity("brute_force", "miss", 0.4, 0.55)

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	// HELP / TYPE header lines.
	wantHeaders := []string{
		"# HELP nexus_rag_similarity_histogram",
		"# TYPE nexus_rag_similarity_histogram histogram",
	}
	for _, want := range wantHeaders {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}

	// Bucket lines for each observed (path, outcome, threshold). Unobserved
	// pairs (e.g. brute_force/hit) must NOT appear in the output —
	// the renderer skips empty histograms so the scraper never sees
	// a zero-count series for a path with no data.
	wantBuckets := []string{
		`nexus_rag_similarity_histogram_bucket{path="hnsw",outcome="hit",threshold="0.55",le="0.8"} 1`,
		`nexus_rag_similarity_histogram_bucket{path="hnsw",outcome="hit",threshold="0.55",le="1"} 2`,
		`nexus_rag_similarity_histogram_bucket{path="hnsw",outcome="hit",threshold="0.55",le="+Inf"} 2`,
		`nexus_rag_similarity_histogram_sum{path="hnsw",outcome="hit",threshold="0.55"} 1.6`,
		`nexus_rag_similarity_histogram_count{path="hnsw",outcome="hit",threshold="0.55"} 2`,
		`nexus_rag_similarity_histogram_bucket{path="hnsw",outcome="miss",threshold="0.55",le="0.4"} 1`,
		`nexus_rag_similarity_histogram_count{path="hnsw",outcome="miss",threshold="0.55"} 1`,
		`nexus_rag_similarity_histogram_count{path="brute_force",outcome="miss",threshold="0.55"} 1`,
	}
	for _, want := range wantBuckets {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}

	// brute_force/hit has zero observations — must be omitted entirely.
	notWant := []string{
		`path="brute_force",outcome="hit"`,
	}
	for _, bad := range notWant {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected empty-series line %q in output\n%s", bad, out)
		}
	}
}

// TestRenderPrometheusRAGSimilarityEmpty (issue #447) verifies the
// "labels and cardinality are documented" half of the AC — when no
// observations have been recorded, the renderer must NOT emit the
// HELP/TYPE header so a freshly-booted scraper never sees an
// always-zero metric family.
func TestRenderPrometheusRAGSimilarityEmpty(t *testing.T) {
	c := NewCollector()
	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()
	if strings.Contains(out, "nexus_rag_similarity_histogram") {
		t.Errorf("fresh collector should not emit nexus_rag_similarity_histogram header; got:\n%s", out)
	}
}

// TestRenderPrometheusLocalConcurrencyGauges (issue #487) drives a real
// concurrencylimit.Limiter to in_flight=1 and asserts both local-route
// concurrency gauges render with correct HELP/TYPE headers and values.
func TestRenderPrometheusLocalConcurrencyGauges(t *testing.T) {
	c := NewCollector()
	limiter := concurrencylimit.New(2, 1<<30, func() int64 { return 8 << 30 })

	// Acquire one slot so in_flight=1; do not release until after render.
	release, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()

	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_local_concurrency_effective_slots", Value: float64(limiter.Effective())},
			{Name: "nexus_local_concurrency_in_flight", Value: float64(limiter.InFlight())},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	wantLines := []string{
		"# HELP nexus_local_concurrency_effective_slots",
		"# TYPE nexus_local_concurrency_effective_slots gauge",
		"nexus_local_concurrency_effective_slots 2",
		"# HELP nexus_local_concurrency_in_flight",
		"# TYPE nexus_local_concurrency_in_flight gauge",
		"nexus_local_concurrency_in_flight 1",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusSLMCacheGauges (issue #531) drives a real
// router.SLMCache to a known state and asserts both SLM cache gauges
// render with correct HELP/TYPE headers and values.
func TestRenderPrometheusSLMCacheGauges(t *testing.T) {
	c := NewCollector()
	cache := router.NewSLMCache(time.Hour, 10)
	ctx := context.Background()

	cache.Set(ctx, "prompt-a", router.RouteLocal)
	cache.Set(ctx, "prompt-b", router.RouteFrontier)

	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_slm_cache_entries", Value: float64(cache.Len())},
			{Name: "nexus_slm_cache_max_entries", Value: float64(cache.MaxEntries())},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	wantLines := []string{
		"# HELP nexus_slm_cache_entries",
		"# TYPE nexus_slm_cache_entries gauge",
		"nexus_slm_cache_entries 2",
		"# HELP nexus_slm_cache_max_entries",
		"# TYPE nexus_slm_cache_max_entries gauge",
		"nexus_slm_cache_max_entries 10",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusSLMCacheGaugesAbsentWhenDisabled (issue #531)
// verifies that when the cache is nil the gauge provider returns nil
// so neither series appears in a fresh scrape.
func TestRenderPrometheusSLMCacheGaugesAbsentWhenDisabled(t *testing.T) {
	c := NewCollector()

	var nilCache *router.SLMCache

	provider := GaugeProviderFunc(func() []GaugeSample {
		if nilCache == nil {
			return nil
		}
		return []GaugeSample{
			{Name: "nexus_slm_cache_entries", Value: float64(nilCache.Len())},
			{Name: "nexus_slm_cache_max_entries", Value: float64(nilCache.MaxEntries())},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if strings.Contains(out, "nexus_slm_cache_entries") {
		t.Errorf("nexus_slm_cache_entries should not appear when cache is nil\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "nexus_slm_cache_max_entries") {
		t.Errorf("nexus_slm_cache_max_entries should not appear when cache is nil\n--- output ---\n%s", out)
	}
}

// TestRenderPrometheusLocalCooldownGauge (issue #530) drives a real
// circuit.Cooldown to an active state and asserts the
// nexus_local_cooldown_active gauge renders with value 1 and correct
// HELP/TYPE headers.
func TestRenderPrometheusLocalCooldownGauge(t *testing.T) {
	c := NewCollector()
	clk := circuit.NewWithClock(10*time.Second, func() time.Time {
		return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	clk.RecordFailure()

	provider := GaugeProviderFunc(func() []GaugeSample {
		var v float64
		if clk.Active() {
			v = 1
		}
		return []GaugeSample{{Name: "nexus_local_cooldown_active", Value: v}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	checks := []string{
		"# HELP nexus_local_cooldown_active",
		"# TYPE nexus_local_cooldown_active gauge",
		"nexus_local_cooldown_active 1",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusLocalCooldownGaugeZeroWhenInactive verifies the gauge
// renders as 0 when the cooldown is not active.
func TestRenderPrometheusLocalCooldownGaugeZeroWhenInactive(t *testing.T) {
	c := NewCollector()
	clk := circuit.NewWithClock(10*time.Second, func() time.Time {
		return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	// No failure recorded — cooldown is inactive.

	provider := GaugeProviderFunc(func() []GaugeSample {
		var v float64
		if clk.Active() {
			v = 1
		}
		return []GaugeSample{{Name: "nexus_local_cooldown_active", Value: v}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if !strings.Contains(out, "nexus_local_cooldown_active 0") {
		t.Errorf("expected nexus_local_cooldown_active 0\ngot:\n%s", out)
	}
}

// TestRenderPrometheusLocalCooldownGaugeAbsentWhenDisabled (issue #530)
// verifies that when the cooldown is nil the gauge provider returns nil
// so the series does not appear in a fresh scrape.
func TestRenderPrometheusLocalCooldownGaugeAbsentWhenDisabled(t *testing.T) {
	c := NewCollector()

	var nilCooldown *circuit.Cooldown

	provider := GaugeProviderFunc(func() []GaugeSample {
		if nilCooldown == nil {
			return nil
		}
		var v float64
		if nilCooldown.Active() {
			v = 1
		}
		return []GaugeSample{{Name: "nexus_local_cooldown_active", Value: v}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if strings.Contains(out, "nexus_local_cooldown_active") {
		t.Errorf("nexus_local_cooldown_active should not appear when cooldown is nil\n--- output ---\n%s", out)
	}
}

// TestBuildInfoGauge (issue #529) verifies the nexus_build_info gauge
// renders with the correct HELP/TYPE headers, labels (version, commit,
// go_version), and a constant value of 1.
func TestBuildInfoGauge(t *testing.T) {
	c := NewCollector()
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{{
			Name: "nexus_build_info",
			Labels: map[string]string{
				"version":    "v1.2.3",
				"commit":     "abc1234",
				"go_version": "go1.22.0",
			},
			Value: 1,
		}}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	checks := []string{
		"# HELP nexus_build_info Build metadata for the running nexus-proxy binary (issue #529). Always 1.",
		"# TYPE nexus_build_info gauge",
		`nexus_build_info{commit="abc1234",go_version="go1.22.0",version="v1.2.3"} 1`,
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}

	// Value must be exactly 1 (Prometheus build-info convention).
	if !strings.Contains(out, `nexus_build_info{`) || !strings.Contains(out, " 1") {
		t.Errorf("build_info value should be 1\ngot:\n%s", out)
	}
}

// TestRenderPrometheusRateLimitUtilizationHistogram (issue #746) asserts
// that the nexus_rate_limit_bucket_utilization histogram is rendered with
// the correct HELP/TYPE headers and bucket lines when observations have
// been recorded. Empty histograms must be omitted from the output.
func TestRenderPrometheusRateLimitUtilizationHistogram(t *testing.T) {
	c := NewCollector()
	c.ObserveRateLimitUtilization("abc123", 0.9) // 75-100% quartile
	c.ObserveRateLimitUtilization("abc123", 0.6) // 50-75% quartile
	c.ObserveRateLimitUtilization("abc123", 0.3) // 25-50% quartile
	c.ObserveRateLimitUtilization("def456", 0.1) // 0-25% quartile

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	wantHeaders := []string{
		"# HELP nexus_rate_limit_bucket_utilization",
		"# TYPE nexus_rate_limit_bucket_utilization histogram",
	}
	for _, want := range wantHeaders {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}

	// Bucket lines for abc123 (3 observations: 0.9, 0.6, 0.3).
	// Cumulative: le=0.25: 0, le=0.5: 1, le=0.75: 2, le=1: 3, +Inf: 3
	wantBuckets := []string{
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="abc123",le="0.25"} 0`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="abc123",le="0.5"} 1`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="abc123",le="0.75"} 2`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="abc123",le="1"} 3`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="abc123",le="+Inf"} 3`,
		`nexus_rate_limit_bucket_utilization_sum{bucket_id="abc123"} 1.8`,
		`nexus_rate_limit_bucket_utilization_count{bucket_id="abc123"} 3`,
		// def456 has only 1 observation (0.1 <= 0.25).
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="def456",le="0.25"} 1`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="def456",le="0.5"} 1`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="def456",le="0.75"} 1`,
		`nexus_rate_limit_bucket_utilization_bucket{bucket_id="def456",le="1"} 1`,
		`nexus_rate_limit_bucket_utilization_count{bucket_id="def456"} 1`,
	}
	for _, want := range wantBuckets {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusRateLimitUtilizationEmpty verifies that when no
// observations have been recorded, the renderer must NOT emit the
// HELP/TYPE header for nexus_rate_limit_bucket_utilization.
func TestRenderPrometheusRateLimitUtilizationEmpty(t *testing.T) {
	c := NewCollector()
	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()
	if strings.Contains(out, "nexus_rate_limit_bucket_utilization") {
		t.Errorf("fresh collector should not emit nexus_rate_limit_bucket_utilization; got:\n%s", out)
	}
}

// TestRenderPrometheusAuthLimiterGauges (issue #744) drives a real
// AuthLimiter to known states and asserts both auth limiter gauges render
// with correct HELP/TYPE headers and values.
func TestRenderPrometheusAuthLimiterGauges(t *testing.T) {
	c := NewCollector()
	resolver := ratelimit.NewClientIPResolver(nil)
	al := ratelimit.NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	// No failures tracked yet.
	provider := GaugeProviderFunc(func() []GaugeSample {
		return []GaugeSample{
			{Name: "nexus_auth_limiter_tracked_ips", Value: float64(al.BucketCount())},
			{Name: "nexus_auth_limiter_blocked_ips", Value: float64(al.BlockedCount())},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	wantLines := []string{
		"# HELP nexus_auth_limiter_tracked_ips",
		"# TYPE nexus_auth_limiter_tracked_ips gauge",
		"nexus_auth_limiter_tracked_ips 0",
		"# HELP nexus_auth_limiter_blocked_ips",
		"# TYPE nexus_auth_limiter_blocked_ips gauge",
		"nexus_auth_limiter_blocked_ips 0",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}

	// Record failures for two IPs; one reaches burst threshold.
	al.RecordFailure("10.0.0.1", "missing")
	al.RecordFailure("10.0.0.1", "missing")
	al.RecordFailure("10.0.0.1", "missing") // burst reached → blocked
	al.RecordFailure("10.0.0.2", "missing") // below burst → not blocked

	sb.Reset()
	RenderPrometheus(&sb, c, provider)
	out = sb.String()

	trackedWant := []string{
		"nexus_auth_limiter_tracked_ips 2",
		"nexus_auth_limiter_blocked_ips 1",
	}
	for _, want := range trackedWant {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q after failures\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderPrometheusAuthLimiterGaugesAbsentWhenDisabled verifies that when
// the auth limiter is nil (not configured) the gauge provider returns nil so
// neither series appears in a fresh scrape.
func TestRenderPrometheusAuthLimiterGaugesAbsentWhenDisabled(t *testing.T) {
	c := NewCollector()

	var nilAL *ratelimit.AuthLimiter

	provider := GaugeProviderFunc(func() []GaugeSample {
		if nilAL == nil {
			return nil
		}
		return []GaugeSample{
			{Name: "nexus_auth_limiter_tracked_ips", Value: float64(nilAL.BucketCount())},
			{Name: "nexus_auth_limiter_blocked_ips", Value: float64(nilAL.BlockedCount())},
		}
	})

	var sb strings.Builder
	RenderPrometheus(&sb, c, provider)
	out := sb.String()

	if strings.Contains(out, "nexus_auth_limiter_tracked_ips") {
		t.Errorf("nexus_auth_limiter_tracked_ips should not appear when limiter is nil\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "nexus_auth_limiter_blocked_ips") {
		t.Errorf("nexus_auth_limiter_blocked_ips should not appear when limiter is nil\n--- output ---\n%s", out)
	}
}

// TestRenderPrometheusAuthLimiterBlockedCounter verifies that the
// nexus_auth_limiter_blocked_total counter is rendered correctly
// with reason labels after IncAuthBlocked calls (issues #831/#937).
func TestRenderPrometheusAuthLimiterBlockedCounter(t *testing.T) {
	c := NewCollector()

	// 2 missing, 3 invalid
	c.IncAuthBlocked("missing")
	c.IncAuthBlocked("missing")
	c.IncAuthBlocked("invalid")
	c.IncAuthBlocked("invalid")
	c.IncAuthBlocked("invalid")

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	wantLines := []string{
		"# HELP nexus_auth_limiter_blocked_total",
		"# TYPE nexus_auth_limiter_blocked_total counter",
		`nexus_auth_limiter_blocked_total{reason="missing"} 2`,
		`nexus_auth_limiter_blocked_total{reason="invalid"} 3`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- output ---\n%s", want, out)
		}
	}
}
