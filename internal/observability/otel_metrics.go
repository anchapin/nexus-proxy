// Package observability implements an in-process metrics collector and
// a Prometheus text-exposition renderer. This file adds OTLP/JSON metrics
// export (issue #1238) so operators using Grafana Cloud, Honeycomb, or
// New Relic can ingest nexus_* metrics without a Prometheus + OTel Collector
// sidecar.
//
// Stdlib-only by design: sync/atomic for the hot path, encoding/json for
// OTLP/JSON serialization. No external OTel SDK dependency.
package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// OtelMetricsServiceName is stamped as service.name on every metric
// resource so operators can query "show me everything from nexus-proxy".
const OtelMetricsServiceName = "nexus-proxy"

// OtelMetricsScopeName is the instrumentation library name for metrics.
const OtelMetricsScopeName = "github.com/anchapin/nexus-proxy/internal/observability"

// OtelMetricsExporter buffers metric snapshots and POSTs them as OTLP/JSON
// batches to the configured collector endpoint on a fixed interval.
// A nil *OtelMetricsExporter is a no-op for every method.
//
// The exporter reuses the same retry/back-off pattern as the tracing
// exporter (internal/tracing/exporter.go) so operators manage both with
// the same NEXUS_TRACING_MAX_RETRIES / _RETRY_BASE_DELAY / _RETRY_MAX_DELAY
// tunables.
type OtelMetricsExporter struct {
	endpoint string
	client   *http.Client
	interval time.Duration
	timeout  time.Duration

	batchCap int

	// Shared retry parameters from the tracing package.
	maxRetries     int
	retryBaseDelay time.Duration
	maxRetryDelay  time.Duration

	queue chan struct{} // one struct{} per tick to signal flush

	wg             sync.WaitGroup
	closed         atomic.Bool
	exportFailures atomic.Uint64 // nexus_otel_metrics_export_failures_total
}

// OtelMetricsConfig is the input to NewOtelMetricsExporter.
type OtelMetricsConfig struct {
	// Endpoint is the full OTLP/JSON HTTP URL, including the
	// `/v1/metrics` path. Empty disables metrics export entirely
	// (NewOtelMetricsExporter returns nil).
	Endpoint string

	// Interval is the cadence at which metric snapshots are collected
	// and POSTed. Must be > 0; default 60s.
	Interval time.Duration

	// Timeout bounds each POST. <=0 falls back to 10s.
	Timeout time.Duration

	// Client is the *http.Client used for POSTs. Nil falls back to a
	// client with the configured Timeout.
	Client *http.Client

	// BatchSize bounds the number of metric snapshots buffered between
	// POST cycles. Ignored in this implementation since we POST on a
	// fixed tick rather than batch-by-count.
	BatchSize int

	// MaxRetries is the maximum retry attempts after initial POST fails.
	// <=0 falls back to defaultOtelMaxRetries (3).
	MaxRetries int

	// RetryBaseDelay is the initial back-off delay. <=0 falls back to
	// defaultOtelRetryBaseDelay (100ms).
	RetryBaseDelay time.Duration

	// MaxRetryDelay caps the back-off ceiling. <=0 falls back to
	// defaultOtelRetryMaxDelay (2s).
	MaxRetryDelay time.Duration
}

const defaultOtelMetricsInterval = 60 * time.Second
const defaultOtelMetricsTimeout = 10 * time.Second

// Default retry constants for OTLP metrics POST retries.
// Mirrors the tracing exporter defaults so operators manage both with
// the same NEXUS_TRACING_MAX_RETRIES / _RETRY_BASE_DELAY / _RETRY_MAX_DELAY tunables.
const (
	defaultOtelMaxRetries     = 3
	defaultOtelRetryBaseDelay = 100 * time.Millisecond
	defaultOtelRetryMaxDelay  = 2 * time.Second
)

// NewOtelMetricsExporter starts the background export loop and returns
// a ready exporter. Returns nil when endpoint is empty (zero overhead
// when metrics export is not configured).
func NewOtelMetricsExporter(cfg OtelMetricsConfig) *OtelMetricsExporter {
	if cfg.Endpoint == "" {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultOtelMetricsInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultOtelMetricsTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	} else {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultOtelMaxRetries
	}
	retryBaseDelay := cfg.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = defaultOtelRetryBaseDelay
	}
	maxRetryDelay := cfg.MaxRetryDelay
	if maxRetryDelay <= 0 {
		maxRetryDelay = defaultOtelRetryMaxDelay
	}
	e := &OtelMetricsExporter{
		endpoint:       cfg.Endpoint,
		client:         client,
		interval:       cfg.Interval,
		timeout:        cfg.Timeout,
		batchCap:       cfg.BatchSize,
		maxRetries:     maxRetries,
		retryBaseDelay: retryBaseDelay,
		maxRetryDelay:  maxRetryDelay,
		queue:          make(chan struct{}, 1),
	}
	e.wg.Add(1)
	go e.run()
	return e
}

// Endpoint returns the OTLP/JSON URL the exporter POSTs to.
func (e *OtelMetricsExporter) Endpoint() string {
	if e == nil {
		return ""
	}
	return e.endpoint
}

// ExportFailures returns the cumulative count of export batches that
// failed to POST (HTTP 4xx/5xx, timeout, or transport error).
func (e *OtelMetricsExporter) ExportFailures() uint64 {
	if e == nil {
		return 0
	}
	return e.exportFailures.Load()
}

// Close signals the background loop to drain and exit. Safe to call
// once; subsequent calls are no-ops. Blocks until the loop returns.
func (e *OtelMetricsExporter) Close() error {
	if e == nil || e.endpoint == "" {
		return nil
	}
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(e.queue)
	e.wg.Wait()
	return nil
}

// run drives the periodic export loop. On each tick it collects a
// snapshot from the Collector and flushes it.
func (e *OtelMetricsExporter) run() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.export()
		case <-e.queue:
			e.export()
			return
		}
	}
}

// export collects a metric snapshot and POSTs it as OTLP/JSON.
func (e *OtelMetricsExporter) export() {
	snapshot := CollectMetricSnapshot()
	if err := e.flush(snapshot); err != nil {
		e.exportFailures.Add(1)
		slog.Warn("otel metrics export failed",
			slog.String("endpoint", e.endpoint),
			slog.Any("err", err),
		)
	}
}

// flush POSTs the metric payload with retry. On error the batch is
// dropped — the spec mandates non-blocking semantics on the export path.
func (e *OtelMetricsExporter) flush(snapshot []MetricSnapshot) error {
	if len(snapshot) == 0 {
		return nil
	}
	body, err := buildOTLPMetricsJSON(snapshot)
	if err != nil {
		return fmt.Errorf("otel_metrics: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt <= e.maxRetries; attempt++ {
		if attempt > 0 {
			delay := e.retryBaseDelay * time.Duration(1<<(attempt-1))
			if delay > e.maxRetryDelay {
				delay = e.maxRetryDelay
			}
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("otel_metrics: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("otel_metrics: do: %w", err)
			break
		}
		if resp != nil {
			defer resp.Body.Close()
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("otel_metrics: collector status %d", resp.StatusCode)
			if resp.StatusCode < 500 {
				return lastErr
			}
			if attempt < e.maxRetries {
				continue
			}
			break
		}
		return nil
	}
	return lastErr
}

// MetricSnapshot is one metric data point captured at export time.
type MetricSnapshot struct {
	Name   string
	Type   MetricType // "counter", "gauge", "histogram"
	Labels map[string]string

	// For counters (Sum with monotonic=true)
	Count uint64
	Sum   float64 // cumulative total for counters

	// For gauges
	Value float64

	// For histograms
	HistogramBuckets []HistogramBucket
	HistogramSum     float64
	HistogramCount   uint64
}

// HistogramBucket is one histogram bucket.
type HistogramBucket struct {
	UpperBound float64
	Count      uint64
}

// MetricType distinguishes counter/gauge/histogram.
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
)

// CollectMetricSnapshot returns a flat snapshot of all current metric
// values from the Collector for OTLP export. The collector's own state
// is read atomically so this is safe to call from the export goroutine.
func CollectMetricSnapshot() []MetricSnapshot {
	var out []MetricSnapshot

	// We can't call RenderPrometheus here (it writes to an io.Writer).
	// Instead we replicate the snapshot logic directly using the Collector's
	// public readout methods. The set of metrics surfaced here must stay in
	// sync with the Prometheus renderer — see prometheus.go RenderPrometheus.

	// Route request counters
	out = append(out, MetricSnapshot{
		Name:   "nexus_requests_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "local"},
		Sum:    float64(collectorSlow.RequestsLocal()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_requests_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "frontier"},
		Sum:    float64(collectorSlow.RequestsFrontier()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_requests_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "fusion"},
		Sum:    float64(collectorSlow.RequestsFusion()),
	})

	// Route error counters
	out = append(out, MetricSnapshot{
		Name:   "nexus_errors_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "local"},
		Sum:    float64(collectorSlow.ErrorsLocal()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_errors_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "frontier"},
		Sum:    float64(collectorSlow.ErrorsFrontier()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_errors_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"route": "fusion"},
		Sum:    float64(collectorSlow.ErrorsFusion()),
	})

	// Token counters
	out = append(out, MetricSnapshot{
		Name: "nexus_input_tokens_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.InputTokensTotal()),
	})
	out = append(out, MetricSnapshot{
		Name: "nexus_output_tokens_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.OutputTokensTotal()),
	})
	out = append(out, MetricSnapshot{
		Name: "nexus_toon_savings_tokens_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.TOONSavingsTokensTotal()),
	})

	// Cost
	out = append(out, MetricSnapshot{
		Name: "nexus_estimated_cost_usd_total",
		Type: MetricTypeCounter,
		Sum:  collectorSlow.EstimatedCostUSD(),
	})

	// RAG
	out = append(out, MetricSnapshot{
		Name:   "nexus_rag_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"outcome": "hit"},
		Sum:    float64(collectorSlow.RAGHits()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_rag_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"outcome": "miss"},
		Sum:    float64(collectorSlow.RAGMisses()),
	})

	// TOON
	out = append(out, MetricSnapshot{
		Name: "nexus_toon_compressed_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.TOONCompressed()),
	})

	// Degraded
	out = append(out, MetricSnapshot{
		Name: "nexus_degraded_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.Degraded()),
	})

	// Auth counters
	authAccepted, authRejectedInvalid, authRejectedMissing := collectorSlow.AuthCountersSnapshot()
	for ip, cnt := range authAccepted {
		out = append(out, MetricSnapshot{
			Name:   "nexus_auth_requests_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"client_ip": ip, "outcome": "accepted"},
			Sum:    float64(cnt.Load()),
		})
	}
	for ip, cnt := range authRejectedInvalid {
		out = append(out, MetricSnapshot{
			Name:   "nexus_auth_requests_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"client_ip": ip, "outcome": "rejected_invalid"},
			Sum:    float64(cnt.Load()),
		})
	}
	for ip, cnt := range authRejectedMissing {
		out = append(out, MetricSnapshot{
			Name:   "nexus_auth_requests_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"client_ip": ip, "outcome": "rejected_missing"},
			Sum:    float64(cnt.Load()),
		})
	}

	// Rate limit counters
	out = append(out, MetricSnapshot{
		Name:   "nexus_rate_limit_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"scope": "global", "outcome": "allowed"},
		Sum:    float64(collectorSlow.RateLimitAllowedGlobal()),
	})
	out = append(out, MetricSnapshot{
		Name:   "nexus_rate_limit_total",
		Type:   MetricTypeCounter,
		Labels: map[string]string{"scope": "global", "outcome": "rejected"},
		Sum:    float64(collectorSlow.RateLimitRejectedGlobal()),
	})

	// Budget counters
	out = append(out, MetricSnapshot{
		Name: "nexus_budget_exceeded_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.BudgetExceeded()),
	})
	out = append(out, MetricSnapshot{
		Name: "nexus_budget_recorded_usd_total",
		Type: MetricTypeCounter,
		Sum:  collectorSlow.BudgetRecordedUSD(),
	})

	// Latency histograms — emit as OTLP histograms
	for _, route := range []string{"local", "frontier", "fusion"} {
		var h *Histogram
		switch route {
		case "local":
			h = collectorSlow.LatencyLocal()
		case "frontier":
			h = collectorSlow.LatencyFrontier()
		case "fusion":
			h = collectorSlow.LatencyFusion()
		}
		if h == nil {
			continue
		}
		cumulative, upperBounds, sum, count := h.Snapshot()
		var buckets []HistogramBucket
		for i, ub := range upperBounds {
			buckets = append(buckets, HistogramBucket{
				UpperBound: ub,
				Count:      cumulative[i],
			})
		}
		// Add +Inf bucket
		buckets = append(buckets, HistogramBucket{
			UpperBound: math.Inf(1),
			Count:      cumulative[len(upperBounds)],
		})
		out = append(out, MetricSnapshot{
			Name:             "nexus_request_latency_ms",
			Type:             MetricTypeHistogram,
			Labels:           map[string]string{"route": route},
			HistogramBuckets: buckets,
			HistogramSum:     sum,
			HistogramCount:   count,
		})
	}

	// TTFT histograms
	for _, route := range []string{"local", "frontier", "fusion"} {
		var h *Histogram
		switch route {
		case "local":
			h = collectorSlow.TTFTLocal()
		case "frontier":
			h = collectorSlow.TTFTFrontier()
		case "fusion":
			h = collectorSlow.TTFTFusion()
		}
		if h == nil {
			continue
		}
		cumulative, upperBounds, sum, count := h.Snapshot()
		var buckets []HistogramBucket
		for i, ub := range upperBounds {
			buckets = append(buckets, HistogramBucket{
				UpperBound: ub,
				Count:      cumulative[i],
			})
		}
		buckets = append(buckets, HistogramBucket{
			UpperBound: math.Inf(1),
			Count:      cumulative[len(upperBounds)],
		})
		out = append(out, MetricSnapshot{
			Name:             "nexus_ttft_ms",
			Type:             MetricTypeHistogram,
			Labels:           map[string]string{"route": route},
			HistogramBuckets: buckets,
			HistogramSum:     sum,
			HistogramCount:   count,
		})
	}

	// Stage pipeline histograms
	for _, stage := range []struct{ name, label string }{
		{"rag", "rag"},
		{"prompt_eng", "prompt_eng"},
		{"toon", "toon"},
		{"slm", "slm"},
		{"upstream", "upstream"},
	} {
		var h *Histogram
		switch stage.name {
		case "rag":
			h = collectorSlow.StageRAG()
		case "prompt_eng":
			h = collectorSlow.StagePromptEng()
		case "toon":
			h = collectorSlow.StageTOON()
		case "slm":
			h = collectorSlow.StageSLM()
		case "upstream":
			h = collectorSlow.StageUpstream()
		}
		if h == nil {
			continue
		}
		cumulative, upperBounds, sum, count := h.Snapshot()
		var buckets []HistogramBucket
		for i, ub := range upperBounds {
			buckets = append(buckets, HistogramBucket{
				UpperBound: ub,
				Count:      cumulative[i],
			})
		}
		buckets = append(buckets, HistogramBucket{
			UpperBound: math.Inf(1),
			Count:      cumulative[len(upperBounds)],
		})
		out = append(out, MetricSnapshot{
			Name:             "nexus_pipeline_stage_latency_ms",
			Type:             MetricTypeHistogram,
			Labels:           map[string]string{"stage": stage.label},
			HistogramBuckets: buckets,
			HistogramSum:     sum,
			HistogramCount:   count,
		})
	}

	// SLM confidence histograms
	slmHists := collectorSlow.SLMConfidenceHistograms()
	for cat, h := range slmHists {
		if h == nil {
			continue
		}
		cumulative, upperBounds, sum, count := h.Snapshot()
		var buckets []HistogramBucket
		for i, ub := range upperBounds {
			buckets = append(buckets, HistogramBucket{
				UpperBound: ub,
				Count:      cumulative[i],
			})
		}
		buckets = append(buckets, HistogramBucket{
			UpperBound: math.Inf(1),
			Count:      cumulative[len(upperBounds)],
		})
		out = append(out, MetricSnapshot{
			Name:             "nexus_slm_confidence",
			Type:             MetricTypeHistogram,
			Labels:           map[string]string{"task_type": cat},
			HistogramBuckets: buckets,
			HistogramSum:     sum,
			HistogramCount:   count,
		})
	}

	// RAG similarity histograms
	ragHists := collectorSlow.RAGSimilarityHistograms()
	for key, h := range ragHists {
		if h == nil {
			continue
		}
		// key format: "path|outcome|threshold"
		var path, outcome string
		fmt.Sscanf(key, "%[^|]|%[^|]|", &path, &outcome)
		cumulative, upperBounds, sum, count := h.Snapshot()
		var buckets []HistogramBucket
		for i, ub := range upperBounds {
			buckets = append(buckets, HistogramBucket{
				UpperBound: ub,
				Count:      cumulative[i],
			})
		}
		buckets = append(buckets, HistogramBucket{
			UpperBound: math.Inf(1),
			Count:      cumulative[len(upperBounds)],
		})
		out = append(out, MetricSnapshot{
			Name:             "nexus_rag_similarity",
			Type:             MetricTypeHistogram,
			Labels:           map[string]string{"path": path, "outcome": outcome},
			HistogramBuckets: buckets,
			HistogramSum:     sum,
			HistogramCount:   count,
		})
	}

	// SLO error budget gauges
	sloGauges := collectorSlow.SLOErrorBudgetGauges()
	for _, g := range sloGauges {
		out = append(out, MetricSnapshot{
			Name:   g.Name,
			Type:   MetricTypeGauge,
			Labels: g.Labels,
			Value:  g.Value,
		})
	}

	// Latency percentile gauges
	pctGauges := collectorSlow.LatencyPercentileGauges()
	for _, g := range pctGauges {
		out = append(out, MetricSnapshot{
			Name:   g.Name,
			Type:   MetricTypeGauge,
			Labels: g.Labels,
			Value:  g.Value,
		})
	}

	// Circuit breaker gauges
	cbGauges := collectorSlow.CircuitBreakerGauges()
	for _, g := range cbGauges {
		out = append(out, MetricSnapshot{
			Name:   g.Name,
			Type:   MetricTypeGauge,
			Labels: g.Labels,
			Value:  g.Value,
		})
	}

	// RAG circuit gauges
	ragCbGauges := collectorSlow.RAGCircuitGauges()
	for _, g := range ragCbGauges {
		out = append(out, MetricSnapshot{
			Name:   g.Name,
			Type:   MetricTypeGauge,
			Labels: g.Labels,
			Value:  g.Value,
		})
	}

	// Judge score counters
	for _, injected := range []bool{true, false} {
		sum := collectorSlow.RAGJudgeScoreSum(injected)
		count := collectorSlow.RAGJudgeScoreCount(injected)
		if count > 0 {
			out = append(out, MetricSnapshot{
				Name:   "nexus_rag_judge_score_sum",
				Type:   MetricTypeCounter,
				Labels: map[string]string{"injected": fmt.Sprintf("%t", injected)},
				Sum:    sum,
			})
			out = append(out, MetricSnapshot{
				Name:   "nexus_rag_judge_score_count",
				Type:   MetricTypeCounter,
				Labels: map[string]string{"injected": fmt.Sprintf("%t", injected)},
				Sum:    float64(count),
			})
		}
	}

	// Embedder failures
	embedderFailures := collectorSlow.EmbedderFailures()
	for kind, cnt := range embedderFailures {
		out = append(out, MetricSnapshot{
			Name:   "nexus_embedder_failures_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"kind": kind},
			Sum:    float64(cnt),
		})
	}

	// RAG circuit trips/recovers
	ragTrips := collectorSlow.RAGCircuitTrips()
	for kind, cnt := range ragTrips {
		out = append(out, MetricSnapshot{
			Name:   "nexus_rag_circuit_trips_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"kind": kind},
			Sum:    float64(cnt),
		})
	}
	ragRecovers := collectorSlow.RAGCircuitRecovers()
	for kind, cnt := range ragRecovers {
		out = append(out, MetricSnapshot{
			Name:   "nexus_rag_circuit_recovers_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"kind": kind},
			Sum:    float64(cnt),
		})
	}

	// Frontier health
	frontierProbes := collectorSlow.FrontierProbeTotals()
	for key, cnt := range frontierProbes {
		// key format: "provider|result"
		var provider, result string
		fmt.Sscanf(key, "%[^|]|%s", &provider, &result)
		out = append(out, MetricSnapshot{
			Name:   "nexus_frontier_probe_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"provider": provider, "result": result},
			Sum:    float64(cnt),
		})
	}
	frontierCircuitOpen := collectorSlow.FrontierCircuitOpenTotals()
	for provider, cnt := range frontierCircuitOpen {
		out = append(out, MetricSnapshot{
			Name:   "nexus_frontier_circuit_open_total",
			Type:   MetricTypeCounter,
			Labels: map[string]string{"provider": provider},
			Sum:    float64(cnt),
		})
	}

	// Confidence errors
	out = append(out, MetricSnapshot{
		Name: "nexus_confidence_errors_total",
		Type: MetricTypeCounter,
		Sum:  float64(collectorSlow.ConfidenceErrors()),
	})

	return out
}

// --- OTLP/JSON envelope for metrics -----------------------------------
//
// The OTLP HTTP/JSON metrics schema wraps metric data inside
// resourceMetrics[] / scopeMetrics[] / metrics[]. We attach one
// service.name="nexus-proxy" resource and one scopeMetrics entry per
// POST. See:
// https://opentelemetry.io/docs/specs/otlp/#otlphttp

type otlpMetricsAttrValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *int64   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpMetricsAttr struct {
	Key   string               `json:"key"`
	Value otlpMetricsAttrValue `json:"value"`
}

type otlpNumberDataPoint struct {
	TimeUnixNano      string            `json:"timeUnixNano"`
	AsInt             *uint64           `json:"asInt,omitempty"`
	AsDouble          *float64          `json:"asDouble,omitempty"`
	Attributes        []otlpMetricsAttr `json:"attributes,omitempty"`
	Flags             int               `json:"flags"`
	StartTimeUnixNano string            `json:"startTimeUnixNano,omitempty"`
	Exemplars         []otlpExemplar    `json:"exemplars,omitempty"`
}

type otlpExemplar struct {
	TimeUnixNano string            `json:"timeUnixNano"`
	AsDouble     *float64          `json:"asDouble,omitempty"`
	AsInt        *uint64           `json:"asInt,omitempty"`
	Attributes   []otlpMetricsAttr `json:"attributes,omitempty"`
}

type otlpHistogramDataPoint struct {
	TimeUnixNano      string            `json:"timeUnixNano"`
	Count             uint64            `json:"count"`
	Sum               float64           `json:"sum"`
	Buckets           []otlpBucket      `json:"buckets"`
	Attributes        []otlpMetricsAttr `json:"attributes,omitempty"`
	Flags             int               `json:"flags"`
	StartTimeUnixNano string            `json:"startTimeUnixNano,omitempty"`
}

type otlpBucket struct {
	Count     uint64         `json:"count"`
	Exemplars []otlpExemplar `json:"exemplars,omitempty"`
}

type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints,omitempty"`
}

type otlpSum struct {
	DataPoints  []otlpNumberDataPoint `json:"dataPoints,omitempty"`
	IsMonotonic bool                  `json:"isMonotonic"`
}

type otlpHistogram struct {
	DataPoints []otlpHistogramDataPoint `json:"dataPoints,omitempty"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics,omitempty"`
}

type otlpMetricsResource struct {
	Attributes []otlpMetricsAttr `json:"attributes"`
}

type otlpMetricsResourceSpans struct {
	Resource     otlpMetricsResource `json:"resource"`
	ScopeMetrics []otlpScopeMetrics  `json:"scopeMetrics"`
}

type otlpMetricsPayload struct {
	ResourceSpans []otlpMetricsResourceSpans `json:"resourceMetrics"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// buildOTLPMetricsJSON marshals the metric snapshot into the OTLP/JSON
// envelope.
func buildOTLPMetricsJSON(snapshot []MetricSnapshot) ([]byte, error) {
	svc := OtelMetricsServiceName
	rs := otlpMetricsResourceSpans{
		Resource: otlpMetricsResource{
			Attributes: []otlpMetricsAttr{{
				Key:   "service.name",
				Value: otlpMetricsAttrValue{StringValue: &svc},
			}},
		},
	}
	scopeMetrics := otlpScopeMetrics{
		Scope: otlpScope{Name: OtelMetricsScopeName},
	}
	for _, m := range snapshot {
		metric := otlpMetric{Name: m.Name}
		labels := buildLabels(m.Labels)
		nowNano := fmt.Sprintf("%d", time.Now().UnixNano())

		switch m.Type {
		case MetricTypeCounter:
			metric.Sum = &otlpSum{
				IsMonotonic: true,
				DataPoints: []otlpNumberDataPoint{{
					TimeUnixNano:      nowNano,
					AsDouble:          &m.Sum,
					Attributes:        labels,
					StartTimeUnixNano: "0",
				}},
			}
		case MetricTypeGauge:
			metric.Gauge = &otlpGauge{
				DataPoints: []otlpNumberDataPoint{{
					TimeUnixNano: nowNano,
					AsDouble:     &m.Value,
					Attributes:   labels,
				}},
			}
		case MetricTypeHistogram:
			var buckets []otlpBucket
			for _, b := range m.HistogramBuckets {
				buckets = append(buckets, otlpBucket{Count: b.Count})
			}
			metric.Histogram = &otlpHistogram{
				DataPoints: []otlpHistogramDataPoint{{
					TimeUnixNano:      nowNano,
					Count:             m.HistogramCount,
					Sum:               m.HistogramSum,
					Buckets:           buckets,
					Attributes:        labels,
					StartTimeUnixNano: "0",
				}},
			}
		}
		scopeMetrics.Metrics = append(scopeMetrics.Metrics, metric)
	}
	rs.ScopeMetrics = []otlpScopeMetrics{scopeMetrics}
	payload := otlpMetricsPayload{ResourceSpans: []otlpMetricsResourceSpans{rs}}
	return json.Marshal(payload)
}

// buildLabels converts a label map into OTLP attribute format.
func buildLabels(m map[string]string) []otlpMetricsAttr {
	if len(m) == 0 {
		return nil
	}
	out := make([]otlpMetricsAttr, 0, len(m))
	for k, v := range m {
		out = append(out, otlpMetricsAttr{
			Key:   k,
			Value: otlpMetricsAttrValue{StringValue: &v},
		})
	}
	return out
}

// --- Slow collector readout methods ------------------------------------
//
// These methods live on the Collector but are not in collector.go's
// hot-path section (they are only called from the OTLP export goroutine,
// not the request path). Adding them to the Collector struct directly
// would make the file even larger, so we define them here alongside the
// exporter that uses them.
//
// collectorSlow is a package-level pointer to the Collector set during
// server boot. This avoids threading the Collector through to the exporter
// as an interface{}.

var collectorSlow *Collector

// globalOtelMetricsExporter holds the currently registered OTEL metrics
// exporter so the Prometheus gauge provider in server.go can read the
// export-failure counter without threading the exporter through separately.
var globalOtelMetricsExporter atomic.Value // stores *OtelMetricsExporter

// RegisterOtelMetricsExporter registers the global OTEL metrics exporter.
func RegisterOtelMetricsExporter(e *OtelMetricsExporter) {
	if e != nil {
		globalOtelMetricsExporter.Store(e)
	}
}

// GlobalOtelMetricsExporter returns the globally registered OTEL metrics
// exporter, or nil if none is configured.
func GlobalOtelMetricsExporter() *OtelMetricsExporter {
	if v := globalOtelMetricsExporter.Load(); v != nil {
		return v.(*OtelMetricsExporter)
	}
	return nil
}

// RegisterCollector registers the global collector for OTLP metric export.
// Called once from server.go during boot.
func RegisterCollector(c *Collector) {
	collectorSlow = c
}

// InputTokensTotal returns the cumulative input token count.
func (c *Collector) InputTokensTotal() uint64 {
	if c == nil {
		return 0
	}
	return c.inputTokensTotal.Load()
}

// OutputTokensTotal returns the cumulative output token count.
func (c *Collector) OutputTokensTotal() uint64 {
	if c == nil {
		return 0
	}
	return c.outputTokensTotal.Load()
}

// TOONSavingsTokensTotal returns the cumulative TOON-savings token count.
func (c *Collector) TOONSavingsTokensTotal() uint64 {
	if c == nil {
		return 0
	}
	return c.toonSavingsTokensTotal.Load()
}

// TOONCompressed returns the cumulative TOON compression count.
func (c *Collector) TOONCompressed() uint64 {
	if c == nil {
		return 0
	}
	return c.toonCompressedTotal.Load()
}

// Degraded returns the cumulative degraded request count.
func (c *Collector) Degraded() uint64 {
	if c == nil {
		return 0
	}
	return c.degradedTotal.Load()
}

// RAGHits returns the cumulative RAG hit count.
func (c *Collector) RAGHits() uint64 {
	if c == nil {
		return 0
	}
	return c.ragHitsTotal.Load()
}

// RAGMisses returns the cumulative RAG miss count.
func (c *Collector) RAGMisses() uint64 {
	if c == nil {
		return 0
	}
	return c.ragMissesTotal.Load()
}

// ErrorsLocal returns the cumulative local error count.
func (c *Collector) ErrorsLocal() uint64 {
	if c == nil {
		return 0
	}
	return c.errorsLocal.Load()
}

// ErrorsFrontier returns the cumulative frontier error count.
func (c *Collector) ErrorsFrontier() uint64 {
	if c == nil {
		return 0
	}
	return c.errorsFrontier.Load()
}

// ErrorsFusion returns the cumulative fusion error count.
func (c *Collector) ErrorsFusion() uint64 {
	if c == nil {
		return 0
	}
	return c.errorsFusion.Load()
}

// RateLimitAllowedGlobal returns the cumulative global rate-limit-allowed count.
func (c *Collector) RateLimitAllowedGlobal() uint64 {
	if c == nil {
		return 0
	}
	return c.rateLimitAllowedGlobal.Load()
}

// RateLimitRejectedGlobal returns the cumulative global rate-limit-rejected count.
func (c *Collector) RateLimitRejectedGlobal() uint64 {
	if c == nil {
		return 0
	}
	return c.rateLimitRejectedGlobal.Load()
}

// StageRAG returns the RAG pipeline stage histogram.
func (c *Collector) StageRAG() *Histogram {
	if c == nil {
		return nil
	}
	return c.stageRAG
}

// StagePromptEng returns the prompt-engineering stage histogram.
func (c *Collector) StagePromptEng() *Histogram {
	if c == nil {
		return nil
	}
	return c.stagePromptEng
}

// StageTOON returns the TOON compression stage histogram.
func (c *Collector) StageTOON() *Histogram {
	if c == nil {
		return nil
	}
	return c.stageTOON
}

// StageSLM returns the SLM routing stage histogram.
func (c *Collector) StageSLM() *Histogram {
	if c == nil {
		return nil
	}
	return c.stageSLM
}

// StageUpstream returns the upstream stage histogram.
func (c *Collector) StageUpstream() *Histogram {
	if c == nil {
		return nil
	}
	return c.stageUpstream
}
