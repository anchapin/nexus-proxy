// Prometheus text-exposition renderer (issue #40). Emits the standard
// format (# HELP / # TYPE / sample lines) that Prometheus and any
// compatible scraper ingest. See
// https://prometheus.io/docs/instrumenting/exposition_formats/.
//
// The renderer is split from collector.go so the hot path (Submit) has
// zero rendering dependencies; only the scrape handler pulls this in.

package observability

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// GaugeSample is one live gauge reading captured at scrape time. Name
// is the full Prometheus metric name (e.g. "nexus_ollama_healthy").
type GaugeSample struct {
	Name  string
	Value float64
}

// GaugeProvider returns live gauge readings at scrape time. The
// /metrics handler calls Gauges() once per scrape; implementations read
// the latest state from their backing source (health poller, VRAM
// probe, judge/quality worker pools, dropped counters) and return a
// slice of samples. Implementations must be safe to call concurrently
// with the request path and must not block on I/O — the whole scrape
// should complete in well under a millisecond.
//
// main.go composes several GaugeProviderFunc closures (one per backing
// source) and passes them to RenderPrometheus.
type GaugeProvider interface {
	Gauges() []GaugeSample
}

// GaugeProviderFunc adapts a plain function to the GaugeProvider
// interface so wiring from main.go stays a one-liner.
type GaugeProviderFunc func() []GaugeSample

// Gauges implements GaugeProvider.
func (f GaugeProviderFunc) Gauges() []GaugeSample { return f() }

// metricMeta carries the HELP text and Prometheus type for a metric
// family. Used so the renderer can emit well-formed HELP/TYPE lines for
// gauges supplied by providers without each provider having to repeat
// the metadata.
type metricMeta struct {
	help string
	typ  string // "counter" | "gauge" | "histogram"
}

// gaugeMeta is the registry of known gauge/counter metric names
// supplied by GaugeProviders. Names not present default to type
// "gauge" with the bare name as HELP so unknown providers still render
// valid output.
var gaugeMeta = map[string]metricMeta{
	"nexus_ollama_healthy": {
		help: "1 if the local Ollama endpoint is healthy (circuit breaker closed), 0 otherwise.",
		typ:  "gauge",
	},
	"nexus_ollama_failure_count": {
		help: "Current consecutive failed Ollama probes since the last success.",
		typ:  "gauge",
	},
	"nexus_vram_budget_tokens": {
		help: "Current dynamic VRAM token budget from the latest probe (0 = static fallback in use).",
		typ:  "gauge",
	},
	"nexus_vram_free_bytes": {
		help: "Free VRAM in bytes reported by the latest probe (0 if the probe does not measure VRAM).",
		typ:  "gauge",
	},
	"nexus_judge_queue_depth": {
		help: "Number of buffered, unjudged samples waiting in the judge evaluator queue.",
		typ:  "gauge",
	},
	"nexus_judge_concurrency": {
		help: "Configured maximum parallel judge calls.",
		typ:  "gauge",
	},
	"nexus_quality_queue_depth": {
		help: "Number of buffered, unverified edits waiting in the quality verifier queue.",
		typ:  "gauge",
	},
	"nexus_quality_concurrency": {
		help: "Configured maximum parallel quality verifications.",
		typ:  "gauge",
	},
	"nexus_quality_dropped_total": {
		help: "Total quality events dropped because the verifier queue was full.",
		typ:  "counter",
	},
	"nexus_metrics_dropped_total": {
		help: "Total metrics records dropped because the SQLite write buffer was full.",
		typ:  "counter",
	},
	"nexus_telemetry_dropped_total": {
		help: "Total telemetry records dropped because the JSONL write buffer was full.",
		typ:  "counter",
	},
	"nexus_tracing_dropped_total": {
		help: "Total trace spans dropped because the exporter buffer was full.",
		typ:  "counter",
	},
	// Issue #70: live middleware gauges. These come from backing
	// sources (rate-limit bucket count, budget running total) at
	// scrape time rather than from per-request events, so they are
	// supplied as gauge providers from main.go.
	"nexus_rate_limit_buckets": {
		help: "Current number of per-client rate-limit buckets held in memory.",
		typ:  "gauge",
	},
	"nexus_budget_spend_usd": {
		help: "Rolling 24-hour spend in USD from the daily frontier budget tracker.",
		typ:  "gauge",
	},
}

// RenderPrometheus writes the full /metrics body in Prometheus
// text-exposition format. Counters and histograms come from the
// Collector; gauges come from the supplied providers (each called once
// at scrape time). Output is deterministic: metric families are emitted
// in a fixed order and gauge samples are sorted by name, so
// scrape-to-scrape diffs are stable and friendly to human inspection.
//
// RenderPrometheus performs no allocation on the hot path — it is only
// called from the scrape handler. With 10,000 accumulated samples the
// handler completes in well under a millisecond because every read is a
// plain atomic load with no lock contention.
func RenderPrometheus(w io.Writer, c *Collector, providers ...GaugeProvider) {
	if c == nil {
		return
	}

	// --- Counters -------------------------------------------------------

	writeCounterLabeled(w, "nexus_requests_total",
		"Total proxied requests by route (local/frontier/fusion).",
		"route", []labelSample{
			{value: "local", n: c.requestsLocal.Load()},
			{value: "frontier", n: c.requestsFrontier.Load()},
			{value: "fusion", n: c.requestsFusion.Load()},
		})

	writeCounterLabeled(w, "nexus_errors_total",
		"Total proxied requests that returned an upstream error, by route.",
		"route", []labelSample{
			{value: "local", n: c.errorsLocal.Load()},
			{value: "frontier", n: c.errorsFrontier.Load()},
			{value: "fusion", n: c.errorsFusion.Load()},
		})
	writeCounter(w, "nexus_rag_hits_total",
		"Total proxied requests where a RAG few-shot snippet was injected.", c.ragHitsTotal.Load())
	writeCounter(w, "nexus_rag_misses_total",
		"Total proxied requests where no RAG snippet met the similarity threshold.", c.ragMissesTotal.Load())
	writeCounter(w, "nexus_toon_compressed_total",
		"Total proxied requests whose JSON-array blocks were TOON-compressed.", c.toonCompressedTotal.Load())
	writeCounter(w, "nexus_degraded_total",
		"Total proxied requests that ran in degraded mode (local Ollama unreachable).", c.degradedTotal.Load())
	writeCounter(w, "nexus_input_tokens_total",
		"Cumulative estimated input tokens across all proxied requests.", c.inputTokensTotal.Load())
	writeCounter(w, "nexus_output_tokens_total",
		"Cumulative estimated output tokens across all proxied requests.", c.outputTokensTotal.Load())
	writeCounter(w, "nexus_toon_savings_tokens_total",
		"Cumulative tokens saved by TOON compression across all proxied requests.", c.toonSavingsTokensTotal.Load())

	// Cumulative frontier cost (float-valued counter).
	writeMeta(w, "nexus_estimated_cost_usd_total",
		"Cumulative estimated frontier cost in USD across all proxied requests.", "counter")
	fmt.Fprintf(w, "nexus_estimated_cost_usd_total %s\n", formatFloat(c.EstimatedCostUSD()))

	// --- Middleware instrumentation (issue #70) --------------------------

	// Auth counters are emitted with one sample line per outcome label
	// (accepted / rejected_invalid / rejected_missing). The fourth
	// outcome "exempt" is intentionally omitted: an exempt request is
	// not an authentication decision and would dilute the per-decision
	// counts. The AuthAuthenticatedClients gauge mirrors the
	// accepted counter so operators can chart a clean "successful
	// authentications" timeline.
	writeCounterLabeled(w, "nexus_auth_requests_total",
		"Authentication decisions by outcome (issue #70).",
		"outcome", []labelSample{
			{value: "accepted", n: c.authAccepted.Load()},
			{value: "rejected_invalid", n: c.authRejectedInvalid.Load()},
			{value: "rejected_missing", n: c.authRejectedMissing.Load()},
		})

	// Rate-limit counters are emitted as two labelled families so the
	// {scope, allowed} matrix is one scrape away. scope values are
	// "global" or "per_client"; the limiter never emits "both" — when
	// both buckets are active the deny from either side wins and the
	// single failing bucket is named.
	writeCounterLabeled(w, "nexus_rate_limit_allowed_total",
		"Requests that passed the rate limiter, by bucket scope.",
		"scope", []labelSample{
			{value: "global", n: c.rateLimitAllowedGlobal.Load()},
			{value: "per_client", n: c.rateLimitAllowedPerClient.Load()},
		})
	writeCounterLabeled(w, "nexus_rate_limit_rejected_total",
		"Requests rejected (429) by the rate limiter, by bucket scope.",
		"scope", []labelSample{
			{value: "global", n: c.rateLimitRejectedGlobal.Load()},
			{value: "per_client", n: c.rateLimitRejectedPerClient.Load()},
		})

	// Budget counters. nexus_budget_recorded_usd_total is a cumulative
	// float-valued counter mirroring SpendTracker.Record calls;
	// nexus_budget_exceeded_total counts WouldExceed == true events.
	writeMeta(w, "nexus_budget_recorded_usd_total",
		"Cumulative USD recorded by the rolling daily frontier budget tracker.", "counter")
	fmt.Fprintf(w, "nexus_budget_recorded_usd_total %s\n", formatFloat(c.BudgetRecordedUSD()))

	writeCounter(w, "nexus_budget_exceeded_total",
		"Number of frontier requests rejected by the daily budget gate.",
		c.budgetExceededTotal.Load())

	// TLS handshake counters. Optional: only non-zero when the operator
	// configured TLS (NEXUS_TLS_CERT + NEXUS_TLS_KEY); otherwise both
	// samples stay at 0.
	writeCounterLabeled(w, "nexus_tls_connections_total",
		"TLS handshake outcomes (issue #70; optional, only non-zero with NEXUS_TLS_CERT).",
		"outcome", []labelSample{
			{value: "accepted", n: c.tlsConnectionsAccepted.Load()},
			{value: "rejected", n: c.tlsConnectionsRejected.Load()},
		})

	// Auth gauge: cumulative accepted authentications. The metric name
	// carries "_clients" per the issue spec; semantically this is a
	// monotonic counter that operators usually want charted as a
	// monotonically-rising line (Prometheus treats it as gauge so a
	// rate() function gives authentications-per-second).
	writeMeta(w, "nexus_auth_authenticated_clients",
		"Cumulative accepted authentications (issue #70).", "gauge")
	fmt.Fprintf(w, "nexus_auth_authenticated_clients %d\n", c.AuthAuthenticatedClients())

	// --- Histograms -----------------------------------------------------

	writeHistogramLabeled(w, "nexus_request_duration_ms",
		"End-to-end request duration in milliseconds, from body read to final flush, by route.",
		"route", map[string]*Histogram{
			"local":    c.latencyLocal,
			"frontier": c.latencyFrontier,
			"fusion":   c.latencyFusion,
		})
	writeHistogramLabeled(w, "nexus_ttft_ms",
		"Time to first token in milliseconds (0 / unobserved for non-streaming responses), by route.",
		"route", map[string]*Histogram{
			"local":    c.ttftLocal,
			"frontier": c.ttftFrontier,
			"fusion":   c.ttftFusion,
		})

	// --- Gauges (live readings from providers) --------------------------

	gauges := collectGauges(providers)
	sort.Slice(gauges, func(i, j int) bool { return gauges[i].Name < gauges[j].Name })
	seen := make(map[string]bool, len(gauges))
	for _, g := range gauges {
		if !seen[g.Name] {
			meta, ok := gaugeMeta[g.Name]
			if !ok {
				meta = metricMeta{help: g.Name, typ: "gauge"}
			}
			writeMeta(w, g.Name, meta.help, meta.typ)
			seen[g.Name] = true
		}
		fmt.Fprintf(w, "%s %s\n", g.Name, formatFloat(g.Value))
	}
}

// labelSample pairs a label value with its counter reading for a
// labelled counter family (e.g. the route dimension on
// nexus_requests_total).
type labelSample struct {
	value string
	n     uint64
}

// writeMeta emits the # HELP and # TYPE header lines for one metric
// family. Called once per family before its sample lines.
func writeMeta(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

// writeCounter emits a single-sample unlabelled counter family.
func writeCounter(w io.Writer, name, help string, v uint64) {
	writeMeta(w, name, help, "counter")
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// writeCounterLabeled emits a counter family with one label dimension.
// Each labelSample becomes its own sample line. The label values are
// emitted in the order given (callers pass them sorted by relevance).
func writeCounterLabeled(w io.Writer, name, help, label string, samples []labelSample) {
	writeMeta(w, name, help, "counter")
	for _, s := range samples {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, s.value, s.n)
	}
}

// writeHistogramLabeled emits a histogram family with one label dimension.
// Each route gets its own bucket lines, _sum, and _count.
// Routes are emitted in a fixed order (local, frontier, fusion) for
// deterministic output.
func writeHistogramLabeled(w io.Writer, name, help, label string, histograms map[string]*Histogram) {
	writeMeta(w, name, help, "histogram")
	// Fixed route order for deterministic output.
	for _, route := range []string{"local", "frontier", "fusion"} {
		h, ok := histograms[route]
		if !ok || h == nil {
			continue
		}
		cum, upperBounds, sum, count := h.Snapshot()
		for i, ub := range upperBounds {
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, route, formatFloat(ub), cum[i])
		}
		fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, route, "+Inf", cum[len(upperBounds)])
		fmt.Fprintf(w, "%s_sum{%s=%q} %s\n", name, label, route, formatFloat(sum))
		fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, label, route, count)
	}
}

// writeHistogram emits a histogram family: one bucket line per finite
// upper bound plus the +Inf bucket, then _sum and _count.
// Kept for backward compatibility with tests and single-route use cases.
func writeHistogram(w io.Writer, name, help string, h *Histogram) {
	if h == nil {
		return
	}
	writeMeta(w, name, help, "histogram")
	cum, upperBounds, sum, count := h.Snapshot()
	for i, ub := range upperBounds {
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatFloat(ub), cum[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, "+Inf", cum[len(upperBounds)])
	fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(sum))
	fmt.Fprintf(w, "%s_count %d\n", name, count)
}

// collectGauges flattens the samples from every non-nil provider into a
// single slice. Nil providers are skipped so main.go can pass a typed
// nil GaugeProviderFunc without panicking.
func collectGauges(providers []GaugeProvider) []GaugeSample {
	var out []GaugeSample
	for _, p := range providers {
		if p == nil {
			continue
		}
		out = append(out, p.Gauges()...)
	}
	return out
}

// formatFloat renders v in the most compact form Prometheus accepts:
// integers print without a decimal point, fractional values use 'g'
// precision, and the special values +Inf / -Inf / NaN use the spellings
// the exposition spec requires.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	// Whole numbers within int64 range print without a decimal point
	// (Prometheus accepts both, but integer output is friendlier for
	// queue depths, token counts, and health flags).
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
