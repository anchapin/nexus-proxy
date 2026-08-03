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
	"strings"

	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// GaugeSample is one live gauge reading captured at scrape time. Name
// is the full Prometheus metric name (e.g. "nexus_ollama_healthy").
// Labels are optional key-value pairs rendered as Prometheus label
// syntax; nil or empty labels produce an unlabelled sample line.
type GaugeSample struct {
	Name   string
	Labels map[string]string
	Value  float64
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
	"nexus_quality_dropped_ring_capacity": {
		help: "Maximum number of dropped events retained in the ring buffer for inspection (issue #1066).",
		typ:  "gauge",
	},
	"nexus_judge_dropped_total": {
		help: "Total judge samples dropped because the judge queue was full (issue #892).",
		typ:  "counter",
	},
	"nexus_judge_frontier_sampled_total": {
		help: "Total frontier completions sampled for judge evaluation (issue #1162).",
		typ:  "counter",
	},
	"nexus_judge_adaptive_sample_rate": {
		help: "Current effective adaptive judge sample rate (issue #1232). 0 when adaptive sampling is disabled.",
		typ:  "gauge",
	},
	"nexus_confidence_store_rows_total": {
		help: "Current number of rows in the routing_outcomes confidence store table (issue #834).",
		typ:  "gauge",
	},
	"nexus_metrics_dropped_total": {
		help: "Total metrics records dropped because the SQLite write buffer was full.",
		typ:  "counter",
	},
	"nexus_metrics_prune_last_rows": {
		help: "Rows removed by the most recent metrics retention prune pass (issue #483).",
		typ:  "gauge",
	},
	"nexus_metrics_prune_last_timestamp_seconds": {
		help: "Unix timestamp of the most recent metrics retention prune pass (issue #483).",
		typ:  "gauge",
	},
	"nexus_telemetry_dropped_total": {
		help: "Total telemetry records dropped because the JSONL write buffer was full.",
		typ:  "counter",
	},
	"nexus_telemetry_write_errors_total": {
		help: "Total write/flush error events in the JSONL recorder background loop (issue #795). Each increment corresponds to one failed disk write or flush that dropped buffered records.",
		typ:  "counter",
	},
	"nexus_telemetry_rotations_total": {
		help: "Total telemetry file rotations triggered by the NEXUS_TELEMETRY_MAX_BYTES size cap.",
		typ:  "counter",
	},
	"nexus_tracing_dropped_total": {
		help: "Total trace spans dropped because the exporter buffer was full.",
		typ:  "counter",
	},
	"nexus_tracing_flush_failures_total": {
		help: "Total trace batches that failed to POST to the collector (HTTP 4xx/5xx, timeout, or transport error). Each failure drops up to 64 spans — distinct from nexus_tracing_dropped_total, which counts per-span buffer-full sheds at submit time (issue #484).",
		typ:  "counter",
	},
	"nexus_tracing_queue_depth": {
		help: "Current number of trace spans waiting in the exporter buffer (issue #596). Gives operators early warning of exporter saturation before nexus_tracing_dropped_total begins incrementing.",
		typ:  "gauge",
	},
	"nexus_tracing_batch_size": {
		help: "Configured OTLP tracing batch size (issue #826). Shows the cap set via NEXUS_TRACING_BATCH_SIZE, not the current fill level.",
		typ:  "gauge",
	},
	// Issue #70: live middleware gauges. These come from backing
	// sources (rate-limit bucket count, budget running total) at
	// scrape time rather than from per-request events, so they are
	// supplied as gauge providers from main.go.
	"nexus_rate_limit_buckets": {
		help: "Current number of per-client rate-limit buckets held in memory.",
		typ:  "gauge",
	},
	// Issue #746: per-client rate-limit bucket utilization histogram.
	"nexus_rate_limit_bucket_utilization": {
		help: "Fractional token utilization (tokens/burst) at moment of acquisition per client bucket, bucketed by quartile (issue #746).",
		typ:  "histogram",
	},
	"nexus_budget_spend_usd": {
		help: "Rolling 24-hour spend in USD from the daily frontier budget tracker.",
		typ:  "gauge",
	},
	// Circuit breaker gauges (issue #304). Each circuit emits three
	// labelled metrics: state (0=closed, 1=half_open, 2=open),
	// failures_total, and last_failure_seconds.
	"nexus_circuit_breaker_state": {
		help: "Circuit breaker state: 0=closed, 1=half_open, 2=open (issue #304).",
		typ:  "gauge",
	},
	"nexus_circuit_breaker_failures_total": {
		help: "Total number of failure events recorded for this circuit breaker (issue #304).",
		typ:  "counter",
	},
	"nexus_circuit_breaker_last_failure_seconds": {
		help: "Unix timestamp of the last failure recorded for this circuit breaker (issue #304).",
		typ:  "gauge",
	},
	// Local-route concurrency limiter gauges (issue #487). The VRAM-aware
	// limiter shrinks its effective slot count dynamically from the latest
	// probe snapshot; these gauges let operators see the ceiling, how many
	// slots are in use, and whether requests are saturating the local path.
	"nexus_local_concurrency_effective_slots": {
		help: "Current effective slot count for the VRAM-aware local-route concurrency limiter (issue #487).",
		typ:  "gauge",
	},
	"nexus_local_concurrency_in_flight": {
		help: "Number of held slots in the local-route concurrency limiter (issue #487).",
		typ:  "gauge",
	},
	// Embedder circuit breaker failures (issue #423).
	"nexus_embedder_failures_total": {
		help: "Total number of circuit breaker trip events for embedder kinds (issue #423).",
		typ:  "counter",
	},
	// RAG embedder circuit breaker state metrics (issue #886).
	"nexus_rag_circuit_state": {
		help: "RAG embedder circuit breaker state: 0=closed, 1=half_open, 2=open (issue #886).",
		typ:  "gauge",
	},
	"nexus_rag_circuit_trip_total": {
		help: "Total number of RAG embedder circuit breaker trip events (issue #886).",
		typ:  "counter",
	},
	"nexus_rag_circuit_recover_total": {
		help: "Total number of RAG embedder circuit breaker recovery events (issue #886).",
		typ:  "counter",
	},
	"nexus_rag_circuit_failure_count": {
		help: "Current consecutive failure count for RAG embedder circuit breakers (issue #886).",
		typ:  "gauge",
	},
	// RAG semantic dedup (issue #1243).
	"nexus_rag_dedup_skipped_total": {
		help: "Total number of RAG chunks skipped at index time because they were too similar to existing chunks (issue #1243).",
		typ:  "counter",
	},
	// SLM decision cache gauges (issue #531).
	"nexus_slm_cache_entries": {
		help: "Current number of entries in the SLM decision cache (issue #531).",
		typ:  "gauge",
	},
	"nexus_slm_cache_max_entries": {
		help: "Configured maximum entry capacity of the SLM decision cache (issue #531).",
		typ:  "gauge",
	},
	"nexus_slm_cache_stale_entries": {
		help: "Number of entries in the SLM decision cache that have passed their TTL but have not yet been evicted (issue #835).",
		typ:  "gauge",
	},
	// Local-route cooldown gauge (issue #530).
	"nexus_local_cooldown_active": {
		help: "1 when the local-route cooldown is active (a cascade failure was recorded and the window has not expired); 0 otherwise (issue #530). Absent from /metrics when the cooldown is disabled (NEXUS_LOCAL_COOLDOWN<=0).",
		typ:  "gauge",
	},
	// Build info gauge (issue #529). Static metadata — value is always 1.
	"nexus_build_info": {
		help: "Build metadata for the running nexus-proxy binary (issue #529). Always 1.",
		typ:  "gauge",
	},
	// Arbiter cache pre-warming gauge (issue #1176). Set once at boot.
	"nexus_cache_warmed_entries": {
		help: "Number of entries loaded into the arbiter cache from historical SQLite metrics during boot-time pre-warming (issue #1176). 0 when pre-warming is disabled or no data was found.",
		typ:  "gauge",
	},
	// Per-route latency percentile gauges (issue #774). Computed from a
	// sliding window ring buffer per route (local/frontier/fusion).
	// Values are in seconds (ms → s conversion at render time).
	"nexus_upstream_request_latency_p50_seconds": {
		help: "p50 request latency in seconds per route (local/frontier/fusion), from sliding window ring buffer (issue #774).",
		typ:  "gauge",
	},
	"nexus_upstream_request_latency_p95_seconds": {
		help: "p95 request latency in seconds per route (local/frontier/fusion), from sliding window ring buffer (issue #774).",
		typ:  "gauge",
	},
	"nexus_upstream_request_latency_p99_seconds": {
		help: "p99 request latency in seconds per route (local/frontier/fusion), from sliding window ring buffer (issue #774).",
		typ:  "gauge",
	},
	// Auth limiter gauges (issue #744). Track brute-force protection state.
	"nexus_auth_limiter_tracked_ips": {
		help: "Current number of IPs being tracked by the auth brute-force limiter (issue #744).",
		typ:  "gauge",
	},
	"nexus_auth_limiter_blocked_ips": {
		help: "Current number of IPs blocked by the auth brute-force limiter (issue #744).",
		typ:  "gauge",
	},
	// Auth limiter reaper evictions counter (issue #839).
	"nexus_auth_limiter_reaper_evictions_total": {
		help: "Total idle IPs evicted from the auth limiter's failures map by the reaper (issue #839).",
		typ:  "counter",
	},
	// Auth limiter blocked counter (issue #831/#937).
	"nexus_auth_limiter_blocked_total": {
		help: "Total IPs blocked by the auth brute-force limiter (burst threshold crossed), by reason (issue #831/#937).",
		typ:  "counter",
	},
	// Confidence store error counter (issue #927).
	"nexus_confidence_errors_total": {
		help: "Total LocalConfidence errors in the planner where the SQLite confidence store returned an error (DB locked, query failed, etc.).",
		typ:  "counter",
	},
	// Frontier provider health counters (issue #1158).
	"nexus_frontier_probe_total": {
		help: "Total frontier provider health probes by provider and result (success/failure) (issue #1158).",
		typ:  "counter",
	},
	"nexus_frontier_circuit_open_total": {
		help: "Total frontier provider circuit-open transitions (issue #1158).",
		typ:  "counter",
	},
	// SLO error budget remaining (issue #1239). One gauge per SLO,
	// labelled by slo name. Values in [0, 1]: 1 = full budget, 0 = exhausted.
	"nexus_slo_error_budget_remaining": {
		help: "Remaining error budget fraction (0..1) for the named SLO (issue #1239). 1 = full budget, 0 = exhausted. Label slo is one of: availability, local_latency_p99, ttft_p95.",
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
	// Issue #1312: TOON compression counters by array type.
	writeCounterLabeled(w, "nexus_toon_compression_fenced_total",
		"Total fenced ```json [...] ``` blocks compressed (issue #1312).",
		"direction", []labelSample{
			{value: "compress", n: c.toonCompressionFencedTotal["compress"].Load()},
		})
	writeCounterLabeled(w, "nexus_toon_compression_unfenced_total",
		"Total bare/embedded [...] arrays compressed when NEXUS_TOON_UNFENCED=true (issue #1312).",
		"direction", []labelSample{
			{value: "compress", n: c.toonCompressionUnfencedTotal["compress"].Load()},
		})
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
	//nolint:errcheck // ResponseWriter error cannot be handled after headers committed.
	fmt.Fprintf(w, "nexus_estimated_cost_usd_total %s\n", formatFloat(c.EstimatedCostUSD()))

	// --- Middleware instrumentation (issue #70) --------------------------

	// Auth counters are emitted with two label dimensions: outcome
	// (accepted / rejected_invalid / rejected_missing) and client_ip.
	// The fourth outcome "exempt" is intentionally omitted: an exempt
	// request is not an authentication decision and would dilute the
	// per-decision counts. Adding client_ip enables operators to identify
	// which IPs are generating auth failures (issue #1061).
	// Snapshot auth maps under authMu to avoid racing with IncAuth*
	// writers that insert new keys (issue #1239 CI fix).
	authAcc, authRejInv, authRejMiss := c.AuthCountersSnapshot()
	// Collect all unique client IPs across all three outcome maps.
	authIPs := make(map[string]struct{})
	for ip := range authAcc {
		authIPs[ip] = struct{}{}
	}
	for ip := range authRejInv {
		authIPs[ip] = struct{}{}
	}
	for ip := range authRejMiss {
		authIPs[ip] = struct{}{}
	}
	// Build sorted slice for deterministic output.
	authIPSlice := make([]string, 0, len(authIPs))
	for ip := range authIPs {
		authIPSlice = append(authIPSlice, ip)
	}
	sort.Strings(authIPSlice)
	authSamples := make([]labelSample2, 0, len(authIPs)*3)
	for _, ip := range authIPSlice {
		if v, ok := authAcc[ip]; ok {
			authSamples = append(authSamples, labelSample2{value1: "accepted", value2: ip, n: v.Load()})
		}
		if v, ok := authRejInv[ip]; ok {
			authSamples = append(authSamples, labelSample2{value1: "rejected_invalid", value2: ip, n: v.Load()})
		}
		if v, ok := authRejMiss[ip]; ok {
			authSamples = append(authSamples, labelSample2{value1: "rejected_missing", value2: ip, n: v.Load()})
		}
	}
	writeCounterLabeled2(w, "nexus_auth_requests_total",
		"Authentication decisions by outcome and client IP (issue #70/#1061).",
		"outcome", "client_ip", authSamples)

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
	//nolint:errcheck // cannot check error after headers committed
	fmt.Fprintf(w, "nexus_budget_recorded_usd_total %s\n", formatFloat(c.BudgetRecordedUSD()))

	writeCounter(w, "nexus_budget_exceeded_total",
		"Number of frontier requests rejected by the daily budget gate.",
		c.budgetExceededTotal.Load())

	// Metrics/Judge SQLite batch transaction counter (issue #1234).
	// Counts the number of batch transactions committed by the metrics
	// and judge store drain goroutines. Each increment represents one
	// BEGIN...INSERT...COMMIT cycle.
	writeCounter(w, "nexus_metrics_batch_total",
		"Number of SQLite batch transactions committed by the metrics and judge stores (issue #1234).",
		c.metricsBatchTotal.Load())

	// TLS handshake counters. Optional: only non-zero when the operator
	// configured TLS (NEXUS_TLS_CERT + NEXUS_TLS_KEY); otherwise both
	// samples stay at 0.
	writeCounterLabeled(w, "nexus_tls_connections_total",
		"TLS handshake outcomes (issue #70; optional, only non-zero with NEXUS_TLS_CERT).",
		"outcome", []labelSample{
			{value: "accepted", n: c.tlsConnectionsAccepted.Load()},
			{value: "rejected", n: c.tlsConnectionsRejected.Load()},
		})

	// Panel panic counter (issue #309). Tracks recovered panics in
	// panel goroutines so operators can observe how often upstream JSON
	// shape causes a goroutine to die.
	writeCounter(w, "nexus_panel_panics_total",
		"Total recovered panics in panel goroutines (issue #309).", upstream.PanelPanicsTotal())

	// Fusion client abort counter (issue #1046). Tracks client disconnects
	// during fusion speculative streaming and arbiter synthesis streaming.
	writeCounter(w, "nexus_fusion_client_abort_total",
		"Total client aborts during fusion speculative streaming and arbiter synthesis streaming (issue #1046).", upstream.FusionClientAbortTotal())

	// Fusion similarity mode gauge (issue #1244). Exposes the current
	// similarity mode and the count of invocations per algorithm.
	writeCounter(w, "nexus_fusion_jaccard_similarity_total",
		"Total number of fusion agreement checks using Jaccard similarity (issue #1244).", upstream.JaccardSimilarityTotal())
	writeCounter(w, "nexus_fusion_semantic_similarity_total",
		"Total number of fusion agreement checks using semantic (cosine) similarity (issue #1244).", upstream.SemanticSimilarityTotal())

	// Coalesce counters (issue #1155). Hits are requests deduplicated via
	// singleflight or served from the TTL cache; misses are requests that
	// actually executed the upstream call.
	writeCounter(w, "nexus_coalesce_hits_total",
		"Total coalesced requests served from cache or singleflight dedup (issue #1155).", upstream.CoalesceHitsTotal())
	writeCounter(w, "nexus_coalesce_misses_total",
		"Total coalesce misses that executed the upstream call (issue #1155).", upstream.CoalesceMissesTotal())

	// Auth gauge: cumulative accepted authentications. The metric name
	// carries "_clients" per the issue spec; semantically this is a
	// monotonic counter that operators usually want charted as a
	// monotonically-rising line (Prometheus treats it as gauge so a
	// rate() function gives authentications-per-second).
	writeMeta(w, "nexus_auth_authenticated_clients",
		"Cumulative accepted authentications (issue #70).", "gauge")
	//nolint:errcheck // cannot check error after headers committed
	fmt.Fprintf(w, "nexus_auth_authenticated_clients %d\n", c.AuthAuthenticatedClients())

	// Embedder circuit breaker failures (issue #423).
	failures := c.EmbedderFailures()
	if len(failures) > 0 {
		samples := make([]labelSample, 0, len(failures))
		for kind, count := range failures {
			samples = append(samples, labelSample{value: kind, n: count})
		}
		writeCounterLabeled(w, "nexus_embedder_failures_total",
			"Total circuit breaker trip events for embedder kinds (issue #423).",
			"kind", samples)
	}

	// RAG circuit breaker trip/recover counters (issue #886).
	if trips := c.RAGCircuitTrips(); len(trips) > 0 {
		samples := make([]labelSample, 0, len(trips))
		for kind, count := range trips {
			samples = append(samples, labelSample{value: kind, n: count})
		}
		writeCounterLabeled(w, "nexus_rag_circuit_trip_total",
			"Total RAG embedder circuit breaker trip events (issue #886).",
			"service", samples)
	}
	if recovers := c.RAGCircuitRecovers(); len(recovers) > 0 {
		samples := make([]labelSample, 0, len(recovers))
		for kind, count := range recovers {
			samples = append(samples, labelSample{value: kind, n: count})
		}
		writeCounterLabeled(w, "nexus_rag_circuit_recover_total",
			"Total RAG embedder circuit breaker recovery events (issue #886).",
			"service", samples)
	}

	// Auth limiter reaper evictions counter (issue #839).
	writeCounter(w, "nexus_auth_limiter_reaper_evictions_total",
		"Total idle IPs evicted from the auth limiter's failures map by the reaper (issue #839).",
		c.authReaperEvictions.Load())

	// Auth limiter blocked counter (issue #831/#937).
	writeCounterLabeled(w, "nexus_auth_limiter_blocked_total",
		"Total IPs blocked by the auth brute-force limiter (burst threshold crossed), by reason (issue #831/#937).",
		"reason", []labelSample{
			{value: "missing", n: c.authBlockedTotal["missing"].Load()},
			{value: "invalid", n: c.authBlockedTotal["invalid"].Load()},
		})

	// Confidence store error counter (issue #927).
	writeCounter(w, "nexus_confidence_errors_total",
		"Total LocalConfidence errors in the planner where the SQLite confidence store returned an error (DB locked, query failed, etc.).",
		c.ConfidenceErrors())

	// RAG-vs-judge quality correlation (issue #1167). Sum and count of
	// judge scores partitioned by whether RAG context was injected.
	// Operators compute avg = sum/count per label to measure retrieval
	// effectiveness.
	writeMeta(w, "nexus_rag_judge_score_sum",
		"Cumulative judge quality score sum partitioned by RAG injection (issue #1167). Compute avg via nexus_rag_judge_score_sum / nexus_rag_judge_score_count.", "counter")
	//nolint:errcheck // ResponseWriter error cannot be handled after headers committed.
	fmt.Fprintf(w, "nexus_rag_judge_score_sum{injected=\"true\"} %s\n", formatFloat(c.RAGJudgeScoreSum(true)))
	//nolint:errcheck // ResponseWriter error cannot be handled after headers committed.
	fmt.Fprintf(w, "nexus_rag_judge_score_sum{injected=\"false\"} %s\n", formatFloat(c.RAGJudgeScoreSum(false)))
	writeCounterLabeled(w, "nexus_rag_judge_score_count",
		"Count of judge quality scores partitioned by RAG injection (issue #1167).",
		"injected", []labelSample{
			{value: "true", n: c.RAGJudgeScoreCount(true)},
			{value: "false", n: c.RAGJudgeScoreCount(false)},
		})

	// Frontier provider health probe counter (issue #1158).
	if probes := c.FrontierProbeTotals(); len(probes) > 0 {
		type pr struct {
			provider string
			result   string
			n        uint64
		}
		var samples []pr
		for key, count := range probes {
			parts := strings.SplitN(key, "|", 2)
			if len(parts) != 2 {
				continue
			}
			samples = append(samples, pr{provider: parts[0], result: parts[1], n: count})
		}
		sort.Slice(samples, func(i, j int) bool {
			if samples[i].provider != samples[j].provider {
				return samples[i].provider < samples[j].provider
			}
			return samples[i].result < samples[j].result
		})
		writeMeta(w, "nexus_frontier_probe_total",
			"Total frontier provider health probes by provider and result (success/failure) (issue #1158).", "counter")
		for _, s := range samples {
			//nolint:errcheck // cannot check error after headers committed
			fmt.Fprintf(w, "nexus_frontier_probe_total{provider=%q,result=%q} %d\n",
				s.provider, s.result, s.n)
		}
	}

	// Frontier provider circuit-open counter (issue #1158).
	if opens := c.FrontierCircuitOpenTotals(); len(opens) > 0 {
		samples := make([]labelSample, 0, len(opens))
		for provider, count := range opens {
			samples = append(samples, labelSample{value: provider, n: count})
		}
		writeCounterLabeled(w, "nexus_frontier_circuit_open_total",
			"Total frontier provider circuit-open transitions (issue #1158).",
			"provider", samples)
	}

	// --- Histograms -----------------------------------------------------

	exemplars := c.ExemplarsEnabled()
	writeHistogramLabeled(w, "nexus_request_duration_ms",
		"End-to-end request duration in milliseconds, from body read to final flush, by route.",
		"route", map[string]*Histogram{
			"local":    c.latencyLocal,
			"frontier": c.latencyFrontier,
			"fusion":   c.latencyFusion,
		}, exemplars)
	writeHistogramLabeled(w, "nexus_ttft_ms",
		"Time to first token in milliseconds (0 / unobserved for non-streaming responses), by route.",
		"route", map[string]*Histogram{
			"local":    c.ttftLocal,
			"frontier": c.ttftFrontier,
			"fusion":   c.ttftFusion,
		}, exemplars)
	// Per-stage pipeline latency histograms (issue #300).
	writeStageHistogram(w, c)

	// SLM confidence histogram (issue #425). Written only when the
	// histograms map is non-nil and contains at least one observation.
	if hists := c.SLMConfidenceHistograms(); len(hists) > 0 {
		writeSLMConfidenceHistogram(w, hists)
	}

	// RAG similarity histogram (issue #447). Written only when at
	// least one (path, outcome) histogram has observations.
	if hists := c.RAGSimilarityHistograms(); len(hists) > 0 {
		writeRAGSimilarityHistogram(w, hists)
	}

	// Rate-limit bucket utilization histogram (issue #746). Written only
	// when at least one bucket has been observed.
	if hists := c.RateLimitUtilizationHistograms(); len(hists) > 0 {
		writeRateLimitUtilizationHistogram(w, hists)
	}

	// --- Gauges (live readings from providers) --------------------------

	gauges := collectGauges(providers)

	// Group samples by metric name so HELP/TYPE is emitted once per family.
	type sampleEntry struct {
		labels map[string]string
		value  float64
	}
	family := make(map[string][]sampleEntry)
	for _, g := range gauges {
		family[g.Name] = append(family[g.Name], sampleEntry{labels: g.Labels, value: g.Value})
	}
	for _, name := range sortedKeys(family) {
		meta, ok := gaugeMeta[name]
		if !ok {
			meta = metricMeta{help: name, typ: "gauge"}
		}
		writeMeta(w, name, meta.help, meta.typ)
		for _, s := range family[name] {
			//nolint:errcheck // cannot check error after headers committed
			if len(s.labels) == 0 {
				fmt.Fprintf(w, "%s %s\n", name, formatFloat(s.value))
			} else {
				fmt.Fprintf(w, "%s%s %s\n", name, formatLabelMap(s.labels), formatFloat(s.value))
			}
		}
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
//
//nolint:errcheck
func writeMeta(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
}

// writeCounter emits a single-sample unlabelled counter family.
//
//nolint:errcheck
func writeCounter(w io.Writer, name, help string, v uint64) {
	writeMeta(w, name, help, "counter")
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// writeCounterLabeled emits a counter family with one label dimension.
// Each labelSample becomes its own sample line. The label values are
// emitted in the order given (callers pass them sorted by relevance).
//
//nolint:errcheck
func writeCounterLabeled(w io.Writer, name, help, label string, samples []labelSample) {
	writeMeta(w, name, help, "counter")
	for _, s := range samples {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, s.value, s.n)
	}
}

// labelSample2 pairs two label values with their counter reading for a
// two-dimensional labelled counter family (e.g. outcome + client_ip on
// nexus_auth_requests_total for issue #1061).
type labelSample2 struct {
	value1 string
	value2 string
	n      uint64
}

// writeCounterLabeled2 emits a counter family with two label dimensions.
// Each (label1, label2) tuple becomes its own sample line. Samples are
// emitted in sorted order by label1, then label2 for deterministic output.
//
//nolint:errcheck
func writeCounterLabeled2(w io.Writer, name, help, label1, label2 string, samples []labelSample2) {
	writeMeta(w, name, help, "counter")
	for _, s := range samples {
		fmt.Fprintf(w, "%s{%s=%q,%s=%q} %d\n", name, label1, s.value1, label2, s.value2, s.n)
	}
}

// writeHistogramLabeled emits a histogram family with one label dimension.
// Each route gets its own bucket lines, _sum, and _count.
// Routes are emitted in a fixed order (local, frontier, fusion) for
// deterministic output.
//
// When exemplars is true, non-+Inf bucket lines carry the most recent
// trace exemplar (issue #1171): `... %d # {trace_id="...",span_id="..."} %s`.
// +Inf, _sum, and _count lines never carry exemplars.
//
//nolint:errcheck
func writeHistogramLabeled(w io.Writer, name, help, label string, histograms map[string]*Histogram, exemplars bool) {
	writeMeta(w, name, help, "histogram")
	// Fixed route order for deterministic output.
	for _, route := range []string{"local", "frontier", "fusion"} {
		h, ok := histograms[route]
		if !ok || h == nil {
			continue
		}
		if exemplars {
			cum, upperBounds, sum, count, exs := h.SnapshotWithExemplars()
			for i, ub := range upperBounds {
				writeBucketLineWithExemplar(w, fmt.Sprintf("%s_bucket{%s=%q,le=%q}", name, label, route, formatFloat(ub)), cum[i], exs[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, route, "+Inf", cum[len(upperBounds)])
			fmt.Fprintf(w, "%s_sum{%s=%q} %s\n", name, label, route, formatFloat(sum))
			fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, label, route, count)
		} else {
			cum, upperBounds, sum, count := h.Snapshot()
			for i, ub := range upperBounds {
				fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, route, formatFloat(ub), cum[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, route, "+Inf", cum[len(upperBounds)])
			fmt.Fprintf(w, "%s_sum{%s=%q} %s\n", name, label, route, formatFloat(sum))
			fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, label, route, count)
		}
	}
}

// writeBucketLineWithExemplar emits one non-+Inf histogram bucket line
// with an optional exemplar suffix (issue #1171). When ex.TraceID is
// non-empty, the line carries `# {trace_id="...",span_id="..."} <value>`;
// otherwise it is a plain bucket line (byte-compatible with pre-exemplar
// output when no exemplar was stored for this bucket).
//
//nolint:errcheck
func writeBucketLineWithExemplar(w io.Writer, prefix string, count uint64, ex Exemplar) {
	if ex.TraceID != "" {
		fmt.Fprintf(w, "%s %d # {trace_id=%q,span_id=%q} %s\n", prefix, count, ex.TraceID, ex.SpanID, formatFloat(ex.Value))
	} else {
		fmt.Fprintf(w, "%s %d\n", prefix, count)
	}
}

// writeHistogram emits a histogram family: one bucket line per finite
// upper bound plus the +Inf bucket, then _sum and _count.
// Kept for backward compatibility with tests and single-route use cases.
//
//nolint:errcheck
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

// writeStageHistogram emits the nexus_pipeline_stage_latency_ms histogram
// family with a "stage" label (issue #300). The five stages are emitted
// in a fixed order for deterministic output.
//
//nolint:errcheck
func writeStageHistogram(w io.Writer, c *Collector) {
	exemplars := c.ExemplarsEnabled()
	stages := []struct {
		name string
		h    *Histogram
	}{
		{"rag", c.stageRAG},
		{"prompt_eng", c.stagePromptEng},
		{"toon", c.stageTOON},
		{"slm", c.stageSLM},
		{"upstream", c.stageUpstream},
	}
	writeMeta(w, "nexus_pipeline_stage_latency_ms",
		"Pipeline stage latency in milliseconds, by stage (issue #300).",
		"histogram")
	for _, s := range stages {
		if s.h == nil {
			continue
		}
		if exemplars {
			cum, upperBounds, sum, count, exs := s.h.SnapshotWithExemplars()
			for i, ub := range upperBounds {
				writeBucketLineWithExemplar(w,
					fmt.Sprintf("nexus_pipeline_stage_latency_ms_bucket{stage=%q,le=%q}", s.name, formatFloat(ub)),
					cum[i], exs[i])
			}
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_bucket{stage=%q,le=%q} %d\n",
				s.name, "+Inf", cum[len(upperBounds)])
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_sum{stage=%q} %s\n",
				s.name, formatFloat(sum))
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_count{stage=%q} %d\n",
				s.name, count)
		} else {
			cum, upperBounds, sum, count := s.h.Snapshot()
			for i, ub := range upperBounds {
				fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_bucket{stage=%q,le=%q} %d\n",
					s.name, formatFloat(ub), cum[i])
			}
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_bucket{stage=%q,le=%q} %d\n",
				s.name, "+Inf", cum[len(upperBounds)])
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_sum{stage=%q} %s\n",
				s.name, formatFloat(sum))
			fmt.Fprintf(w, "nexus_pipeline_stage_latency_ms_count{stage=%q} %d\n",
				s.name, count)
		}
	}
}

// writeSLMConfidenceHistogram emits the nexus_slm_confidence_histogram
// histogram family labelled by task_category (issue #425). Categories
// are emitted in sorted order for deterministic output.
func writeSLMConfidenceHistogram(w io.Writer, histograms map[string]*Histogram) {
	writeMeta(w, "nexus_slm_confidence_histogram",
		"SLM routing confidence score distribution by task category (issue #425).",
		"histogram")
	// Sort categories for deterministic output.
	categories := sortedKeys(histograms)
	for _, cat := range categories {
		h := histograms[cat]
		if h == nil {
			continue
		}
		cum, upperBounds, sum, count := h.Snapshot()
		// Skip completely empty histograms.
		if count == 0 {
			continue
		}
		for i, ub := range upperBounds {
			fmt.Fprintf(w, "nexus_slm_confidence_histogram_bucket{task_category=%q,le=%q} %d\n",
				cat, formatFloat(ub), cum[i])
		}
		fmt.Fprintf(w, "nexus_slm_confidence_histogram_bucket{task_category=%q,le=%q} %d\n",
			cat, "+Inf", cum[len(upperBounds)])
		fmt.Fprintf(w, "nexus_slm_confidence_histogram_sum{task_category=%q} %s\n",
			cat, formatFloat(sum))
		fmt.Fprintf(w, "nexus_slm_confidence_histogram_count{task_category=%q} %d\n",
			cat, count)
	}
}

// writeRAGSimilarityHistogram emits the nexus_rag_similarity_histogram
// histogram family labelled by path, outcome, and threshold (issue #447, #671).
//
// Labels:
//   - path       ∈ {"hnsw", "brute_force"} — the retrieval algorithm
//     Retrieve actually used; see rag.IndexPath.
//   - outcome    ∈ {"hit", "miss"}         — "hit" when a snippet cleared
//     the configured threshold, "miss" when it did not.
//   - threshold  ∈ (0.0, 1.0]              — the effective similarity floor
//     applied for this retrieval (global or per-directory override).
//
// Cardinality: dynamic — one series per (path × outcome × threshold) tuple.
// Each series has RAGSimilarityBuckets (10) + +Inf bucket lines, plus _sum
// and _count. The map key is "path|outcome|threshold"; we split it back
// into three labels at render time so the Prometheus exposition matches
// the documented label schema.
//
// Series are emitted in sorted key order so scrape-to-scrape diffs are
// stable and friendly to human inspection. Empty histograms (count == 0)
// are skipped so the scrape output stays clean until the first observation
// lands — and when ALL histograms are empty the HELP/TYPE header is omitted
// too, so a freshly-booted scraper never sees a misleading zero-count
// family.
func writeRAGSimilarityHistogram(w io.Writer, histograms map[string]*Histogram) {
	type snapshot struct {
		path, outcome string
		threshold     float64
		cum           []uint64
		upperBounds   []float64
		sum           float64
		count         uint64
	}
	var snaps []snapshot
	for key, h := range histograms {
		if h == nil {
			continue
		}
		cum, upperBounds, sum, count := h.Snapshot()
		if count == 0 {
			continue
		}
		// Parse key: "path|outcome|threshold"
		parts := strings.Split(key, "|")
		if len(parts) != 3 {
			continue
		}
		path := parts[0]
		outcome := parts[1]
		var threshold float64
		if _, err := fmt.Sscanf(parts[2], "%f", &threshold); err != nil {
			continue
		}
		snaps = append(snaps, snapshot{
			path:        path,
			outcome:     outcome,
			threshold:   threshold,
			cum:         cum,
			upperBounds: upperBounds,
			sum:         sum,
			count:       count,
		})
	}
	if len(snaps) == 0 {
		return
	}
	// Sort for stable output order
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].path != snaps[j].path {
			return snaps[i].path < snaps[j].path
		}
		if snaps[i].outcome != snaps[j].outcome {
			return snaps[i].outcome < snaps[j].outcome
		}
		return snaps[i].threshold < snaps[j].threshold
	})
	writeMeta(w, "nexus_rag_similarity_histogram",
		"RAG retrieval cosine-similarity score distribution, labelled by index path, outcome, and effective threshold (issue #447, #671).",
		"histogram")
	for _, s := range snaps {
		thr := fmt.Sprintf("%.2f", s.threshold)
		for i, ub := range s.upperBounds {
			fmt.Fprintf(w, "nexus_rag_similarity_histogram_bucket{path=%q,outcome=%q,threshold=%q,le=%q} %d\n",
				s.path, s.outcome, thr, formatFloat(ub), s.cum[i])
		}
		fmt.Fprintf(w, "nexus_rag_similarity_histogram_bucket{path=%q,outcome=%q,threshold=%q,le=%q} %d\n",
			s.path, s.outcome, thr, "+Inf", s.cum[len(s.upperBounds)])
		fmt.Fprintf(w, "nexus_rag_similarity_histogram_sum{path=%q,outcome=%q,threshold=%q} %s\n",
			s.path, s.outcome, thr, formatFloat(s.sum))
		fmt.Fprintf(w, "nexus_rag_similarity_histogram_count{path=%q,outcome=%q,threshold=%q} %d\n",
			s.path, s.outcome, thr, s.count)
	}
}

// writeRateLimitUtilizationHistogram emits the
// nexus_rate_limit_bucket_utilization histogram family labelled by
// bucket_id (hashed IP) and utilization quartile (issue #746).
// Series are emitted in sorted bucket-ID order for deterministic output.
// Empty histograms (count == 0) are skipped so a freshly-booted scraper
// never sees a misleading zero-count family.
func writeRateLimitUtilizationHistogram(w io.Writer, histograms map[string]*Histogram) {
	type snapshot struct {
		bucketID    string
		cum         []uint64
		upperBounds []float64
		sum         float64
		count       uint64
	}
	var snaps []snapshot
	for bucketID, h := range histograms {
		if h == nil {
			continue
		}
		cum, upperBounds, sum, count := h.Snapshot()
		if count == 0 {
			continue
		}
		snaps = append(snaps, snapshot{
			bucketID:    bucketID,
			cum:         cum,
			upperBounds: upperBounds,
			sum:         sum,
			count:       count,
		})
	}
	if len(snaps) == 0 {
		return
	}
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].bucketID < snaps[j].bucketID
	})
	writeMeta(w, "nexus_rate_limit_bucket_utilization",
		"Fractional token utilization (tokens/burst) at moment of acquisition per client bucket, bucketed by quartile (issue #746).",
		"histogram")
	for _, s := range snaps {
		for i, ub := range s.upperBounds {
			fmt.Fprintf(w, "nexus_rate_limit_bucket_utilization_bucket{bucket_id=%q,le=%q} %d\n",
				s.bucketID, formatFloat(ub), s.cum[i])
		}
		fmt.Fprintf(w, "nexus_rate_limit_bucket_utilization_bucket{bucket_id=%q,le=%q} %d\n",
			s.bucketID, "+Inf", s.cum[len(s.upperBounds)])
		fmt.Fprintf(w, "nexus_rate_limit_bucket_utilization_sum{bucket_id=%q} %s\n",
			s.bucketID, formatFloat(s.sum))
		fmt.Fprintf(w, "nexus_rate_limit_bucket_utilization_count{bucket_id=%q} %d\n",
			s.bucketID, s.count)
	}
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

// sortedKeys returns the sorted keys of a map[string]T, for deterministic output.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// formatLabelMap renders a map as a Prometheus label set string,
// e.g. {circuit="ollama",state="open"}. Keys are sorted for
// deterministic output.
func formatLabelMap(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
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
