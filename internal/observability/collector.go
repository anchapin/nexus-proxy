// Package observability implements an in-process metrics collector and
// a Prometheus text-exposition renderer (issue #40). The collector
// holds lock-free atomic counters and fixed-bucket histograms; the
// renderer emits standard Prometheus text format so a scrape from
// Prometheus (or a plain curl) gets real-time visibility into request
// rate, routing decisions, error rate, latency percentiles, TTFT,
// VRAM budget, judge/quality queue depths, and cost accumulation.
//
// Stdlib-only by design: sync/atomic for the hot path, fmt/io/math for
// rendering. No prometheus/client_golang — the text-exposition format
// is plain fmt.Fprintf output, which is all the spec requires.
package observability

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/health"
)

// DefaultBuckets are the histogram bucket upper bounds (in
// milliseconds) used for both request latency and TTFT. They span
// sub-frame (5 ms) through slow-fusion (30 s); the implicit +Inf
// bucket catches anything beyond. Tuned for the coding-agent workload
// where local Ollama responses land in the 100 ms–2.5 s band and
// frontier streams occasionally exceed 10 s.
var DefaultBuckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

// ConfidenceBuckets are the histogram bucket upper bounds for SLM
// confidence scores (issue #425). They span 0.1 through 1.0 in 0.1
// increments; the implicit +Inf bucket catches any value > 1.0.
var ConfidenceBuckets = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}

// RAGSimilarityBuckets are the histogram bucket upper bounds for RAG
// cosine-similarity scores (issue #447). Cosine similarity on the
// [-1, 1] range is, in practice for code retrieval, 0..1 with the
// default floor at 0.55 — same shape as ConfidenceBuckets so the two
// distributions are visually comparable in Grafana. The implicit
// +Inf bucket catches any value > 1.0 (which should never happen for
// cosine but defends against a buggy embedder emitting >1).
var RAGSimilarityBuckets = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}

// RateLimitUtilizationBuckets are the histogram bucket upper bounds for
// per-client rate-limit bucket utilization fractions (issue #746).
// Quartiles [0.25, 0.50, 0.75, 1.0] let operators see how close each
// bucket is to its burst limit at the moment of acquisition.
var RateLimitUtilizationBuckets = []float64{0.25, 0.50, 0.75, 1.0}

// ragSimilarityLabels are the fixed low-cardinality (path, outcome)
// label pairs used for the RAG similarity histogram (issue #447).
// Both axes are bounded at construction:
//
//   - path: "hnsw" or "brute_force" (see rag.IndexPath)
//   - outcome: "hit" or "miss"
//
// Total cardinality is 4 series. We pre-allocate one histogram per pair
// so ObserveRAGSimilarity is a lock-free single increment — the
// histogram map lookup is read-locked but the histogram's Observe
// method itself never contends.
var ragSimilarityLabels = []struct {
	path    string
	outcome string
}{
	{path: "hnsw", outcome: "hit"},
	{path: "hnsw", outcome: "miss"},
	{path: "brute_force", outcome: "hit"},
	{path: "brute_force", outcome: "miss"},
}

// slmConfidenceCategories are the fixed low-cardinality task-category
// labels used for the SLM confidence histogram (issue #425). They
// mirror the Category* constants in internal/router/confidence.go.
var slmConfidenceCategories = []string{
	"css",
	"refactoring",
	"debugging",
	"architecture",
	"boilerplate",
	"documentation",
	"other",
}

// ObservabilityEvent is the per-request payload the chat handler
// dispatches to the Collector via the ObservabilityObserver hook. Every
// proxied request — success or failure — produces exactly one event;
// the collector increments its atomics from Submit.
//
// The type lives here (not in internal/handlers) because it is the
// collector's own event surface. handlers imports this leaf package
// (no cycle: observability imports nothing internal), so the collector
// satisfies handlers.ObservabilityObserver directly and main.go needs
// no field-copy adapter.
type ObservabilityEvent struct {
	Route string // "local" | "frontier" | "fusion"; unknown values count as frontier

	// Error is non-empty when the upstream call failed. The collector
	// increments errorsTotal iff Error != "".
	Error string

	// Routing/optimisation dimensions.
	RAGInjected    bool // a few-shot snippet was injected into the prompt
	TOONCompressed bool // JSON-array blocks were TOON-compressed
	// TOONCompressionMethod records which TOON compression pattern was applied
	// (issue #1312): "fenced", "nested", "unfenced", or "" (none).
	TOONCompressionMethod string
	Degraded              bool // local arm was skipped because Ollama was unhealthy

	// Token + cost accounting (cumulative across the process lifetime).
	InputTokens       int
	OutputTokens      int
	TOONSavingsTokens int
	EstimatedCostUSD  float64

	// Latency dimensions. TTFTMs is 0 for non-streaming responses.
	TotalLatencyMs int64
	TTFTMs         int64

	// Per-stage latency breakdown (issue #300). Each field is the
	// wall-clock milliseconds spent in that pipeline stage. 0 when
	// the stage was skipped or not applicable.
	RAGRetrievalMs      int64 // RAG embedding + retrieval
	PromptEngineeringMs int64 // meta-prompt injection
	TOONCompressionMs   int64 // JSON array compression
	SLMRoutingMs        int64 // SLM routing decision (including DSL fast-pass)
	UpstreamFirstByteMs int64 // upstream call to first byte (TTFT minus proxy overhead)

	// SLM confidence recording (issue #425). Confidence is 0.0..1.0
	// from the judge-guided adaptive routing store; TaskType is the
	// Categorize() bucket. Both are zero when the SLM was not
	// consulted (guardrail/DSL stages) or on cache hits.
	SLMConfidence float64
	SLMTaskType   string

	// Trace context for exemplar attachment (issue #1171). Populated
	// by the chat handler from the root span; empty when tracing is
	// not active. The collector stores the most recent trace per
	// histogram bucket so Prometheus exemplars link latency outliers
	// to the trace that produced them.
	TraceID string
	SpanID  string
}

// Collector is the in-process metrics surface. It is safe for
// concurrent use: every field is a sync/atomic primitive, so the
// request path (Submit) and the scrape path (Snapshot / RenderPrometheus)
// never contend on a lock. The scrape handler completes in well under a
// millisecond regardless of how many samples have accumulated.
type Collector struct {
	// Per-route request counters. Rendered with a route label so a
	// single PromQL query can break traffic down by destination.
	requestsLocal    atomic.Uint64
	requestsFrontier atomic.Uint64
	requestsFusion   atomic.Uint64

	// Per-route error counters (issue #120).
	errorsLocal    atomic.Uint64
	errorsFrontier atomic.Uint64
	errorsFusion   atomic.Uint64

	ragHitsTotal        atomic.Uint64
	ragMissesTotal      atomic.Uint64
	toonCompressedTotal atomic.Uint64
	// TOON compression counters by type and direction (issue #1312).
	// Keyed by direction: "compress" (decompress not yet implemented).
	toonCompressionFencedTotal   map[string]*atomic.Uint64
	toonCompressionUnfencedTotal map[string]*atomic.Uint64
	degradedTotal                atomic.Uint64
	inputTokensTotal             atomic.Uint64
	outputTokensTotal            atomic.Uint64
	toonSavingsTokensTotal       atomic.Uint64

	// estimatedCostUSDBits holds the cumulative USD cost as its
	// IEEE-754 bit pattern in an atomic.Uint64
	// (math.Float64bits / Float64frombits) so the hot path can
	// accumulate a float without a mutex.
	estimatedCostUSDBits atomic.Uint64

	// Per-route latency and TTFT histograms (issue #120).
	latencyLocal    *Histogram
	latencyFrontier *Histogram
	latencyFusion   *Histogram
	ttftLocal       *Histogram
	ttftFrontier    *Histogram
	ttftFusion      *Histogram

	// Per-stage pipeline latency histograms (issue #300).
	// Labelled by stage: rag, prompt_eng, toon, slm, upstream.
	stageRAG       *Histogram
	stagePromptEng *Histogram
	stageTOON      *Histogram
	stageSLM       *Histogram
	stageUpstream  *Histogram

	// --- Middleware instrumentation (issue #70) ---------------------------
	//
	// Auth counters are labelled by (outcome, client_ip) via maps of
	// atomics. The outcome label has three fixed values (accepted,
	// rejected_invalid, rejected_missing); client_ip is dynamic.
	// Maps are keyed by clientIP to enable per-IP metric tracking so
	// operators can distinguish attack sources (issue #1061).
	// The mutex guards map mutations (adding new IP keys); atomic
	// operations on existing keys are lock-free.
	authMu              sync.Mutex
	authAccepted        map[string]*atomic.Uint64
	authRejectedInvalid map[string]*atomic.Uint64
	authRejectedMissing map[string]*atomic.Uint64

	// Auth limiter reaper evictions counter (issue #839). Incremented
	// each time the reaper goroutine evicts an idle IP from the
	// failures map.
	authReaperEvictions atomic.Uint64

	// Auth limiter blocked counter (issue #831/#937). Incremented each time
	// an IP is blocked (burst threshold crossed) by the auth limiter.
	// Keyed by reason: "missing" or "invalid".
	authBlockedTotal map[string]*atomic.Uint64

	// Rate-limit counters are emitted per bucket (global / per_client)
	// so operators can tell at a glance whether the global bucket or a
	// specific client is the bottleneck (issue #70 AC: "How many
	// requests are 429'd by the rate limiter (per client IP and
	// globally)?").
	rateLimitAllowedGlobal     atomic.Uint64
	rateLimitAllowedPerClient  atomic.Uint64
	rateLimitRejectedGlobal    atomic.Uint64
	rateLimitRejectedPerClient atomic.Uint64

	// Budget counters track daily frontier spend (issue #38).
	// Exceeded is bumped when the gate rejects; RecordedUSD is the
	// running sum (float, lock-free via the bits trick) of amounts
	// the tracker recorded after a frontier call completed.
	budgetExceededTotal   atomic.Uint64
	budgetRecordedUSDBits atomic.Uint64

	// --- Metrics batch counter (issue #1234) ---------------------------
	//
	// metricsBatchTotal counts the number of SQLite batch transactions
	// committed by the metrics store drain goroutine. Each increment
	// represents one BEGIN...INSERT...COMMIT cycle that flushed N records
	// (where N is the batch size or the partial-final batch on timeout).
	metricsBatchTotal atomic.Uint64

	// TLS counters are bumped from main.go via http.Server.ConnState.
	// Accepted fires on http.StateTLSHandshakeComplete; Rejected
	// fires when a connection closes before reaching that state.
	tlsConnectionsAccepted atomic.Uint64
	tlsConnectionsRejected atomic.Uint64

	// --- Circuit breaker instrumentation (issue #304) ---------------
	//
	// Tracks the state of each named circuit breaker (ollama, rag).
	// State values: 0=closed, 1=half_open, 2=open.
	// Protected by cbMu; read via atomic for hot path.
	cbMu    sync.RWMutex
	cbState map[string]*circuitBreakerState

	// --- SLM confidence histogram (issue #425) --------------------
	//
	// Per-task-category confidence histograms. Maps category name to
	// histogram. Pre-allocated in NewCollector so ObserveSLMConfidence
	// only needs a read lock to find the histogram; the histogram's
	// Observe method itself is lock-free.
	slmConfidenceMu         sync.RWMutex
	slmConfidenceHistograms map[string]*Histogram

	// --- RAG similarity histogram (issue #447) -------------------
	//
	// Per-(path, outcome) cosine-similarity histograms. The (path,
	// outcome) label set is fixed at 4 pairs (see ragSimilarityLabels)
	// so we pre-allocate the histograms in NewCollector and never
	// mutate the map after boot. ObserveRAGSimilarity only needs a
	// read lock to find the histogram; Histogram.Observe is lock-free.
	ragSimilarityMu         sync.RWMutex
	ragSimilarityHistograms map[string]*Histogram // keyed by "path|outcome"

	// --- Rate-limit bucket utilization histogram (issue #746) --------
	//
	// Per-bucket-ID utilization histograms. Each histogram records the
	// fractional token utilization (tokens/burst) at the moment of
	// acquisition. Histograms are created lazily per bucket so the
	// hot path is a single read-lock + map lookup; Histogram.Observe
	// itself is lock-free.
	rateLimitUtilizationMu         sync.RWMutex
	rateLimitUtilizationHistograms map[string]*Histogram // keyed by bucketID (hashed IP)

	// --- Embedder circuit breaker instrumentation (issue #423) -----
	//
	// Tracks failures for each embedder circuit breaker (ollama, rag).
	// Protected by cbMu for map access; individual counters are atomic.
	embedderMu       sync.RWMutex
	embedderFailures map[string]*atomic.Uint64 // keyed by "ollama", "openai", "cohere"

	// --- RAG embedder circuit breaker state metrics (issue #886) -----
	//
	// Tracks trip/recover events per embedder kind. State and failure count
	// are read live from health.breakers at scrape time.
	ragCircuitMu       sync.RWMutex
	ragCircuitTrips    map[string]*atomic.Uint64 // keyed by "ollama", "openai", "cohere"
	ragCircuitRecovers map[string]*atomic.Uint64

	// --- Per-route latency percentile ring buffers (issue #774) --------
	//
	// Per-(route) sliding window ring buffers that store recent latency
	// samples and maintain running p50/p95/p99 estimates. Keyed by route
	// string ("local", "frontier", "fusion").
	latencyPercentilesMu sync.RWMutex
	latencyPercentiles   map[string]*latencyPercentileBuffer

	// --- ConfidenceStore error counter (issue #927) -----------------
	//
	// Tracks LocalConfidence errors so operators can detect DB/locking
	// issues in the SQLite-backed confidence store.
	confidenceErrorsTotal atomic.Uint64

	// --- Exemplar gate (issue #1171) --------------------------------
	//
	// When true, the renderer appends OTLP trace exemplars to
	// histogram bucket lines. Set from config (NEXUS_METRICS_EXEMPLARS)
	// during boot. When false, the output is byte-identical to the
	// pre-exemplar renderer because Submit/ObservePipelineStage call
	// plain Observe (no exemplar slots populated).
	exemplarsEnabled atomic.Bool

	// --- RAG-vs-judge quality correlation (issue #1167) -------------
	//
	// Sum and count of judge scores partitioned by whether RAG context
	// was injected. Two label values (true|false) so four atomics
	// total. The sum is stored as IEEE-754 bits (same trick as
	// estimatedCostUSDBits) so the hot path accumulates a float
	// without a mutex. Only valid scores (1..5) are recorded; parse
	// failures are excluded.
	ragJudgeScoreSumBitsTrue  atomic.Uint64
	ragJudgeScoreSumBitsFalse atomic.Uint64
	ragJudgeScoreCountTrue    atomic.Uint64
	ragJudgeScoreCountFalse   atomic.Uint64

	// --- Frontier provider health metrics (issue #1158) -----------------
	//
	// frontierProbeTotal records the cumulative probe count per
	// (provider, result) pair. Keyed by "provider|result" so the
	// Prometheus renderer can emit a labelled counter family.
	// frontierCircuitOpenTotal records the cumulative count of
	// circuit-open transitions per provider.
	frontierHealthMu         sync.RWMutex
	frontierProbeTotal       map[string]*atomic.Uint64 // keyed by "provider|result"
	frontierCircuitOpenTotal map[string]*atomic.Uint64 // keyed by provider

	// --- SLO error budget tracking (issue #1239) ------------------------
	//
	// sloErrorBudgetBits stores the IEEE-754 bits of the error budget
	// remaining ratio per SLO (0..1, where 1 = full budget, 0 = exhausted).
	// Keyed by SLO name: "availability", "local_latency_p99", "ttft_p95".
	// Updated at scrape time from the in-process percentile gauges.
	sloBudgetMu        sync.RWMutex
	sloErrorBudgetBits map[string]*atomic.Uint64 // keyed by SLO name
}

// circuitBreakerState holds the atomic state for one named circuit.
type circuitBreakerState struct {
	state       atomic.Int32 // 0=closed, 1=half_open, 2=open
	failures    atomic.Uint64
	lastFailure atomic.Int64 // Unix timestamp (seconds) of last failure
}

const (
	circuitStateClosed   int32 = 0
	circuitStateHalfOpen int32 = 1
	circuitStateOpen     int32 = 2
)

// NewCollector constructs a Collector with the default latency and TTFT
// histograms for each route and per-stage pipeline histograms (issue #300).
// The returned collector is ready to receive Submit calls and
// RenderPrometheus scrapes from any goroutine.
func NewCollector() *Collector {
	c := &Collector{
		latencyLocal:        NewHistogram(DefaultBuckets),
		latencyFrontier:     NewHistogram(DefaultBuckets),
		latencyFusion:       NewHistogram(DefaultBuckets),
		ttftLocal:           NewHistogram(DefaultBuckets),
		ttftFrontier:        NewHistogram(DefaultBuckets),
		ttftFusion:          NewHistogram(DefaultBuckets),
		stageRAG:            NewHistogram(DefaultBuckets),
		stagePromptEng:      NewHistogram(DefaultBuckets),
		stageTOON:           NewHistogram(DefaultBuckets),
		stageSLM:            NewHistogram(DefaultBuckets),
		stageUpstream:       NewHistogram(DefaultBuckets),
		embedderFailures:    make(map[string]*atomic.Uint64),
		authAccepted:        make(map[string]*atomic.Uint64),
		authRejectedInvalid: make(map[string]*atomic.Uint64),
		authRejectedMissing: make(map[string]*atomic.Uint64),
		authBlockedTotal: map[string]*atomic.Uint64{
			"missing": {},
			"invalid": {},
		},
		// Issue #1312: TOON compression counters keyed by direction.
		toonCompressionFencedTotal: map[string]*atomic.Uint64{
			"compress": {},
		},
		toonCompressionUnfencedTotal: map[string]*atomic.Uint64{
			"compress": {},
		},
	}
	// Pre-allocate SLM confidence histograms for each known category
	// (issue #425). Pre-allocation means ObserveSLMConfidence only
	// needs a read lock to find the histogram; Histogram.Observe
	// itself is lock-free.
	c.slmConfidenceHistograms = make(map[string]*Histogram, len(slmConfidenceCategories))
	for _, cat := range slmConfidenceCategories {
		c.slmConfidenceHistograms[cat] = NewHistogram(ConfidenceBuckets)
	}
	// Pre-allocate RAG similarity histograms for the default (path, outcome)
	// pairs (issue #447). Additional histograms for per-threshold observations
	// are created lazily by ObserveRAGSimilarity (issue #671).
	c.ragSimilarityHistograms = make(map[string]*Histogram, len(ragSimilarityLabels))
	for _, l := range ragSimilarityLabels {
		key := ragSimilarityKey(l.path, l.outcome, 0) // 0 = default/global threshold
		c.ragSimilarityHistograms[key] = NewHistogram(RAGSimilarityBuckets)
	}
	// Pre-allocate SLO error budget storage (issue #1239).
	c.sloErrorBudgetBits = make(map[string]*atomic.Uint64, 3)
	for _, slo := range []string{"availability", "local_latency_p99", "ttft_p95"} {
		c.sloErrorBudgetBits[slo] = &atomic.Uint64{}
	}
	return c
}

// ragSimilarityKey builds the lookup key for the (path, outcome, threshold) tuple.
// Stable across processes so scrape diffs are reproducible.
// The threshold is rounded to 2 decimal places to avoid floating-point
// key explosion while still providing per-threshold visibility (issue #671).
func ragSimilarityKey(path, outcome string, threshold float64) string {
	return fmt.Sprintf("%s|%s|%.2f", path, outcome, threshold)
}

// Submit records one ObservabilityEvent. Called exactly once per
// proxied request from the chat handler's request goroutine. Submit is
// a sequence of atomic increments — it never blocks, never allocates,
// and is safe to call from many goroutines concurrently.
func (c *Collector) Submit(e ObservabilityEvent) {
	var latencyHist *Histogram
	var ttftHist *Histogram
	switch e.Route {
	case "local":
		c.requestsLocal.Add(1)
		if e.Error != "" {
			c.errorsLocal.Add(1)
		}
		latencyHist = c.latencyLocal
		ttftHist = c.ttftLocal
	case "fusion":
		c.requestsFusion.Add(1)
		if e.Error != "" {
			c.errorsFusion.Add(1)
		}
		latencyHist = c.latencyFusion
		ttftHist = c.ttftFusion
	default: // "frontier" and any unrecognised route count as frontier
		c.requestsFrontier.Add(1)
		if e.Error != "" {
			c.errorsFrontier.Add(1)
		}
		latencyHist = c.latencyFrontier
		ttftHist = c.ttftFrontier
	}
	if e.RAGInjected {
		c.ragHitsTotal.Add(1)
	} else {
		c.ragMissesTotal.Add(1)
	}
	if e.TOONCompressed {
		c.toonCompressedTotal.Add(1)
	}
	// Issue #1312: increment fenced vs unfenced counter based on compression method.
	if e.TOONCompressionMethod != "" {
		dir := "compress" // direction label; decompress not yet implemented
		switch e.TOONCompressionMethod {
		case "fenced":
			if c.toonCompressionFencedTotal[dir] != nil {
				c.toonCompressionFencedTotal[dir].Add(1)
			}
		case "unfenced":
			if c.toonCompressionUnfencedTotal[dir] != nil {
				c.toonCompressionUnfencedTotal[dir].Add(1)
			}
			// "nested" is tracked by the existing toonCompressedTotal but does not
			// get its own fenced/unfenced counter since it is a subset of compression.
		}
	}
	if e.Degraded {
		c.degradedTotal.Add(1)
	}
	if e.InputTokens > 0 {
		c.inputTokensTotal.Add(uint64(e.InputTokens))
	}
	if e.OutputTokens > 0 {
		c.outputTokensTotal.Add(uint64(e.OutputTokens))
	}
	if e.TOONSavingsTokens > 0 {
		c.toonSavingsTokensTotal.Add(uint64(e.TOONSavingsTokens))
	}
	if e.EstimatedCostUSD > 0 {
		atomicAddFloat(&c.estimatedCostUSDBits, e.EstimatedCostUSD)
	}
	if e.TotalLatencyMs > 0 && latencyHist != nil {
		if e.TraceID != "" {
			latencyHist.ObserveWithExemplar(float64(e.TotalLatencyMs), Exemplar{TraceID: e.TraceID, SpanID: e.SpanID, Value: float64(e.TotalLatencyMs)})
		} else {
			latencyHist.Observe(float64(e.TotalLatencyMs))
		}
	}
	if e.TTFTMs > 0 && ttftHist != nil {
		if e.TraceID != "" {
			ttftHist.ObserveWithExemplar(float64(e.TTFTMs), Exemplar{TraceID: e.TraceID, SpanID: e.SpanID, Value: float64(e.TTFTMs)})
		} else {
			ttftHist.Observe(float64(e.TTFTMs))
		}
	}
	// Per-stage pipeline latency histograms (issue #300).
	ex := Exemplar{TraceID: e.TraceID, SpanID: e.SpanID}
	if e.RAGRetrievalMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.RAGRetrievalMs)
			c.stageRAG.ObserveWithExemplar(float64(e.RAGRetrievalMs), ex)
		} else {
			c.stageRAG.Observe(float64(e.RAGRetrievalMs))
		}
	}
	if e.PromptEngineeringMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.PromptEngineeringMs)
			c.stagePromptEng.ObserveWithExemplar(float64(e.PromptEngineeringMs), ex)
		} else {
			c.stagePromptEng.Observe(float64(e.PromptEngineeringMs))
		}
	}
	if e.TOONCompressionMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.TOONCompressionMs)
			c.stageTOON.ObserveWithExemplar(float64(e.TOONCompressionMs), ex)
		} else {
			c.stageTOON.Observe(float64(e.TOONCompressionMs))
		}
	}
	if e.SLMRoutingMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.SLMRoutingMs)
			c.stageSLM.ObserveWithExemplar(float64(e.SLMRoutingMs), ex)
		} else {
			c.stageSLM.Observe(float64(e.SLMRoutingMs))
		}
	}
	if e.UpstreamFirstByteMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.UpstreamFirstByteMs)
			c.stageUpstream.ObserveWithExemplar(float64(e.UpstreamFirstByteMs), ex)
		} else {
			c.stageUpstream.Observe(float64(e.UpstreamFirstByteMs))
		}
	}
	// Confidence > 0 and TaskType is a known category. A zero
	// confidence means the SLM was not consulted (guardrail/DSL
	// path); an empty TaskType means cache hit or no confidence
	// store was wired.
	if e.SLMConfidence > 0 && e.SLMTaskType != "" {
		c.ObserveSLMConfidence(e.SLMTaskType, e.SLMConfidence)
	}
}

// RequestsLocal returns the cumulative local-route request count.
// Exported for tests and operational tooling.
func (c *Collector) RequestsLocal() uint64 { return c.requestsLocal.Load() }

// RequestsFrontier returns the cumulative frontier-route request count.
func (c *Collector) RequestsFrontier() uint64 { return c.requestsFrontier.Load() }

// RequestsFusion returns the cumulative fusion-route request count.
func (c *Collector) RequestsFusion() uint64 { return c.requestsFusion.Load() }

// EstimatedCostUSD returns the cumulative estimated frontier cost in USD.
func (c *Collector) EstimatedCostUSD() float64 {
	return math.Float64frombits(c.estimatedCostUSDBits.Load())
}

// Latency returns the request-latency histogram (legacy, returns local for backward compatibility).
// Deprecated: use LatencyLocal, LatencyFrontier, or LatencyFusion.
func (c *Collector) Latency() *Histogram { return c.latencyLocal }

// TTFT returns the time-to-first-token histogram (legacy, returns local for backward compatibility).
// Deprecated: use TTFTLocal, TTFTFrontier, or TTFTFusion.
func (c *Collector) TTFT() *Histogram { return c.ttftLocal }

// LatencyLocal returns the local-route request-latency histogram.
func (c *Collector) LatencyLocal() *Histogram { return c.latencyLocal }

// LatencyFrontier returns the frontier-route request-latency histogram.
func (c *Collector) LatencyFrontier() *Histogram { return c.latencyFrontier }

// LatencyFusion returns the fusion-route request-latency histogram.
func (c *Collector) LatencyFusion() *Histogram { return c.latencyFusion }

// TTFTLocal returns the local-route time-to-first-token histogram.
func (c *Collector) TTFTLocal() *Histogram { return c.ttftLocal }

// TTFTFrontier returns the frontier-route time-to-first-token histogram.
func (c *Collector) TTFTFrontier() *Histogram { return c.ttftFrontier }

// TTFTFusion returns the fusion-route time-to-first-token histogram.
func (c *Collector) TTFTFusion() *Histogram { return c.ttftFusion }

// ObserveLatency records a latency observation for percentile computation
// (issue #774). route is the routing decision ("local", "frontier", "fusion").
// latencyMs is the total request latency in milliseconds. Safe for concurrent use.
func (c *Collector) ObserveLatency(route string, latencyMs int64) {
	if c == nil || latencyMs <= 0 {
		return
	}
	c.latencyPercentilesMu.RLock()
	buf, ok := c.latencyPercentiles[route]
	c.latencyPercentilesMu.RUnlock()
	if ok && buf != nil {
		buf.Observe(float64(latencyMs))
		return
	}
	// Lazily create buffer
	c.latencyPercentilesMu.Lock()
	if c.latencyPercentiles == nil {
		c.latencyPercentiles = make(map[string]*latencyPercentileBuffer)
	}
	buf, ok = c.latencyPercentiles[route]
	if !ok || buf == nil {
		buf = newLatencyPercentileBuffer(defaultLatencyBufferCapacity)
		c.latencyPercentiles[route] = buf
	}
	c.latencyPercentilesMu.Unlock()
	buf.Observe(float64(latencyMs))
}

// LatencyPercentileGauges returns the current p50/p95/p99 latency readings
// per route as GaugeSamples for the Prometheus renderer. Values are 0 when
// no samples have been recorded for a route.
func (c *Collector) LatencyPercentileGauges() []GaugeSample {
	if c == nil {
		return nil
	}
	c.latencyPercentilesMu.RLock()
	defer c.latencyPercentilesMu.RUnlock()
	var out []GaugeSample
	// Fixed route order for deterministic output
	for _, route := range []string{"local", "frontier", "fusion"} {
		buf := c.latencyPercentiles[route]
		if buf == nil {
			continue
		}
		p50, p95, p99 := buf.Perc()
		labels := map[string]string{"route": route}
		out = append(out,
			GaugeSample{Name: "nexus_upstream_request_latency_p50_seconds", Labels: labels, Value: p50 / 1000}, // ms → s
			GaugeSample{Name: "nexus_upstream_request_latency_p95_seconds", Labels: labels, Value: p95 / 1000},
			GaugeSample{Name: "nexus_upstream_request_latency_p99_seconds", Labels: labels, Value: p99 / 1000},
		)
	}
	return out
}

// --- Middleware instrumentation helpers (issue #70) ----------------------
//
// Each helper bumps exactly one atomic counter so the middleware hot
// path stays a single atomic add. The middleware packages own the
// decision logic (when a request is "accepted" vs "rejected_invalid"
// etc.); the collector only stores the resulting counts.

// AuthCountersSnapshot returns a shallow snapshot of the three auth
// counter maps (accepted, rejectedInvalid, rejectedMissing) under
// authMu so callers can iterate without racing against IncAuth* writers.
func (c *Collector) AuthCountersSnapshot() (accepted, rejectedInvalid, rejectedMissing map[string]*atomic.Uint64) {
	c.authMu.Lock()
	accepted = make(map[string]*atomic.Uint64, len(c.authAccepted))
	for k, v := range c.authAccepted {
		accepted[k] = v
	}
	rejectedInvalid = make(map[string]*atomic.Uint64, len(c.authRejectedInvalid))
	for k, v := range c.authRejectedInvalid {
		rejectedInvalid[k] = v
	}
	rejectedMissing = make(map[string]*atomic.Uint64, len(c.authRejectedMissing))
	for k, v := range c.authRejectedMissing {
		rejectedMissing[k] = v
	}
	c.authMu.Unlock()
	return
}

// IncAuthAccepted records one accepted authentication request from the
// given client IP (issue #1061).
func (c *Collector) IncAuthAccepted(clientIP string) {
	c.authMu.Lock()
	if _, ok := c.authAccepted[clientIP]; !ok {
		c.authAccepted[clientIP] = &atomic.Uint64{}
	}
	c.authAccepted[clientIP].Add(1)
	c.authMu.Unlock()
}

// IncAuthRejectedInvalid records a request that presented a
// credential but it did not match any configured key, from the given
// client IP (issue #1061).
func (c *Collector) IncAuthRejectedInvalid(clientIP string) {
	c.authMu.Lock()
	if _, ok := c.authRejectedInvalid[clientIP]; !ok {
		c.authRejectedInvalid[clientIP] = &atomic.Uint64{}
	}
	c.authRejectedInvalid[clientIP].Add(1)
	c.authMu.Unlock()
}

// IncAuthRejectedMissing records a request that presented no
// credential at all (no Authorization / X-API-Key header), from the
// given client IP (issue #1061).
func (c *Collector) IncAuthRejectedMissing(clientIP string) {
	c.authMu.Lock()
	if _, ok := c.authRejectedMissing[clientIP]; !ok {
		c.authRejectedMissing[clientIP] = &atomic.Uint64{}
	}
	c.authRejectedMissing[clientIP].Add(1)
	c.authMu.Unlock()
}

// IncAuthReaperEvictions records one reaper eviction of an idle IP
// from the auth limiter's failures map (issue #839).
func (c *Collector) IncAuthReaperEvictions() { c.authReaperEvictions.Add(1) }

// IncAuthBlocked records one auth limiter block event — an IP that
// crossed the burst threshold and is now blocked (issue #831/#937).
// reason is "missing" or "invalid", indicating which auth failure type
// accumulated to the burst threshold.
func (c *Collector) IncAuthBlocked(reason string) { c.authBlockedTotal[reason].Add(1) }

// AuthAuthenticatedClients returns the cumulative count of accepted
// authentications across all client IPs. The /metrics renderer exposes
// it under the gauge name nexus_auth_authenticated_clients so operators
// can chart a running total of successful auth events without scraping
// logs.
//
// (The name carries "clients" rather than "events" because the issue
// spec calls for a gauge by that name; semantically this is a
// monotonic counter rendered as a gauge family so a single PromQL
// query shows the long-running trend.)
func (c *Collector) AuthAuthenticatedClients() uint64 {
	var total uint64
	for _, v := range c.authAccepted {
		total += v.Load()
	}
	return total
}

// IncRateLimit bumps the appropriate rate-limit counter for scope
// (one of "global", "per_client"). The middleware packages own the
// mapping from configuration to scope label.
//
// A scope other than "global" or "per_client" is silently ignored
// rather than treated as a default — the renderer only knows those
// two label values, so a third bucket would be invisible. Invalid
// scopes indicate a wiring bug worth surfacing in logs at the call
// site rather than silently dropping.
func (c *Collector) IncRateLimit(scope string, allowed bool) {
	switch scope {
	case "global":
		if allowed {
			c.rateLimitAllowedGlobal.Add(1)
		} else {
			c.rateLimitRejectedGlobal.Add(1)
		}
	case "per_client":
		if allowed {
			c.rateLimitAllowedPerClient.Add(1)
		} else {
			c.rateLimitRejectedPerClient.Add(1)
		}
	}
}

// IncBudgetExceeded bumps the budget-exceeded counter when the
// SpendGate rejects a frontier request (issue #70 AC: "How often is
// the daily frontier budget hit?").
func (c *Collector) IncBudgetExceeded() { c.budgetExceededTotal.Add(1) }

// AddBudgetRecorded adds amount to the cumulative recorded-spend
// counter. The collector mirrors the SpendTracker.Record behaviour:
// positive amounts only, lock-free via the bits trick.
func (c *Collector) AddBudgetRecorded(amount float64) {
	if amount > 0 {
		atomicAddFloat(&c.budgetRecordedUSDBits, amount)
	}
}

// BudgetRecordedUSD returns the cumulative USD the budget tracker
// recorded (sum of all Record calls). The /metrics renderer exposes
// it as the gauge nexus_budget_recorded_usd_total.
//
// The gauge name carries "_total" because it is monotonic; the
// renderer types it as "counter" in the Prometheus exposition.
func (c *Collector) BudgetRecordedUSD() float64 {
	return math.Float64frombits(c.budgetRecordedUSDBits.Load())
}

// BudgetExceeded returns the cumulative budget-exceeded count.
func (c *Collector) BudgetExceeded() uint64 { return c.budgetExceededTotal.Load() }

// IncMetricsBatch increments the batch-transaction counter (issue #1234).
// Called from the metrics and judge SQLite store drain goroutines whenever
// a BEGIN...INSERT...COMMIT cycle completes.
func (c *Collector) IncMetricsBatch() { c.metricsBatchTotal.Add(1) }

// IncTLSAccepted bumps the accepted TLS-handshake counter. Wired
// from main.go via http.Server.ConnState on
// http.StateTLSHandshakeComplete.
func (c *Collector) IncTLSAccepted() { c.tlsConnectionsAccepted.Add(1) }

// IncTLSRejected bumps the rejected TLS-handshake counter. Wired
// from main.go via http.Server.ConnState for connections that
// close before reaching http.StateTLSHandshakeComplete.
func (c *Collector) IncTLSRejected() { c.tlsConnectionsRejected.Add(1) }

// --- Circuit breaker instrumentation (issue #304) --------------------
//
// RecordCircuitFailure records a failure for the named circuit and
// transitions its state to "open". Called from the chat handler when
// the local cooldown or RAG breaker trips.
func (c *Collector) RecordCircuitFailure(circuit string) {
	if circuit == "" {
		return
	}
	cb := c.getOrCreateCircuit(circuit)
	cb.state.Store(circuitStateOpen)
	cb.failures.Add(1)
	cb.lastFailure.Store(time.Now().Unix())
}

// RecordCircuitRecovery transitions the named circuit back to "closed".
// Called from the chat handler when the cooldown window expires or a
// RAG request succeeds after the breaker was open.
func (c *Collector) RecordCircuitRecovery(circuit string) {
	if circuit == "" {
		return
	}
	cb := c.getOrCreateCircuit(circuit)
	cb.state.Store(circuitStateClosed)
}

// RecordCircuitHalfOpen transitions the named circuit to "half_open".
// Used when a circuit begins recovery but hasn't fully closed yet.
func (c *Collector) RecordCircuitHalfOpen(circuit string) {
	if circuit == "" {
		return
	}
	cb := c.getOrCreateCircuit(circuit)
	cb.state.Store(circuitStateHalfOpen)
}

// CircuitBreakerGauges returns the live state of all tracked circuit
// breakers as gauge samples for the Prometheus renderer. Each circuit
// emits three samples: state (0=closed, 1=half_open, 2=open),
// failures_total, and last_failure_seconds.
func (c *Collector) CircuitBreakerGauges() []GaugeSample {
	var out []GaugeSample
	c.cbMu.RLock()
	defer c.cbMu.RUnlock()
	for name, cb := range c.cbState {
		lastFail := cb.lastFailure.Load()
		labels := map[string]string{"circuit": name}
		out = append(out,
			GaugeSample{Name: "nexus_circuit_breaker_state", Labels: labels, Value: float64(cb.state.Load())},
			GaugeSample{Name: "nexus_circuit_breaker_failures_total", Labels: labels, Value: float64(cb.failures.Load())},
			GaugeSample{Name: "nexus_circuit_breaker_last_failure_seconds", Labels: labels, Value: float64(lastFail)},
		)
	}
	return out
}

// RAGCircuitGauges returns live state and failure count readings for all
// registered RAG embedder circuit breakers (issue #886). State values:
// 0=closed, 1=half_open, 2=open. Reads directly from health.breakers
// via health.GetBreakerStates so gauges are always current at scrape time.
func (c *Collector) RAGCircuitGauges() []GaugeSample {
	var out []GaugeSample
	states := health.GetBreakerStates()
	for kind, st := range states {
		labels := map[string]string{"service": kind}
		out = append(out,
			GaugeSample{Name: "nexus_rag_circuit_state", Labels: labels, Value: float64(st.State)},
			GaugeSample{Name: "nexus_rag_circuit_failure_count", Labels: labels, Value: float64(st.FailureCount)},
		)
	}
	return out
}

// Gauges implements GaugeProvider so *Collector can be passed
// directly to RenderPrometheus via the RouteCounters.Handler() chain
// (issue #443). It returns the circuit-breaker state, failures,
// last-failure samples, RAG circuit breaker state/failure count (issue #886),
// latency percentile gauges (issue #774), and SLO error budget gauges
// (issue #1239).
// Safe for a nil receiver — returns nil so the collector can be
// omitted without panicking during boot or in tests.
func (c *Collector) Gauges() []GaugeSample {
	if c == nil {
		return nil
	}
	var out []GaugeSample
	out = append(out, c.CircuitBreakerGauges()...)
	out = append(out, c.RAGCircuitGauges()...)
	out = append(out, c.LatencyPercentileGauges()...)
	out = append(out, c.SLOErrorBudgetGauges()...)
	return out
}

// getOrCreateCircuit returns the state for a named circuit, creating
// it if first seen. Caller must hold cbMu.
func (c *Collector) getOrCreateCircuit(name string) *circuitBreakerState {
	if c.cbState == nil {
		c.cbState = make(map[string]*circuitBreakerState)
	}
	if c.cbState[name] == nil {
		c.cbState[name] = &circuitBreakerState{}
	}
	return c.cbState[name]
}

// IncEmbedderFailure increments the failure counter for the given embedder kind
// (one of "ollama", "openai", "cohere"). Called when an embedder circuit breaker
// trips (issue #423).
func (c *Collector) IncEmbedderFailure(kind string) {
	if kind == "" {
		return
	}
	c.embedderMu.Lock()
	defer c.embedderMu.Unlock()
	if c.embedderFailures == nil {
		c.embedderFailures = make(map[string]*atomic.Uint64)
	}
	if c.embedderFailures[kind] == nil {
		c.embedderFailures[kind] = new(atomic.Uint64)
	}
	c.embedderFailures[kind].Add(1)
}

// EmbedderFailures returns the current failure counts keyed by embedder kind.
// Used by the Prometheus renderer.
func (c *Collector) EmbedderFailures() map[string]uint64 {
	c.embedderMu.RLock()
	defer c.embedderMu.RUnlock()
	out := make(map[string]uint64, len(c.embedderFailures))
	for k, v := range c.embedderFailures {
		out[k] = v.Load()
	}
	return out
}

// IncRAGCircuitTrip increments the trip counter for the given embedder kind
// (one of "ollama", "openai", "cohere"). Called when a RAG embedder circuit
// breaker trips (issue #886).
func (c *Collector) IncRAGCircuitTrip(kind string) {
	if kind == "" {
		return
	}
	c.ragCircuitMu.Lock()
	defer c.ragCircuitMu.Unlock()
	if c.ragCircuitTrips == nil {
		c.ragCircuitTrips = make(map[string]*atomic.Uint64)
	}
	if c.ragCircuitTrips[kind] == nil {
		c.ragCircuitTrips[kind] = new(atomic.Uint64)
	}
	c.ragCircuitTrips[kind].Add(1)
}

// IncRAGCircuitRecover increments the recovery counter for the given embedder kind
// (one of "ollama", "openai", "cohere"). Called when a RAG embedder circuit
// breaker recovers (issue #886).
func (c *Collector) IncRAGCircuitRecover(kind string) {
	if kind == "" {
		return
	}
	c.ragCircuitMu.Lock()
	defer c.ragCircuitMu.Unlock()
	if c.ragCircuitRecovers == nil {
		c.ragCircuitRecovers = make(map[string]*atomic.Uint64)
	}
	if c.ragCircuitRecovers[kind] == nil {
		c.ragCircuitRecovers[kind] = new(atomic.Uint64)
	}
	c.ragCircuitRecovers[kind].Add(1)
}

// RAGCircuitTrips returns the current trip counts keyed by embedder kind.
// Used by the Prometheus renderer (issue #886).
func (c *Collector) RAGCircuitTrips() map[string]uint64 {
	c.ragCircuitMu.RLock()
	defer c.ragCircuitMu.RUnlock()
	out := make(map[string]uint64, len(c.ragCircuitTrips))
	for k, v := range c.ragCircuitTrips {
		out[k] = v.Load()
	}
	return out
}

// RAGCircuitRecovers returns the current recovery counts keyed by embedder kind.
// Used by the Prometheus renderer (issue #886).
func (c *Collector) RAGCircuitRecovers() map[string]uint64 {
	c.ragCircuitMu.RLock()
	defer c.ragCircuitMu.RUnlock()
	out := make(map[string]uint64, len(c.ragCircuitRecovers))
	for k, v := range c.ragCircuitRecovers {
		out[k] = v.Load()
	}
	return out
}

// --- ConfidenceStore error counter (issue #927) --------------------

// IncConfidenceError increments the confidence store error counter.
// Called when LocalConfidence returns an error so operators can detect
// DB locking or other SQLite errors in the confidence store path.
func (c *Collector) IncConfidenceError() { c.confidenceErrorsTotal.Add(1) }

// ConfidenceErrors returns the cumulative confidence store error count.
// Used by the Prometheus renderer (issue #927).
func (c *Collector) ConfidenceErrors() uint64 { return c.confidenceErrorsTotal.Load() }

// --- Exemplar gate (issue #1171) ---------------------------------------

// SetExemplarsEnabled controls whether the Prometheus renderer emits
// OTLP trace exemplars on histogram bucket lines. Call once during boot
// from config (NEXUS_METRICS_EXEMPLARS). When false, Submit and
// ObservePipelineStage use plain Observe (no exemplar stored) and the
// renderer omits exemplar suffixes, keeping output byte-identical to
// the pre-exemplar build.
func (c *Collector) SetExemplarsEnabled(enabled bool) {
	if c == nil {
		return
	}
	c.exemplarsEnabled.Store(enabled)
}

// ExemplarsEnabled reports whether the collector is configured to emit
// exemplars. Used by the Prometheus renderer to decide whether to call
// SnapshotWithExemplars.
func (c *Collector) ExemplarsEnabled() bool {
	if c == nil {
		return false
	}
	return c.exemplarsEnabled.Load()
}

// --- RAG-vs-judge quality correlation (issue #1167) -------------------

// ObserveJudgeScore records a judge quality score partitioned by whether
// RAG context was injected. Called from the judge worker's score callback
// (wired in cmd/nexus). Only valid scores (1..5) are recorded; parse
// failures (score == 0 or out of range) are silently skipped so the
// correlation metrics reflect actual model quality, not judge errors.
// Safe for concurrent use — all updates are lock-free atomic operations.
func (c *Collector) ObserveJudgeScore(ragInjected bool, score int) {
	if c == nil {
		return
	}
	if score < 1 || score > 5 {
		return
	}
	if ragInjected {
		atomicAddFloat(&c.ragJudgeScoreSumBitsTrue, float64(score))
		c.ragJudgeScoreCountTrue.Add(1)
	} else {
		atomicAddFloat(&c.ragJudgeScoreSumBitsFalse, float64(score))
		c.ragJudgeScoreCountFalse.Add(1)
	}
}

// RAGJudgeScoreSum returns the cumulative judge score sum for the given
// injected label. Used by the Prometheus renderer (issue #1167).
func (c *Collector) RAGJudgeScoreSum(injected bool) float64 {
	if injected {
		return math.Float64frombits(c.ragJudgeScoreSumBitsTrue.Load())
	}
	return math.Float64frombits(c.ragJudgeScoreSumBitsFalse.Load())
}

// RAGJudgeScoreCount returns the cumulative judge score count for the
// given injected label. Used by the Prometheus renderer (issue #1167).
func (c *Collector) RAGJudgeScoreCount(injected bool) uint64 {
	if injected {
		return c.ragJudgeScoreCountTrue.Load()
	}
	return c.ragJudgeScoreCountFalse.Load()
}

// --- Frontier provider health metrics (issue #1158) --------------------

// IncFrontierProbe increments the probe counter for the given
// (provider, result) pair. result is "success" or "failure". Called
// from the frontier health poller after every probe via the probe
// callback wired in server.go.
func (c *Collector) IncFrontierProbe(provider, result string) {
	if provider == "" || result == "" {
		return
	}
	key := provider + "|" + result
	c.frontierHealthMu.Lock()
	defer c.frontierHealthMu.Unlock()
	if c.frontierProbeTotal == nil {
		c.frontierProbeTotal = make(map[string]*atomic.Uint64)
	}
	if c.frontierProbeTotal[key] == nil {
		c.frontierProbeTotal[key] = new(atomic.Uint64)
	}
	c.frontierProbeTotal[key].Add(1)
}

// IncFrontierCircuitOpen increments the circuit-open counter for the
// given provider. Called from the frontier health poller when a
// provider's circuit transitions from closed to open.
func (c *Collector) IncFrontierCircuitOpen(provider string) {
	if provider == "" {
		return
	}
	c.frontierHealthMu.Lock()
	defer c.frontierHealthMu.Unlock()
	if c.frontierCircuitOpenTotal == nil {
		c.frontierCircuitOpenTotal = make(map[string]*atomic.Uint64)
	}
	if c.frontierCircuitOpenTotal[provider] == nil {
		c.frontierCircuitOpenTotal[provider] = new(atomic.Uint64)
	}
	c.frontierCircuitOpenTotal[provider].Add(1)
}

// FrontierProbeTotals returns the cumulative probe counts keyed by
// "provider|result". Used by the Prometheus renderer (issue #1158).
func (c *Collector) FrontierProbeTotals() map[string]uint64 {
	c.frontierHealthMu.RLock()
	defer c.frontierHealthMu.RUnlock()
	out := make(map[string]uint64, len(c.frontierProbeTotal))
	for k, v := range c.frontierProbeTotal {
		out[k] = v.Load()
	}
	return out
}

// FrontierCircuitOpenTotals returns the cumulative circuit-open counts
// keyed by provider. Used by the Prometheus renderer (issue #1158).
func (c *Collector) FrontierCircuitOpenTotals() map[string]uint64 {
	c.frontierHealthMu.RLock()
	defer c.frontierHealthMu.RUnlock()
	out := make(map[string]uint64, len(c.frontierCircuitOpenTotal))
	for k, v := range c.frontierCircuitOpenTotal {
		out[k] = v.Load()
	}
	return out
}

// --- SLO error budget tracking (issue #1239) -----------------------------

// SLOTarget defines an SLO threshold for error budget computation.
type SLOTarget struct {
	Name         string  // SLO identifier: "availability", "local_latency_p99", "ttft_p95"
	Threshold    float64 // SLO threshold (e.g. 0.001 error rate, 2.0s latency, 0.5s TTFT)
	ErrorBudget  float64 // Allowed error fraction per window (e.g. 0.001 = 0.1%)
	CurrentValue float64 // Current observed value (error rate, latency, TTFT)
}

// SetSLOErrorBudget updates the error budget remaining for the named SLO.
// value is the remaining fraction in [0, 1], where 1 = full budget and
// 0 = exhausted. Values are clamped to [0, 1]. Safe for concurrent use.
// Called from the Prometheus scrape path to recompute budgets from the
// in-process percentile gauges.
func (c *Collector) SetSLOErrorBudget(sloName string, value float64) {
	if c == nil || sloName == "" {
		return
	}
	if value < 0 {
		value = 0
	} else if value > 1 {
		value = 1
	}
	c.sloBudgetMu.RLock()
	bits, ok := c.sloErrorBudgetBits[sloName]
	c.sloBudgetMu.RUnlock()
	if ok && bits != nil {
		bits.Store(math.Float64bits(value))
	}
}

// SLOErrorBudgetGauges returns the current error budget remaining for all
// tracked SLOs as gauge samples for the Prometheus renderer. Each SLO emits
// one sample labelled by slo name. Values are in [0, 1].
// Safe for a nil receiver — returns nil.
func (c *Collector) SLOErrorBudgetGauges() []GaugeSample {
	if c == nil {
		return nil
	}
	c.sloBudgetMu.RLock()
	defer c.sloBudgetMu.RUnlock()
	out := make([]GaugeSample, 0, len(c.sloErrorBudgetBits))
	for name, bits := range c.sloErrorBudgetBits {
		out = append(out, GaugeSample{
			Name:   "nexus_slo_error_budget_remaining",
			Labels: map[string]string{"slo": name},
			Value:  math.Float64frombits(bits.Load()),
		})
	}
	return out
}

// --- Pipeline stage latency breakdown (issue #300) -------------------
//
// ObservePipelineStage records per-stage timing breakdown from the chat
// handler (issue #300). Each field is milliseconds spent in that stage;
// 0 when the stage was skipped. Safe for concurrent use.
//
// Also records the SLM confidence histogram (issue #425) when
// SLMConfidence > 0 and SLMTaskType is non-empty.
func (c *Collector) ObservePipelineStage(e PipelineStageEvent) {
	ex := Exemplar{TraceID: e.TraceID, SpanID: e.SpanID}
	if e.RAGRetrievalMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.RAGRetrievalMs)
			c.stageRAG.ObserveWithExemplar(float64(e.RAGRetrievalMs), ex)
		} else {
			c.stageRAG.Observe(float64(e.RAGRetrievalMs))
		}
	}
	if e.PromptEngineeringMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.PromptEngineeringMs)
			c.stagePromptEng.ObserveWithExemplar(float64(e.PromptEngineeringMs), ex)
		} else {
			c.stagePromptEng.Observe(float64(e.PromptEngineeringMs))
		}
	}
	if e.TOONCompressionMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.TOONCompressionMs)
			c.stageTOON.ObserveWithExemplar(float64(e.TOONCompressionMs), ex)
		} else {
			c.stageTOON.Observe(float64(e.TOONCompressionMs))
		}
	}
	if e.SLMRoutingMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.SLMRoutingMs)
			c.stageSLM.ObserveWithExemplar(float64(e.SLMRoutingMs), ex)
		} else {
			c.stageSLM.Observe(float64(e.SLMRoutingMs))
		}
	}
	if e.UpstreamFirstByteMs > 0 {
		if e.TraceID != "" {
			ex.Value = float64(e.UpstreamFirstByteMs)
			c.stageUpstream.ObserveWithExemplar(float64(e.UpstreamFirstByteMs), ex)
		} else {
			c.stageUpstream.Observe(float64(e.UpstreamFirstByteMs))
		}
	}
	// SLM confidence histogram (issue #425).
	if e.SLMConfidence > 0 && e.SLMTaskType != "" {
		c.ObserveSLMConfidence(e.SLMTaskType, e.SLMConfidence)
	}
}

// PipelineStageEvent mirrors handlers.PipelineStageEvent so the
// collector stays independent of the handlers package.
type PipelineStageEvent struct {
	RAGRetrievalMs      int64
	PromptEngineeringMs int64
	TOONCompressionMs   int64
	SLMRoutingMs        int64
	UpstreamFirstByteMs int64

	// SLM confidence for histogram recording (issue #425).
	SLMConfidence float64
	SLMTaskType   string

	// Trace context for exemplar attachment (issue #1171).
	TraceID string
	SpanID  string
}

// Handler returns an http.Handler that renders stage latency histograms
// in Prometheus text format. Used by the /metrics endpoint to expose
// nexus_pipeline_stage_latency_ms (issue #300).
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		RenderPrometheus(w, c)
	})
}

// --- SLM confidence histogram (issue #425) ------------------------------
//
// ObserveSLMConfidence records a confidence observation for the given task
// category. It is called from Submit (which is invoked in the handler's
// request goroutine after the planner returns a Decision with a
// confidence value). The histogram itself is pre-allocated per category
// in NewCollector so this method only needs a read lock to find it;
// Histogram.Observe is lock-free.
func (c *Collector) ObserveSLMConfidence(category string, confidence float64) {
	c.slmConfidenceMu.RLock()
	h, ok := c.slmConfidenceHistograms[category]
	c.slmConfidenceMu.RUnlock()
	if ok && h != nil {
		h.Observe(confidence)
	}
}

// SLMConfidenceHistograms returns the per-category SLM confidence
// histograms for rendering. Returns nil if the collector is not yet
// initialized.
func (c *Collector) SLMConfidenceHistograms() map[string]*Histogram {
	c.slmConfidenceMu.RLock()
	defer c.slmConfidenceMu.RUnlock()
	return c.slmConfidenceHistograms
}

// --- RAG similarity histogram (issue #447) ------------------------------
//
// ObserveRAGSimilarity records one similarity observation for the
// given (path, outcome, threshold) tuple (issue #447, #671).
// Called from the RAG observer closure in main.go when
// handlers.RAGEvent carries a non-empty IndexPath.
//
// The outcome is "hit" when the retrieval returned a snippet above the
// configured threshold and "miss" otherwise; both observations land in
// the same bucket layout so the bucket counts can be directly compared.
// Score values outside [0, 1] are clamped to the [0, 1] range so a
// buggy embedder cannot push an observation into the +Inf bucket
// spuriously — the cosine contract is that similarity is bounded by 1.
//
// path values outside the fixed set ("hnsw", "brute_force") are
// silently dropped rather than bucketed under a third label, to
// preserve the bounded-cardinality contract documented for
// nexus_rag_similarity_histogram.
//
// effectiveThreshold is the similarity floor that was applied for this
// retrieval (issue #671). When per-directory overrides are configured,
// this may differ from the global NEXUS_RAG_THRESHOLD. Operators use
// the threshold label to tune per-domain thresholds.
func (c *Collector) ObserveRAGSimilarity(path, outcome string, score, effectiveThreshold float64) {
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	key := ragSimilarityKey(path, outcome, effectiveThreshold)
	c.ragSimilarityMu.RLock()
	h, ok := c.ragSimilarityHistograms[key]
	c.ragSimilarityMu.RUnlock()
	if !ok || h == nil {
		// Lazily create histogram for this (path, outcome, threshold) combination
		// (issue #671). This allows per-directory threshold tuning visibility
		// without pre-allocating histograms for every possible threshold.
		c.ragSimilarityMu.Lock()
		// Double-check after acquiring write lock
		if h, ok = c.ragSimilarityHistograms[key]; !ok || h == nil {
			h = NewHistogram(RAGSimilarityBuckets)
			c.ragSimilarityHistograms[key] = h
		}
		c.ragSimilarityMu.Unlock()
	}
	h.Observe(score)
}

// RAGSimilarityHistograms returns the per-(path, outcome, threshold) RAG
// similarity histograms for rendering. The map is keyed by
// "path|outcome|threshold" (e.g. "hnsw|hit|0.55"); callers iterate the keys to
// emit nexus_rag_similarity_histogram_bucket lines (issue #671).
//
// Returns nil when the collector is not yet initialised (e.g. when a
// nil receiver is passed to RenderPrometheus). Safe to call from
// multiple goroutines — returns a snapshot reference to the internal
// map, which is never mutated after NewCollector returns.
func (c *Collector) RAGSimilarityHistograms() map[string]*Histogram {
	c.ragSimilarityMu.RLock()
	defer c.ragSimilarityMu.RUnlock()
	return c.ragSimilarityHistograms
}

// --- Rate-limit bucket utilization histogram (issue #746) ---------------
//
// ObserveRateLimitUtilization records one utilization observation for the
// given bucket ID. The utilization value is clamped to [0, 1] so a
// buggy caller cannot push an observation past the +Inf bucket
// spuriously. Histograms are created lazily per bucket ID.
func (c *Collector) ObserveRateLimitUtilization(bucketID string, utilizationPct float64) {
	if bucketID == "" || c == nil {
		return
	}
	if utilizationPct < 0 {
		utilizationPct = 0
	} else if utilizationPct > 1 {
		utilizationPct = 1
	}
	c.rateLimitUtilizationMu.RLock()
	h, ok := c.rateLimitUtilizationHistograms[bucketID]
	c.rateLimitUtilizationMu.RUnlock()
	if !ok || h == nil {
		c.rateLimitUtilizationMu.Lock()
		if h, ok = c.rateLimitUtilizationHistograms[bucketID]; !ok || h == nil {
			h = NewHistogram(RateLimitUtilizationBuckets)
			if c.rateLimitUtilizationHistograms == nil {
				c.rateLimitUtilizationHistograms = make(map[string]*Histogram)
			}
			c.rateLimitUtilizationHistograms[bucketID] = h
		}
		c.rateLimitUtilizationMu.Unlock()
	}
	h.Observe(utilizationPct)
}

// RateLimitUtilizationHistograms returns the per-bucket-ID utilization
// histograms for rendering. The map is keyed by bucket ID (hashed IP).
//
// Returns nil when the collector is not yet initialized. Safe to call
// from multiple goroutines.
func (c *Collector) RateLimitUtilizationHistograms() map[string]*Histogram {
	if c == nil {
		return nil
	}
	c.rateLimitUtilizationMu.RLock()
	defer c.rateLimitUtilizationMu.RUnlock()
	return c.rateLimitUtilizationHistograms
}

// Histogram}

// --- Per-route latency percentile ring buffers (issue #774) -----------
//
// latencyPercentileBuffer stores a sliding window of latency samples and
// maintains running p50/p95/p99 percentile estimates. Updated on each
// request completion so percentiles are always current at scrape time.
// The ring buffer has fixed capacity; oldest samples are evicted.
//
// Using a ring buffer (not histogram interpolation) gives exact
// percentile values from actual samples — operators can set precise
// SLO alerts (e.g. "p95 < 2s") without client-side queries.
//
// Capacity of 1000 samples gives ~3–15 min of history depending on
// request rate, sufficient for stable p95/p99 estimates.
type latencyPercentileBuffer struct {
	mu       sync.Mutex
	samples  []float64 // latency in milliseconds, oldest first
	capacity int
	p50Bits  atomic.Uint64 // IEEE-754 bits of p50 value
	p95Bits  atomic.Uint64
	p99Bits  atomic.Uint64
}

const defaultLatencyBufferCapacity = 1000

func newLatencyPercentileBuffer(capacity int) *latencyPercentileBuffer {
	if capacity <= 0 {
		capacity = defaultLatencyBufferCapacity
	}
	return &latencyPercentileBuffer{
		samples:  make([]float64, 0, capacity),
		capacity: capacity,
	}
}

func (b *latencyPercentileBuffer) Observe(latencyMs float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.samples) < b.capacity {
		b.samples = append(b.samples, latencyMs)
	} else {
		// Ring buffer: overwrite oldest, keep newest
		copy(b.samples, b.samples[1:])
		b.samples[b.capacity-1] = latencyMs
	}

	b.recomputePercentilesLocked()
}

func (b *latencyPercentileBuffer) recomputePercentilesLocked() {
	n := len(b.samples)
	if n == 0 {
		return
	}
	// Sort ascending for percentile computation
	sorted := make([]float64, n)
	copy(sorted, b.samples)
	sort.Float64s(sorted)

	b.p50Bits.Store(math.Float64bits(percentile(sorted, 0.50)))
	b.p95Bits.Store(math.Float64bits(percentile(sorted, 0.95)))
	b.p99Bits.Store(math.Float64bits(percentile(sorted, 0.99)))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	// Linear interpolation between nearest ranks
	idx := p * float64(len(sorted)-1)
	lower := int(idx)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// Perc returns the current p50/p95/p99 readings. Values are 0 when
// no samples have been recorded yet.
func (b *latencyPercentileBuffer) Perc() (p50, p95, p99 float64) {
	return math.Float64frombits(b.p50Bits.Load()),
		math.Float64frombits(b.p95Bits.Load()),
		math.Float64frombits(b.p99Bits.Load())
}

// Count returns the number of samples currently in the buffer.
func (b *latencyPercentileBuffer) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.samples)
}

// Exemplar holds the most recent trace context for a single histogram
// bucket (issue #1171). Prometheus exemplars let operators click a
// histogram outlier in Grafana and jump directly to the trace that
// produced it. Each bucket stores one slot — the latest observation —
// because Prometheus scrapers only consume the most recent exemplar.
type Exemplar struct {
	TraceID string  // 32-hex-char W3C trace ID
	SpanID  string  // 16-hex-char W3C span ID
	Value   float64 // the observed value that landed in this bucket
}

// Histogram is a fixed-bucket cumulative histogram. Buckets are
// pre-allocated at construction; Observe performs a single linear scan
// over the finite upper bounds (at most one atomic increment) plus the
// running sum/count, so it is allocation-free on the hot path.
//
// Per-bucket counts are stored non-cumulatively; the cumulative counts
// required by the Prometheus exposition format are derived at render
// time (Snapshot). This keeps Observe to a single increment regardless
// of bucket count.
//
// Exemplars (issue #1171): the exemplars slice holds one slot per
// bucket (including +Inf), pre-allocated at construction. Each slot
// stores the most recent trace context from an ObserveWithExemplar call.
// exemplarMu guards writes during observation and reads at snapshot time;
// the mutex is only held for a struct copy, never an allocation.
type Histogram struct {
	upperBounds []float64       // finite upper bounds, ascending
	counts      []atomic.Uint64 // len == len(upperBounds)+1; last is the +Inf overflow bucket
	sumBits     atomic.Uint64   // float64 bits (math.Float64bits)
	count       atomic.Uint64   // total observations
	exemplarMu  sync.Mutex      // guards exemplars slice
	exemplars   []Exemplar      // len == len(upperBounds)+1; one slot per bucket
}

// NewHistogram constructs a Histogram whose finite buckets are bounded
// by upperBounds (ascending). A trailing +Inf bucket is implicit.
func NewHistogram(upperBounds []float64) *Histogram {
	return &Histogram{
		upperBounds: upperBounds,
		counts:      make([]atomic.Uint64, len(upperBounds)+1),
		exemplars:   make([]Exemplar, len(upperBounds)+1),
	}
}

// Observe records a single observation. The value lands in the first
// bucket whose upper bound is >= v, or in the trailing +Inf bucket when
// v exceeds every finite bound. Observe is safe for concurrent use.
func (h *Histogram) Observe(v float64) {
	idx := h.bucketIndex(v)
	h.counts[idx].Add(1)
	h.count.Add(1)
	atomicAddFloat(&h.sumBits, v)
}

// ObserveWithExemplar records a single observation and stores the trace
// context as an exemplar on the bucket the value landed in (issue #1171).
// When ex.TraceID is empty, no exemplar is stored and the call is
// equivalent to Observe. Safe for concurrent use.
func (h *Histogram) ObserveWithExemplar(v float64, ex Exemplar) {
	idx := h.bucketIndex(v)
	h.counts[idx].Add(1)
	h.count.Add(1)
	atomicAddFloat(&h.sumBits, v)
	if ex.TraceID != "" {
		h.exemplarMu.Lock()
		h.exemplars[idx] = ex
		h.exemplarMu.Unlock()
	}
}

// bucketIndex returns the index into counts/exemplars for value v.
func (h *Histogram) bucketIndex(v float64) int {
	for i, ub := range h.upperBounds {
		if v <= ub {
			return i
		}
	}
	return len(h.upperBounds)
}

// UpperBounds returns the finite bucket upper bounds. The returned
// slice aliases the histogram's internal storage; callers must not
// mutate it.
func (h *Histogram) UpperBounds() []float64 { return h.upperBounds }

// Snapshot returns the cumulative bucket counts, the finite upper
// bounds, the total sum, and the total observation count for rendering.
// The cumulative slice is freshly allocated so the caller never races a
// concurrent Observe.
//
// cumulative[i] holds the count of observations <= upperBounds[i]; the
// final entry (index len(upperBounds)) is the +Inf bucket and equals
// the total observation count.
func (h *Histogram) Snapshot() (cumulative []uint64, upperBounds []float64, sum float64, count uint64) {
	cumulative = make([]uint64, len(h.upperBounds)+1)
	var running uint64
	for i := range h.upperBounds {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	running += h.counts[len(h.upperBounds)].Load()
	cumulative[len(h.upperBounds)] = running
	upperBounds = h.upperBounds
	sum = math.Float64frombits(h.sumBits.Load())
	count = h.count.Load()
	return
}

// SnapshotWithExemplars is like Snapshot but also returns a copy of the
// per-bucket exemplar slots (issue #1171). The exemplars slice has the
// same length as cumulative (one slot per bucket, including +Inf).
// Buckets with no stored exemplar have a zero-value Exemplar (empty
// TraceID); the renderer checks TraceID to decide whether to emit the
// exemplar suffix.
func (h *Histogram) SnapshotWithExemplars() (cumulative []uint64, upperBounds []float64, sum float64, count uint64, exemplars []Exemplar) {
	cumulative, upperBounds, sum, count = h.Snapshot()
	h.exemplarMu.Lock()
	exemplars = make([]Exemplar, len(h.exemplars))
	copy(exemplars, h.exemplars)
	h.exemplarMu.Unlock()
	return
}

// atomicAddFloat adds delta to the float64 whose IEEE-754 bits live in
// addr, using a compare-and-swap loop so the hot path stays lock-free.
// Contention is essentially nil in practice (one add per request), so
// the retry loop virtually never spins.
func atomicAddFloat(addr *atomic.Uint64, delta float64) {
	for {
		old := addr.Load()
		newVal := math.Float64frombits(old) + delta
		if addr.CompareAndSwap(old, math.Float64bits(newVal)) {
			return
		}
	}
}
