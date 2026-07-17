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
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBuckets are the histogram bucket upper bounds (in
// milliseconds) used for both request latency and TTFT. They span
// sub-frame (5 ms) through slow-fusion (30 s); the implicit +Inf
// bucket catches anything beyond. Tuned for the coding-agent workload
// where local Ollama responses land in the 100 ms–2.5 s band and
// frontier streams occasionally exceed 10 s.
var DefaultBuckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

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
	Degraded       bool // local arm was skipped because Ollama was unhealthy

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

	ragHitsTotal           atomic.Uint64
	ragMissesTotal         atomic.Uint64
	toonCompressedTotal    atomic.Uint64
	degradedTotal          atomic.Uint64
	inputTokensTotal       atomic.Uint64
	outputTokensTotal      atomic.Uint64
	toonSavingsTokensTotal atomic.Uint64

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
	// Auth counters are labelled by outcome via three separate atomics
	// rather than a label-keyed map. The label set is fixed at three
	// values, so three atomics is the simplest lock-free layout and
	// keeps the hot path to a single add per request.
	authAccepted        atomic.Uint64
	authRejectedInvalid atomic.Uint64
	authRejectedMissing atomic.Uint64

	// Auth rate limit counter (issue #296). Bumped when the auth
	// brute-force limiter rejects a client with 429.
	authRateLimitRejected atomic.Uint64

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

	// TLS counters are bumped from main.go via http.Server.ConnState.
	// Accepted fires on http.StateTLSHandshakeComplete; Rejected
	// fires when a connection closes before reaching that state.
	tlsConnectionsAccepted atomic.Uint64
	tlsConnectionsRejected atomic.Uint64

	// --- RAG watcher instrumentation (issue #367) ------------------
	//
	// RAG watcher scan outcome metrics. Updated after each scan by
	// rag.Watcher calling OnScanComplete.
	ragWatcherFilesIndexed   atomic.Uint64 // files processed in last scan
	ragWatcherLastScanTime   atomic.Int64  // Unix timestamp (seconds) of last scan
	ragWatcherScanDurationMs atomic.Uint64 // last scan duration in milliseconds
	ragWatcherIndexingErrors atomic.Uint64 // cumulative indexing errors

	// --- RAG embedder instrumentation (issue #411) ----------------
	//
	// ragEmbedFailures counts per-request Ollama embedding failures
	// so operators can build alerts for "breaker about to trip".
	ragEmbedFailures atomic.Uint64

	// --- Circuit breaker instrumentation (issue #304, #411) ---------------
	//
	// Tracks the state of each named circuit breaker (ollama, rag).
	// State values: 0=closed, 1=half_open, 2=open.
	// Protected by cbMu; read via atomic for hot path.
	cbMu    sync.RWMutex
	cbState map[string]*circuitBreakerState

	// --- VRAM gauge instrumentation (issue #394) ------------------
	//
	// vramGaugeFunc is the callback that returns per-GPU free VRAM
	// readings (gpu_id -> free bytes) at scrape time. Set by main.go
	// from the probe manager so the collector never imports internal/probe.
	// Nil means VRAM data is not available.
	vramGaugeFunc atomic.Value // func() map[string]int64

	// bytesPerSlot is the VRAM bytes assumed per local-route concurrency
	// slot, used to compute nexus_vram_slots_available. Set alongside
	// vramGaugeFunc via SetVramGaugeFunc.
	bytesPerSlot atomic.Int64
}

// circuitBreakerState holds the atomic state for one named circuit.
type circuitBreakerState struct {
	state       atomic.Int32 // 0=closed, 1=half_open, 2=open
	tripCount   atomic.Uint64
	failures    atomic.Uint64
	lastFailure atomic.Int64 // Unix timestamp (seconds) of last failure
	lastTrip    atomic.Int64 // Unix timestamp (seconds) of last trip to open
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
	return &Collector{
		latencyLocal:    NewHistogram(DefaultBuckets),
		latencyFrontier: NewHistogram(DefaultBuckets),
		latencyFusion:   NewHistogram(DefaultBuckets),
		ttftLocal:       NewHistogram(DefaultBuckets),
		ttftFrontier:    NewHistogram(DefaultBuckets),
		ttftFusion:      NewHistogram(DefaultBuckets),
		stageRAG:        NewHistogram(DefaultBuckets),
		stagePromptEng:  NewHistogram(DefaultBuckets),
		stageTOON:       NewHistogram(DefaultBuckets),
		stageSLM:        NewHistogram(DefaultBuckets),
		stageUpstream:   NewHistogram(DefaultBuckets),
	}
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
		latencyHist.Observe(float64(e.TotalLatencyMs))
	}
	if e.TTFTMs > 0 && ttftHist != nil {
		ttftHist.Observe(float64(e.TTFTMs))
	}
	// Per-stage pipeline latency histograms (issue #300).
	if e.RAGRetrievalMs > 0 {
		c.stageRAG.Observe(float64(e.RAGRetrievalMs))
	}
	if e.PromptEngineeringMs > 0 {
		c.stagePromptEng.Observe(float64(e.PromptEngineeringMs))
	}
	if e.TOONCompressionMs > 0 {
		c.stageTOON.Observe(float64(e.TOONCompressionMs))
	}
	if e.SLMRoutingMs > 0 {
		c.stageSLM.Observe(float64(e.SLMRoutingMs))
	}
	if e.UpstreamFirstByteMs > 0 {
		c.stageUpstream.Observe(float64(e.UpstreamFirstByteMs))
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

// --- Middleware instrumentation helpers (issue #70) ----------------------
//
// Each helper bumps exactly one atomic counter so the middleware hot
// path stays a single atomic add. The middleware packages own the
// decision logic (when a request is "accepted" vs "rejected_invalid"
// etc.); the collector only stores the resulting counts.

// IncAuthAccepted records one accepted authentication request.
func (c *Collector) IncAuthAccepted() { c.authAccepted.Add(1) }

// IncAuthRejectedInvalid records a request that presented a
// credential but it did not match any configured key.
func (c *Collector) IncAuthRejectedInvalid() { c.authRejectedInvalid.Add(1) }

// IncAuthRejectedMissing records a request that presented no
// credential at all (no Authorization / X-API-Key header).
func (c *Collector) IncAuthRejectedMissing() { c.authRejectedMissing.Add(1) }

// IncAuthRateLimitRejected bumps the auth-rate-limit counter when
// the auth brute-force limiter rejects a client (issue #296).
func (c *Collector) IncAuthRateLimitRejected() { c.authRateLimitRejected.Add(1) }

// AuthAuthenticatedClients returns the cumulative count of accepted
// authentications. The /metrics renderer exposes it under the gauge
// name nexus_auth_authenticated_clients so operators can chart a
// running total of successful auth events without scraping logs.
//
// (The name carries "clients" rather than "events" because the issue
// spec calls for a gauge by that name; semantically this is a
// monotonic counter rendered as a gauge family so a single PromQL
// query shows the long-running trend.)
func (c *Collector) AuthAuthenticatedClients() uint64 { return c.authAccepted.Load() }

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

// IncTLSAccepted bumps the accepted TLS-handshake counter. Wired
// from main.go via http.Server.ConnState on
// http.StateTLSHandshakeComplete.
func (c *Collector) IncTLSAccepted() { c.tlsConnectionsAccepted.Add(1) }

// IncTLSRejected bumps the rejected TLS-handshake counter. Wired
// from main.go via http.Server.ConnState for connections that
// close before reaching http.StateTLSHandshakeComplete.
func (c *Collector) IncTLSRejected() { c.tlsConnectionsRejected.Add(1) }

// IncRAGEmbedFailure increments the per-request Ollama embedding failure
// counter (issue #411). Called from the chat handler after a ragErr != nil
// when the RAG embedder returns an error, giving operators an early-warning
// signal before the circuit breaker trips.
func (c *Collector) IncRAGEmbedFailure() { c.ragEmbedFailures.Add(1) }

// --- RAG watcher instrumentation (issue #367) ----------------------
//
// OnScanComplete records the outcome of one watcher scan. Called by
// rag.Watcher after each scanOnce; implements rag.WatcherMetrics so
// main.go can wire it with watcher.SetMetrics(c).
func (c *Collector) OnScanComplete(filesIndexed int, durationMs int64, indexingErrors int) {
	c.ragWatcherFilesIndexed.Store(uint64(filesIndexed))
	c.ragWatcherLastScanTime.Store(time.Now().Unix())
	c.ragWatcherScanDurationMs.Store(uint64(durationMs))
	if indexingErrors > 0 {
		c.ragWatcherIndexingErrors.Add(uint64(indexingErrors))
	}
}

// RAGWatcherGauges returns the live RAG watcher readings as gauge samples
// for the Prometheus renderer.
func (c *Collector) RAGWatcherGauges() []GaugeSample {
	return []GaugeSample{
		{Name: "nexus_rag_watcher_files_indexed", Value: float64(c.ragWatcherFilesIndexed.Load())},
		{Name: "nexus_rag_watcher_last_scan_time", Value: float64(c.ragWatcherLastScanTime.Load())},
		{Name: "nexus_rag_watcher_scan_duration_ms", Value: float64(c.ragWatcherScanDurationMs.Load())},
		{Name: "nexus_rag_watcher_indexing_errors", Value: float64(c.ragWatcherIndexingErrors.Load())},
	}
}

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

// RecordCircuitTrip records a circuit breaker trip event (issue #411).
// Called via the OnBreakerTrip callback wired from the Ollama health
// breaker in main.go so the collector can emit nexus_circuit_breaker_trip_total.
func (c *Collector) RecordCircuitTrip(circuit string) {
	if circuit == "" {
		return
	}
	cb := c.getOrCreateCircuit(circuit)
	cb.tripCount.Add(1)
	cb.lastTrip.Store(time.Now().Unix())
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
// emits five samples: state (0=closed, 1=half_open, 2=open),
// trip_total, failures_total, last_failure_seconds, and last_trip_seconds
// (issue #411).
func (c *Collector) CircuitBreakerGauges() []GaugeSample {
	var out []GaugeSample
	c.cbMu.RLock()
	defer c.cbMu.RUnlock()
	for name, cb := range c.cbState {
		lastFail := cb.lastFailure.Load()
		lastTrip := cb.lastTrip.Load()
		labels := map[string]string{"circuit": name}
		out = append(out,
			GaugeSample{Name: "nexus_circuit_breaker_state", Labels: labels, Value: float64(cb.state.Load())},
			GaugeSample{Name: "nexus_circuit_breaker_trip_total", Labels: labels, Value: float64(cb.tripCount.Load())},
			GaugeSample{Name: "nexus_circuit_breaker_failures_total", Labels: labels, Value: float64(cb.failures.Load())},
			GaugeSample{Name: "nexus_circuit_breaker_last_failure_seconds", Labels: labels, Value: float64(lastFail)},
			GaugeSample{Name: "nexus_circuit_breaker_last_trip_seconds", Labels: labels, Value: float64(lastTrip)},
		)
	}
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

// --- VRAM gauge instrumentation (issue #394) ------------------------
//
// SetVramGaugeFunc configures the callback that supplies per-GPU free VRAM
// readings at scrape time. The bytesPerSlot parameter is the VRAM bytes
// assumed per local-route concurrency slot; it is used to compute
// nexus_vram_slots_available. Called once from main.go at startup.
func (c *Collector) SetVramGaugeFunc(fn func() map[string]int64, bytesPerSlot int64) {
	c.vramGaugeFunc.Store(fn)
	c.bytesPerSlot.Store(bytesPerSlot)
}

// VRAMGauges returns the live VRAM readings as gauge samples for the
// Prometheus renderer. Each GPU emits two samples:
//   - nexus_vram_free_bytes{gpu_id="cardN"} — free VRAM in bytes
//   - nexus_vram_slots_available{gpu_id="cardN"} — derived slot count
//
// Slots are computed as freeVRAM / bytesPerSlot, floored at 0.
// When no VRAM function has been configured, returns nil (no samples emitted).
func (c *Collector) VRAMGauges() []GaugeSample {
	fnRaw := c.vramGaugeFunc.Load()
	if fnRaw == nil {
		return nil
	}
	fn := fnRaw.(func() map[string]int64)
	perGPU := fn()
	if len(perGPU) == 0 {
		return nil
	}
	bps := c.bytesPerSlot.Load()
	var out []GaugeSample
	for gpuID, freeBytes := range perGPU {
		labels := map[string]string{"gpu_id": gpuID}
		out = append(out, GaugeSample{Name: "nexus_vram_free_bytes", Labels: labels, Value: float64(freeBytes)})
		if bps > 0 {
			slots := freeBytes / bps
			out = append(out, GaugeSample{Name: "nexus_vram_slots_available", Labels: labels, Value: float64(slots)})
		}
	}
	return out
}

// --- Pipeline stage latency breakdown (issue #300) -------------------
//
// ObservePipelineStage records per-stage timing breakdown from the chat
// handler (issue #300). Each field is milliseconds spent in that stage;
// 0 when the stage was skipped. Safe for concurrent use.
func (c *Collector) ObservePipelineStage(e PipelineStageEvent) {
	if e.RAGRetrievalMs > 0 {
		c.stageRAG.Observe(float64(e.RAGRetrievalMs))
	}
	if e.PromptEngineeringMs > 0 {
		c.stagePromptEng.Observe(float64(e.PromptEngineeringMs))
	}
	if e.TOONCompressionMs > 0 {
		c.stageTOON.Observe(float64(e.TOONCompressionMs))
	}
	if e.SLMRoutingMs > 0 {
		c.stageSLM.Observe(float64(e.SLMRoutingMs))
	}
	if e.UpstreamFirstByteMs > 0 {
		c.stageUpstream.Observe(float64(e.UpstreamFirstByteMs))
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

// Histogram}

// Histogram is a fixed-bucket cumulative histogram. Buckets are
// pre-allocated at construction; Observe performs a single linear scan
// over the finite upper bounds (at most one atomic increment) plus the
// running sum/count, so it is allocation-free on the hot path.
//
// Per-bucket counts are stored non-cumulatively; the cumulative counts
// required by the Prometheus exposition format are derived at render
// time (Snapshot). This keeps Observe to a single increment regardless
// of bucket count.
type Histogram struct {
	upperBounds []float64       // finite upper bounds, ascending
	counts      []atomic.Uint64 // len == len(upperBounds)+1; last is the +Inf overflow bucket
	sumBits     atomic.Uint64   // float64 bits (math.Float64bits)
	count       atomic.Uint64   // total observations
}

// NewHistogram constructs a Histogram whose finite buckets are bounded
// by upperBounds (ascending). A trailing +Inf bucket is implicit.
func NewHistogram(upperBounds []float64) *Histogram {
	return &Histogram{
		upperBounds: upperBounds,
		counts:      make([]atomic.Uint64, len(upperBounds)+1),
	}
}

// Observe records a single observation. The value lands in the first
// bucket whose upper bound is >= v, or in the trailing +Inf bucket when
// v exceeds every finite bound. Observe is safe for concurrent use.
func (h *Histogram) Observe(v float64) {
	for i, ub := range h.upperBounds {
		if v <= ub {
			h.counts[i].Add(1)
			h.count.Add(1)
			atomicAddFloat(&h.sumBits, v)
			return
		}
	}
	h.counts[len(h.upperBounds)].Add(1)
	h.count.Add(1)
	atomicAddFloat(&h.sumBits, v)
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
