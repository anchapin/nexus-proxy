package observability

import (
	"strings"
	"testing"
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

// TestRenderPrometheusAuthCounters (issue #70) renders the auth
// counters and gauge after a known sequence of Inc* calls and
// confirms the per-outcome labels and cumulative gauge are correct.
func TestRenderPrometheusAuthCounters(t *testing.T) {
	c := NewCollector()

	c.IncAuthAccepted()
	c.IncAuthAccepted()
	c.IncAuthAccepted()
	c.IncAuthRejectedInvalid()
	c.IncAuthRejectedMissing()
	c.IncAuthRejectedMissing()

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	wantLines := []string{
		"# TYPE nexus_auth_requests_total counter",
		`nexus_auth_requests_total{outcome="accepted"} 3`,
		`nexus_auth_requests_total{outcome="rejected_invalid"} 1`,
		`nexus_auth_requests_total{outcome="rejected_missing"} 2`,
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

// TestRenderPrometheusVRAMGauges verifies that VRAM gauges are rendered
// when a callback is configured (issue #394).
func TestRenderPrometheusVRAMGauges(t *testing.T) {
	c := NewCollector()
	c.SetVramGaugeFunc(func() map[string]int64 {
		return map[string]int64{
			"card0": int64(8) << 30, // 8 GiB free
			"card1": int64(4) << 30, // 4 GiB free
		}
	}, int64(2)<<30) // 2 GiB per slot

	var sb strings.Builder
	RenderPrometheus(&sb, c)
	out := sb.String()

	// Check TYPE lines are present for both VRAM metric families.
	vramTypes := []string{
		"# TYPE nexus_vram_free_bytes gauge",
		"# TYPE nexus_vram_slots_available gauge",
	}
	for _, want := range vramTypes {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}

	// Check HELP lines.
	vramHelps := []string{
		"# HELP nexus_vram_free_bytes Free VRAM in bytes per GPU",
		"# HELP nexus_vram_slots_available Available local-route concurrency slots",
	}
	for _, want := range vramHelps {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}

	// Check sample lines with gpu_id labels.
	samples := []string{
		`nexus_vram_free_bytes{gpu_id="card0"}`,
		`nexus_vram_free_bytes{gpu_id="card1"}`,
		`nexus_vram_slots_available{gpu_id="card0"} 4`, // 8 GiB / 2 GiB = 4
		`nexus_vram_slots_available{gpu_id="card1"} 2`, // 4 GiB / 2 GiB = 2
	}
	for _, want := range samples {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
}
