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
	"sync/atomic"
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

	errorsTotal            atomic.Uint64
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

	latency *Histogram
	ttft    *Histogram
}

// NewCollector constructs a Collector with the default latency and TTFT
// histograms. The returned collector is ready to receive Submit calls
// and RenderPrometheus scrapes from any goroutine.
func NewCollector() *Collector {
	return &Collector{
		latency: NewHistogram(DefaultBuckets),
		ttft:    NewHistogram(DefaultBuckets),
	}
}

// Submit records one ObservabilityEvent. Called exactly once per
// proxied request from the chat handler's request goroutine. Submit is
// a sequence of atomic increments — it never blocks, never allocates,
// and is safe to call from many goroutines concurrently.
func (c *Collector) Submit(e ObservabilityEvent) {
	switch e.Route {
	case "local":
		c.requestsLocal.Add(1)
	case "fusion":
		c.requestsFusion.Add(1)
	default: // "frontier" and any unrecognised route count as frontier
		c.requestsFrontier.Add(1)
	}
	if e.Error != "" {
		c.errorsTotal.Add(1)
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
	if e.TotalLatencyMs > 0 {
		c.latency.Observe(float64(e.TotalLatencyMs))
	}
	if e.TTFTMs > 0 {
		c.ttft.Observe(float64(e.TTFTMs))
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

// Latency returns the request-latency histogram.
func (c *Collector) Latency() *Histogram { return c.latency }

// TTFT returns the time-to-first-token histogram.
func (c *Collector) TTFT() *Histogram { return c.ttft }

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
