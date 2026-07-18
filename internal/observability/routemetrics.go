// Package observability exposes in-process counters for routing
// decisions (issue #74). The proxy is stdlib-only by design, so this
// package implements a tiny Prometheus-text-format exposition rather
// than pulling in the official client library. The counters are
// updated synchronously from the chat handler's request goroutine —
// each Observe call is a handful of atomic increments, so the hot
// path pays negligible overhead.
//
// The exposition endpoint is wired in cmd/nexus/main.go at /metrics
// and is consumable by any Prometheus-compatible scraper (Prometheus,
// Grafana Agent, VictoriaMetrics, ...). Operators who do not scrape
// the endpoint pay only the cost of the atomic counters — no
// background goroutine, no disk I/O.
package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anchapin/nexus-proxy/internal/ioutils"
)

// Confidence buckets. The SLM emits a float64 in [0,1]; we collapse
// it into three ordinal buckets so the Prometheus label cardinality
// stays bounded (route * source * bucket = ~36 series maximum). The
// thresholds match the router's own confidence-floor/ceiling defaults
// (NeutralConfidence = 0.5) so "medium" aligns with the neutral band.
const (
	confidenceLow    = 0.4
	confidenceMedium = 0.7

	bucketLow    = "low"
	bucketMedium = "medium"
	bucketHigh   = "high"
	bucketNone   = "none" // guardrail / DSL / non-SLM sources
)

// responseTruncated is a package-level counter for truncated upstream
// responses (issue #365). It is set by SetTruncationCounter and
// incremented by IncrementTruncationCounter. When nil, increments are
// no-ops.
var responseTruncated *uint64

// SetTruncationCounter configures the package-level truncation counter.
// Called once at startup from main.go.
func SetTruncationCounter(p *uint64) {
	responseTruncated = p
}

// IncrementTruncationCounter atomically increments the truncation counter.
// Safe for concurrent use. Nil counter is a no-op.
func IncrementTruncationCounter() {
	if responseTruncated != nil {
		atomic.AddUint64(responseTruncated, 1)
	}
}

// BucketConfidence collapses a raw SLM confidence into a short,
// bounded-cardinality label. Non-SLM sources (guardrail, DSL) carry
// no meaningful confidence, so callers should pass bucketNone by
// convention — but this helper is exported for tests and downstream
// consumers that want consistent bucketing.
func BucketConfidence(c float64) string {
	switch {
	case c <= 0:
		return bucketNone
	case c < confidenceLow:
		return bucketLow
	case c < confidenceMedium:
		return bucketMedium
	default:
		return bucketHigh
	}
}

// counterKey is the composite label set for a single counter series.
// Keeping it as a struct (not a joined string) avoids string
// allocation on the hot path — the caller passes already-owned
// strings from the Decision.
type counterKey struct {
	route      string
	source     string
	confBucket string
	taskType   string
}

// RouteCounters is a concurrency-safe collection of route-decision
// counters. The zero value is NOT safe to use directly because Go
// zero-value maps are nil — always construct via NewRouteCounters.
// Observe can be called from many goroutines; each call is a map
// lookup (guarded by a mutex) followed by an atomic increment, so
// contention is minimal.
//
// Four metric families are exposed:
//   - nexus_route_decisions_total{route,source}
//   - nexus_slm_decisions_total{route,confidence_bucket,task_type}
//   - nexus_slm_low_confidence_escalations_total{task_type}
//   - nexus_slm_cache_hits_total{kind}
//   - nexus_slm_cache_misses_total
//
// A fifth family (issue #119) records requests the proxy rejected
// before they reached an upstream:
//   - nexus_requests_rejected_total{reason}
//
// A fifth family (issue #187) records fusion arbiter outcomes:
//   - nexus_fusion_arbiter_total{outcome}
//     where outcome is "skipped" (agreement reached, arbiter not invoked)
//     or "invoked" (disagreement, arbiter was called).
//
// A sixth family (issue #186) records RAG retrieval outcomes:
//   - nexus_rag_retrieval_total{hit}
//
// A seventh family (issue #232) records fusion arbiter cache hits/misses:
//   - nexus_fusion_arbiter_cache_total{hit}
//
// An eighth family (issue #449) records SLM decision cache evictions:
//   - nexus_slm_cache_evictions_total{reason}
//     where reason is "ttl" (entries removed because their TTL elapsed)
//     or "lru" (entries removed to make room at capacity). The label
//     set is bounded so cardinality stays at 2 series maximum.
//
// The reason label values are short, bounded strings (method,
// body_too_large, bad_request, rate_limit, ...) defined as constants
// in internal/handlers so the chat handler and the rate-limit
// middleware agree on the vocabulary without importing this package.
//
// A fifth family (issue #205) records cascade fallback events:
//
// 腔   - nexus_cascade_fallback_total{reason}
//
// The reason label values are "timeout", "transport_error", or
// "malformed_toolcall".
type RouteCounters struct {
	mu sync.Mutex

	routeDecisions           map[counterKey]*uint64
	slmDecisions             map[counterKey]*uint64
	lowConfidenceEscalations map[counterKey]*uint64
	slmCacheHits             map[string]*uint64 // "exact" | "semantic" (issue #352)
	slmCacheMisses           *uint64
	slmCacheEvictions        map[string]*uint64 // "ttl" | "lru" (issue #449)
	rejections               map[string]*uint64
	responseTruncated        uint64 // nexus_upstream_response_truncated_total
	fusionArbiter            map[string]*uint64
	rRAGHits                 map[string]*uint64
	rRAGMisses               map[string]*uint64
	cascadeFallbacks         map[string]*uint64
	arbiterCache             map[string]*uint64 // "hit" | "miss"
	slmEscalations           map[string]*uint64 // reason label for issue #301

	judgeQueueOverflow   uint64 // atomic; use atomic.AddUint64/atomic.LoadUint64
	qualityQueueOverflow uint64 // atomic; use atomic.AddUint64/atomic.LoadUint64

	// RAG embedding cache counters (issue #303).
	ragCacheHits   *uint64
	ragCacheMisses *uint64

	// Panel panic counter (issue #309). Bumped when a panel goroutine
	// recovers from a panic and returns a panic error.
	panelPanics uint64 // atomic

	// collector is an optional Collector whose CircuitBreakerGauges()
	// are merged into the /metrics output when non-nil.
	collector *Collector

	// gaugeProviders supply live gauge readings (e.g. dropped counters)
	// at scrape time. They are passed to RenderPrometheus by Handler().
	gaugeProviders []GaugeProvider
}

// NewRouteCounters returns a ready-to-use RouteCounters.
func NewRouteCounters() *RouteCounters {
	misses := uint64(0)
	cHits, cMisses := uint64(0), uint64(0)
	return &RouteCounters{
		routeDecisions:           make(map[counterKey]*uint64),
		slmDecisions:             make(map[counterKey]*uint64),
		lowConfidenceEscalations: make(map[counterKey]*uint64),
		slmCacheHits:             make(map[string]*uint64),
		slmCacheMisses:           &misses,
		slmCacheEvictions:        make(map[string]*uint64),
		rejections:               make(map[string]*uint64),
		fusionArbiter:            make(map[string]*uint64),
		rRAGHits:                 make(map[string]*uint64),
		rRAGMisses:               make(map[string]*uint64),
		cascadeFallbacks:         make(map[string]*uint64),
		arbiterCache:             make(map[string]*uint64),
		slmEscalations:           make(map[string]*uint64),
		ragCacheHits:             &cHits,
		ragCacheMisses:           &cMisses,
	}
}

// SetGlobalTruncationCounter sets the package-level truncation counter
// to point at the RouteCounters' internal truncation counter. Called
// once at startup so ReadAllLimited can increment the counter without
// needing a *RouteCounters reference.
func (rc *RouteCounters) SetGlobalTruncationCounter() {
	SetTruncationCounter(&rc.responseTruncated)
}

// Observe records a single routing decision. Call this from the chat
// handler after planner.Plan returns. The method is safe for
// concurrent use; it never blocks.
//
// Parameters:
//   - route: the chosen route ("local", "frontier", "fusion")
//   - source: the decision source ("guardrail", "dsl", "slm", "slm-error", "escalation")
//   - confidence: the SLM confidence in [0,1] (0.5 neutral; pass 0 for non-SLM)
//   - taskType: the SLM category bucket (empty for non-SLM sources)
func (rc *RouteCounters) Observe(route, source string, confidence float64, taskType, model string) {
	if rc == nil {
		return
	}
	// Always bump the aggregate route-decisions counter.
	k := counterKey{route: route, source: source}
	atomic.AddUint64(rc.slot(rc.routeDecisions, k), 1)

	// SLM-sourced decisions (success or error) also feed the
	// SLM-specific counter so operators can cross-tabulate by
	// task type and confidence bucket.
	if source == "slm" || source == "slm-error" {
		bucket := BucketConfidence(confidence)
		if source != "slm" {
			// slm-error carries no real confidence signal —
			// bucket as "none" so the series does not pollute
			// the low/medium/high distribution.
			bucket = bucketNone
		}
		sk := counterKey{
			route:      route,
			confBucket: bucket,
			taskType:   taskType,
		}
		atomic.AddUint64(rc.slot(rc.slmDecisions, sk), 1)

		// Low-confidence escalation: an SLM decision that landed
		// on frontier with confidence below the low threshold.
		// This is the signal the issue asks to make
		// distinguishable from a plain frontier decision.
		if route == "frontier" && confidence > 0 && confidence < confidenceLow {
			ek := counterKey{taskType: taskType}
			atomic.AddUint64(rc.slot(rc.lowConfidenceEscalations, ek), 1)
		}
	}

	// Source==escalation is the planner's defensive nil-SLM path
	// (the SLM timed out or was nil, so the planner fell back to
	// frontier). Record it under low-confidence escalations so the
	// counter captures every frontier-bound override, not just the
	// SLM-confidence ones above.
	if source == "escalation" {
		ek := counterKey{taskType: taskType}
		atomic.AddUint64(rc.slot(rc.lowConfidenceEscalations, ek), 1)
	}

	// Source==slm-escalation is the planner's hard override for
	// low-confidence SLM decisions (issue #301). Record it under
	// slm-escalations with reason label so the metric is
	// nexus_slm_escalations_total{reason="low_confidence"}.
	if source == "slm-escalation" {
		atomic.AddUint64(rc.escalationSlot("low_confidence"), 1)
	}
}

// ObserveRejection records a single rejected request, partitioned by
// reason (issue #119). Call this from every early-return path in the
// chat handler (method, body_too_large, bad_request, ...) and from
// the rate-limit middleware's 429 path. The method is safe for
// concurrent use and never blocks; nil receivers are a no-op so
// callers can invoke it unconditionally. reason is the short label
// value that appears in the Prometheus exposition.
func (rc *RouteCounters) ObserveRejection(reason string) {
	if rc == nil {
		return
	}
	atomic.AddUint64(rc.reasonSlot(reason), 1)
}

// ObserveResponseTruncated increments the counter for upstream responses
// that were truncated because they exceeded MaxResponseBytes (issue #365).
// Safe for concurrent use; nil receivers are a no-op.
func (rc *RouteCounters) ObserveResponseTruncated() {
	if rc == nil {
		return
	}
	atomic.AddUint64(&rc.responseTruncated, 1)
}

// ObserveFusionOutcome records the outcome of a fusion panel after
// PanelStreaming returns (issue #187). arbiterSkipped is true when
// the two panel members agreed (SimilarityRatio >= agreementThreshold)
// and the arbiter was not invoked; false when disagreement triggered
// arbiter synthesis. This gives operators the data to compute the
// fusion agreement rate: skipped/(skipped+invoked).
func (rc *RouteCounters) ObserveFusionOutcome(arbiterSkipped bool) {
	if rc == nil {
		return
	}
	outcome := "invoked"
	if arbiterSkipped {
		outcome = "skipped"
	}
	atomic.AddUint64(rc.fusionSlot(outcome), 1)
}

// fusionSlot returns the *uint64 for the fusion outcome label, creating
// it if absent. Same lock-then-atomic pattern as slot.
func (rc *RouteCounters) fusionSlot(outcome string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.fusionArbiter[outcome]
	if !ok {
		v := uint64(0)
		p = &v
		rc.fusionArbiter[outcome] = p
	}
	rc.mu.Unlock()
	return p
}

// ObserveRAGHit records a RAG retrieval hit (issue #186). filename
// is the source file of the matched example and is attached as a
// label so operators can see which snippets are being retrieved.
func (rc *RouteCounters) ObserveRAGHit(filename string) {
	if rc == nil {
		return
	}
	atomic.AddUint64(rc.ragHitSlot(filename), 1)
}

// ObserveRAGMiss records a RAG retrieval miss (issue #186). reason
// is the miss cause: "empty_store" when the RAG store has no indexed
// examples, "threshold" when the best match scored below the similarity
// floor, or "embed_error" when the embedding call failed.
func (rc *RouteCounters) ObserveRAGMiss(reason string) {
	if rc == nil {
		return
	}
	atomic.AddUint64(rc.ragMissSlot(reason), 1)
}

// ObserveRAGCacheHit records a RAG prompt embedding cache hit (issue #303).
func (rc *RouteCounters) ObserveRAGCacheHit() {
	if rc == nil || rc.ragCacheHits == nil {
		return
	}
	atomic.AddUint64(rc.ragCacheHits, 1)
}

// ObserveRAGCacheMiss records a RAG prompt embedding cache miss (issue #303).
func (rc *RouteCounters) ObserveRAGCacheMiss() {
	if rc == nil || rc.ragCacheMisses == nil {
		return
	}
	atomic.AddUint64(rc.ragCacheMisses, 1)
}

// ragHitSlot returns the *uint64 for a hit filename, creating it if absent.
func (rc *RouteCounters) ragHitSlot(filename string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.rRAGHits[filename]
	if !ok {
		v := uint64(0)
		p = &v
		rc.rRAGHits[filename] = p
	}
	rc.mu.Unlock()
	return p
}

// ragMissSlot returns the *uint64 for a miss reason, creating it if absent.
func (rc *RouteCounters) ragMissSlot(reason string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.rRAGMisses[reason]
	if !ok {
		v := uint64(0)
		p = &v
		rc.rRAGMisses[reason] = p
	}
	rc.mu.Unlock()
	return p
}

// ObserveSLMCacheHit records one SLM decision cache hit (issue #206).
// kind is "exact" for an exact string match or "semantic" for a cosine-
// similarity match (issue #245, #352). The cache deduplicates identical
// prompts within a TTL window; a hit means the same prompt was seen
// recently and the cached decision was returned instead of calling the
// SLM. Safe for concurrent use; nil receivers are a no-op.
func (rc *RouteCounters) ObserveSLMCacheHit(kind string) {
	if rc == nil || rc.slmCacheHits == nil {
		return
	}
	rc.mu.Lock()
	p, ok := rc.slmCacheHits[kind]
	if !ok {
		v := uint64(0)
		p = &v
		rc.slmCacheHits[kind] = p
	}
	rc.mu.Unlock()
	atomic.AddUint64(p, 1)
}

// ObserveSLMCacheMiss records one SLM decision cache miss (issue #206).
// A miss means the prompt was not in the cache (or was expired), so
// the SLM was called to produce a decision. Safe for concurrent use;
// nil receivers are a no-op.
func (rc *RouteCounters) ObserveSLMCacheMiss() {
	if rc == nil || rc.slmCacheMisses == nil {
		return
	}
	atomic.AddUint64(rc.slmCacheMisses, 1)
}

// ObserveSLMCacheEviction records one SLM decision cache eviction
// (issue #449). reason is the bounded label value: "ttl" when an
// entry was removed because its TTL elapsed, or "lru" when an entry
// was removed to make room at capacity. Distinguishing the two
// reasons lets operators tell whether the cache is undersized (high
// lru) or its TTL is too short (high ttl). Safe for concurrent use;
// nil receivers and empty reason are no-ops so callers can invoke
// unconditionally without guarding the call site.
func (rc *RouteCounters) ObserveSLMCacheEviction(reason string) {
	if rc == nil || reason == "" {
		return
	}
	atomic.AddUint64(rc.slmCacheEvictionSlot(reason), 1)
}

// slmCacheEvictionSlot returns the *uint64 for the SLM cache eviction
// reason label, creating it if absent. Same lock-then-atomic pattern
// as reasonSlot: the mutex guards the map mutation only, the increment
// happens lock-free.
func (rc *RouteCounters) slmCacheEvictionSlot(reason string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.slmCacheEvictions[reason]
	if !ok {
		v := uint64(0)
		p = &v
		rc.slmCacheEvictions[reason] = p
	}
	rc.mu.Unlock()
	return p
}

// ObserveCascadeFallback records a single cascade fallback event,
// partitioned by reason (issue #205). Call this after Cascade.Run
// returns when FallbackReason is non-empty. The method is safe for
// concurrent use and never blocks; nil receivers are a no-op.
// reason is one of "timeout", "transport_error", or "malformed_toolcall".
func (rc *RouteCounters) ObserveCascadeFallback(reason string) {
	if rc == nil || reason == "" {
		return
	}
	atomic.AddUint64(rc.cascadeFallbackSlot(reason), 1)
}

// ObserveArbiterCacheHit records an arbiter cache lookup result
// (issue #232). hit=true means the synthesis was served from cache;
// hit=false means the cache missed and the arbiter was invoked.
// The method is safe for concurrent use and never blocks; nil
// receivers are a no-op.
func (rc *RouteCounters) ObserveArbiterCacheHit(hit bool) {
	if rc == nil {
		return
	}
	label := "false"
	if hit {
		label = "true"
	}
	rc.mu.Lock()
	p, ok := rc.arbiterCache[label]
	if !ok {
		v := uint64(0)
		p = &v
		rc.arbiterCache[label] = p
	}
	rc.mu.Unlock()
	atomic.AddUint64(p, 1)
}

// ObserveJudgeQueueOverflow records one judge queue overflow event
// (issue #226). The judge evaluator calls this when its internal
// Enqueue returns false due to a full queue.
func (rc *RouteCounters) ObserveJudgeQueueOverflow() {
	if rc == nil {
		return
	}
	atomic.AddUint64(&rc.judgeQueueOverflow, 1)
}

// ObserveQualityQueueOverflow records one quality verifier queue overflow
// event (issue #226). The quality verifier calls this when its
// internal Submit returns false due to a full queue.
func (rc *RouteCounters) ObserveQualityQueueOverflow() {
	if rc == nil {
		return
	}
	atomic.AddUint64(&rc.qualityQueueOverflow, 1)
}

// ObservePanelPanic records one panel goroutine panic recovery (issue #309).
func (rc *RouteCounters) ObservePanelPanic() {
	if rc == nil {
		return
	}
	atomic.AddUint64(&rc.panelPanics, 1)
}

// reasonSlot returns the *uint64 for reason, creating it if absent.
// Same lock-then-atomic pattern as slot: the mutex guards the map
// mutation only, the increment happens lock-free.
func (rc *RouteCounters) reasonSlot(reason string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.rejections[reason]
	if !ok {
		v := uint64(0)
		p = &v
		rc.rejections[reason] = p
	}
	rc.mu.Unlock()
	return p
}

// ReadAllLimited reads from r with a byte limit of maxBytes. If the
// response body is larger than maxBytes, the body is truncated and
// ObserveResponseTruncated is called on rc (if non-nil) or the
// package-level counter is incremented (issue #365). This prevents
// memory exhaustion from a malicious upstream returning gigabytes.
// The returned error is any read error encountered before hitting the
// limit; a truncation itself is not treated as an error.
func ReadAllLimited(rc *RouteCounters, r io.Reader, maxBytes int) ([]byte, error) {
	lr := io.LimitReader(r, int64(maxBytes))
	body, err := io.ReadAll(lr)
	// If we read exactly maxBytes, the response was likely truncated.
	// The edge case of a response that is exactly maxBytes is
	// astronomically unlikely at 64 MiB.
	if len(body) >= maxBytes {
		if rc != nil {
			rc.ObserveResponseTruncated()
		} else {
			IncrementTruncationCounter()
		}
	}
	return body, err
}

// cascadeFallbackSlot returns the *uint64 for cascade fallback reason,
// creating it if absent. Same lock-then-atomic pattern as slot.
func (rc *RouteCounters) cascadeFallbackSlot(reason string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.cascadeFallbacks[reason]
	if !ok {
		v := uint64(0)
		p = &v
		rc.cascadeFallbacks[reason] = p
	}
	rc.mu.Unlock()
	return p
}

// escalationSlot returns the *uint64 for an SLM escalation with the
// given reason label, creating it if absent.
func (rc *RouteCounters) escalationSlot(reason string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.slmEscalations[reason]
	if !ok {
		v := uint64(0)
		p = &v
		rc.slmEscalations[reason] = p
	}
	rc.mu.Unlock()
	return p
}

// slot returns the *uint64 for key, creating it if absent. The
// pointer is returned so the caller can atomic.AddUint64 without
// holding the lock during the increment.
func (rc *RouteCounters) slot(m map[counterKey]*uint64, key counterKey) *uint64 {
	rc.mu.Lock()
	p, ok := m[key]
	if !ok {
		v := uint64(0)
		p = &v
		m[key] = p
	}
	rc.mu.Unlock()
	return p
}

// Handler returns an http.Handler that writes the Prometheus text
// exposition. The handler sets Content-Type to the Prometheus text
// format and never errors — a scrape always returns 200 with the
// current counter snapshot. When a Collector is set (via SetCollector),
// circuit breaker gauges from the collector are also included.
func (rc *RouteCounters) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = rc.WriteTo(w)
		if rc.collector != nil || len(rc.gaugeProviders) > 0 {
			RenderPrometheus(w, rc.collector, rc.gaugeProviders...)
		}
	})
}

// SetCollector attaches a Collector whose circuit breaker gauges are
// included in the /metrics output. Nil clears the collector.
func (rc *RouteCounters) SetCollector(c *Collector) {
	rc.collector = c
}

// SetGaugeProviders attaches one or more GaugeProviders whose live
// readings are included in the /metrics output via RenderPrometheus.
// Nil providers are silently ignored at scrape time.
func (rc *RouteCounters) SetGaugeProviders(providers ...GaugeProvider) {
	rc.gaugeProviders = append(rc.gaugeProviders, providers...)
}

// Snapshot returns a point-in-time copy of the routing decision counters
// as a sorted slice. Used by the /status JSON endpoint to provide a
// routing distribution snapshot without exposing the internal counter map.
func (rc *RouteCounters) Snapshot() []RouteCounterEntry {
	if rc == nil {
		return nil
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()

	var entries []RouteCounterEntry
	for k, v := range rc.routeDecisions {
		entries = append(entries, RouteCounterEntry{
			Route:  k.route,
			Source: k.source,
			Count:  atomic.LoadUint64(v),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Route != entries[j].Route {
			return entries[i].Route < entries[j].Route
		}
		return entries[i].Source < entries[j].Source
	})
	return entries
}

// RouteCounterEntry is one route/source bucket from the routing snapshot.
type RouteCounterEntry struct {
	Route  string `json:"route"`
	Source string `json:"source"`
	Count  uint64 `json:"count"`
}

// WriteTo writes the full Prometheus text exposition to w. The output
// is deterministic: series are sorted by label key so successive
// scrapes diff cleanly. Returns the number of bytes written and any
// write error.
func (rc *RouteCounters) WriteTo(w io.Writer) (int64, error) {
	if rc == nil {
		return 0, nil
	}
	var total int64
	if n, err := writeSeries(w, "nexus_route_decisions_total",
		"Total routing decisions partitioned by route and source.",
		rc.routeDecisions, []string{"route", "source"}); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeSeries(w, "nexus_slm_decisions_total",
		"SLM-sourced routing decisions partitioned by route, confidence bucket, and task type.",
		rc.slmDecisions, []string{"route", "confidence_bucket", "task_type"}); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeSeries(w, "nexus_slm_low_confidence_escalations_total",
		"Requests escalated to frontier because the SLM confidence was below the low threshold.",
		rc.lowConfidenceEscalations, []string{"task_type"}); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeRejectionSeries(w, "nexus_slm_escalations_total",
		"Hard escalations to frontier due to low SLM confidence (issue #301).",
		rc.slmEscalations); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeSLMCacheSeries(w, rc.slmCacheHits, rc.slmCacheMisses); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeSLMCacheEvictionsSeries(w, rc.slmCacheEvictions); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeRejectionSeries(w, "nexus_requests_rejected_total",
		"Requests the proxy rejected before they reached an upstream.",
		rc.rejections); err != nil {
		return total, err
	} else {
		total += n
	}
	// nexus_upstream_response_truncated_total (issue #365)
	// Read from the ioutils package-level counter which is incremented
	// by ReadAllLimited calls in upstream, cascade, router, judge, and rag.
	truncated := ioutils.ReadAllTruncatedCounter()
	n, err := fmt.Fprintf(w, "# HELP nexus_upstream_response_truncated_total %s\n# TYPE nexus_upstream_response_truncated_total counter\nnexus_upstream_response_truncated_total %d\n",
		"Upstream responses truncated because they exceeded MaxResponseBytes.",
		truncated)
	if err != nil {
		return total, err
	}
	total += int64(n)

	if n, err := writeFusionSeries(w, "nexus_fusion_arbiter_total",
		"Fusion panel outcomes: arbiter skipped (agreement) or invoked (disagreement).",
		rc.fusionArbiter); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeRAGSeries(w, "nexus_rag_retrieval_total",
		"RAG retrieval outcomes partitioned by hit/miss and reason or filename.",
		rc.rRAGHits, rc.rRAGMisses); err != nil {
		return total, err
	} else {
		total += n
	}

	// RAG embedding cache counters (issue #303).
	hitsVal := atomic.LoadUint64(rc.ragCacheHits)
	missesVal := atomic.LoadUint64(rc.ragCacheMisses)
	if n, err := fmt.Fprintf(w, "# HELP nexus_rag_cache_hits_total RAG prompt embedding cache hits.\n# TYPE nexus_rag_cache_hits_total counter\nnexus_rag_cache_hits_total %d\n", hitsVal); err != nil {
		return total, err
	} else {
		total += int64(n)
	}
	if n, err := fmt.Fprintf(w, "# HELP nexus_rag_cache_misses_total RAG prompt embedding cache misses.\n# TYPE nexus_rag_cache_misses_total counter\nnexus_rag_cache_misses_total %d\n", missesVal); err != nil {
		return total, err
	} else {
		total += int64(n)
	}

	if n, err := writeRejectionSeries(w, "nexus_cascade_fallback_total",
		"Cascade fallback events partitioned by reason (timeout, transport_error, malformed_toolcall).",
		rc.cascadeFallbacks); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeRejectionSeries(w, "nexus_fusion_arbiter_cache_total",
		"Fusion arbiter synthesis cache hits and misses (issue #232).",
		rc.arbiterCache); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeOverflowSeries(w, "nexus_judge_queue_overflow_total",
		"Judge evaluator queue overflow events — sample was dropped because the queue was full.",
		&rc.judgeQueueOverflow); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeOverflowSeries(w, "nexus_quality_queue_overflow_total",
		"Quality verifier queue overflow events — event was dropped because the queue was full.",
		&rc.qualityQueueOverflow); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeOverflowSeries(w, "nexus_panel_panics_total",
		"Panel goroutine panic recoveries (issue #309).",
		&rc.panelPanics); err != nil {
		return total, err
	} else {
		total += n
	}
	return total, nil
}

// writeSeries emits one HELP line, one TYPE line, then one sample
// line per non-zero counter. The labels slice declares the label
// names in the order they should appear; the counterKey fields are
// read positionally via labelValue.
func writeSeries(w io.Writer, name, help string, m map[counterKey]*uint64, labels []string) (int64, error) {
	var total int64
	// Always emit HELP/TYPE so scrapers see the metric family even
	// when no samples have been recorded yet.
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	if err != nil {
		return total + int64(n), err
	}
	total += int64(n)
	if len(m) == 0 {
		return total, nil
	}
	// Collect and sort keys for deterministic output.
	keys := make([]counterKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keyLess(keys[i], keys[j], labels)
	})
	for _, k := range keys {
		v := atomic.LoadUint64(m[k])
		n, err := fmt.Fprintf(w, "%s%s %d\n", name, formatLabels(k, labels), v)
		if err != nil {
			return total + int64(n), err
		}
		total += int64(n)
	}
	return total, nil
}

// writeRejectionSeries emits the nexus_requests_rejected_total
// family. It is a String-keyed variant of writeSeries so the
// rejection counters (keyed only by reason) do not need to reuse the
// multi-field counterKey struct. Output is sorted by reason for
// deterministic scrape diffs.
func writeRejectionSeries(w io.Writer, name, help string, m map[string]*uint64) (int64, error) {
	var total int64
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	if err != nil {
		return total + int64(n), err
	}
	total += int64(n)
	if len(m) == 0 {
		return total, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := atomic.LoadUint64(m[k])
		n, err := fmt.Fprintf(w, "%s{reason=\"%s\"} %d\n", name, sanitizeLabel(k), v)
		if err != nil {
			return total + int64(n), err
		}
		total += int64(n)
	}
	return total, nil
}

// writeFusionSeries emits the nexus_fusion_arbiter_total family (issue #187).
// outcome="skipped" when the two panel members agreed and the arbiter was not invoked;
// outcome="invoked" when disagreement triggered arbiter synthesis.
// Output is sorted by outcome label for deterministic scrape diffs.
func writeFusionSeries(w io.Writer, name, help string, m map[string]*uint64) (int64, error) {
	var total int64
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	if err != nil {
		return total + int64(n), err
	}
	total += int64(n)
	if len(m) == 0 {
		return total, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := atomic.LoadUint64(m[k])
		n, err := fmt.Fprintf(w, "%s{outcome=%q} %d\n", name, sanitizeLabel(k), v)
		if err != nil {
			return total + int64(n), err
		}
		total += int64(n)
	}
	return total, nil
}

// writeRAGSeries emits the nexus_rag_retrieval_total family.
// hits and misses share the same metric name but are distinguished by
// the "hit" label (true/false). filename is attached to hits so
// operators can see which snippets fire most often; reason is attached
// to misses so they can diagnose why retrieval fails. Output is sorted
// by hit then by key for deterministic scrape diffs.
func writeRAGSeries(w io.Writer, name, help string, hits, misses map[string]*uint64) (int64, error) {
	var total int64
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	if err != nil {
		return total, err
	}
	total += int64(n)

	writeHitPairs := func(m map[string]*uint64) (int64, error) {
		var subTotal int64
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := atomic.LoadUint64(m[k])
			n, err := fmt.Fprintf(w, "%s{hit=%q,filename=%q} %d\n", name, "true", sanitizeLabel(k), v)
			if err != nil {
				return subTotal + int64(n), err
			}
			subTotal += int64(n)
		}
		return subTotal, nil
	}

	writeMissPairs := func(m map[string]*uint64) (int64, error) {
		var subTotal int64
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := atomic.LoadUint64(m[k])
			n, err := fmt.Fprintf(w, "%s{hit=%q,reason=%q} %d\n", name, "false", sanitizeLabel(k), v)
			if err != nil {
				return subTotal + int64(n), err
			}
			subTotal += int64(n)
		}
		return subTotal, nil
	}

	if n, err := writeHitPairs(hits); err != nil {
		return total, err
	} else {
		total += n
	}
	if n, err := writeMissPairs(misses); err != nil {
		return total, err
	} else {
		total += n
	}
	return total, nil
}

// writeSLMCacheSeries emits the nexus_slm_cache_hits_total and
// nexus_slm_cache_misses_total counters (issue #206, #352). These track
// the SLM decision cache hit rate so operators can tune the cache
// TTL and diagnose cache ineffectiveness. The hits map may be nil or
// empty; nil is treated as zero. Misses is a single pointer (no labels).
func writeSLMCacheSeries(w io.Writer, hits map[string]*uint64, misses *uint64) (int64, error) {
	var total int64

	n, err := fmt.Fprintf(w, "# HELP nexus_slm_cache_hits_total SLM decision cache hits partitioned by kind (issue #352).\n# TYPE nexus_slm_cache_hits_total counter\n")
	if err != nil {
		return total, err
	}
	total += int64(n)

	// Emit one line per kind label, sorted for determinism.
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := atomic.LoadUint64(hits[k])
		n, err := fmt.Fprintf(w, "nexus_slm_cache_hits_total{kind=%q} %d\n", k, v)
		if err != nil {
			return total, err
		}
		total += int64(n)
	}

	missesVal := atomic.LoadUint64(misses)
	n, err = fmt.Fprintf(w, "# HELP nexus_slm_cache_misses_total SLM decision cache misses.\n# TYPE nexus_slm_cache_misses_total counter\n")
	if err != nil {
		return total, err
	}
	total += int64(n)
	n, err = fmt.Fprintf(w, "nexus_slm_cache_misses_total %d\n", missesVal)
	if err != nil {
		return total, err
	}
	total += int64(n)

	return total, nil
}

// writeSLMCacheEvictionsSeries emits the
// nexus_slm_cache_evictions_total counter family (issue #449). The
// reason label is bounded to the small closed set defined by the
// router.EvictionReasonTTL / EvictionReasonLRU constants so the metric
// family never exceeds two series. Output is sorted by reason label
// for deterministic scrape diffs. The map may be nil or empty; an
// empty map still emits HELP/TYPE so scrapers can discover the
// family even before the first eviction fires.
func writeSLMCacheEvictionsSeries(w io.Writer, evictions map[string]*uint64) (int64, error) {
	var total int64

	n, err := fmt.Fprintf(w, "# HELP nexus_slm_cache_evictions_total SLM decision cache evictions partitioned by reason (issue #449).\n# TYPE nexus_slm_cache_evictions_total counter\n")
	if err != nil {
		return total, err
	}
	total += int64(n)

	keys := make([]string, 0, len(evictions))
	for k := range evictions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := atomic.LoadUint64(evictions[k])
		n, err := fmt.Fprintf(w, "nexus_slm_cache_evictions_total{reason=%q} %d\n", k, v)
		if err != nil {
			return total, err
		}
		total += int64(n)
	}

	return total, nil
}

// writeOverflowSeries emits a simple unlabeled counter for queue
// overflow events (issue #226). Unlike the map-based counters, the
// overflow counters are plain uint64 fields so we can use atomic
// operations without mutex contention on the hot path.
func writeOverflowSeries(w io.Writer, name, help string, counter *uint64) (int64, error) {
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	if err != nil {
		return int64(n), err
	}
	v := atomic.LoadUint64(counter)
	n, err = fmt.Fprintf(w, "%s %d\n", name, v)
	if err != nil {
		return int64(n), err
	}
	return int64(n), nil
}

// keyLess reports whether k1 < k2 considering only the fields named
// in labels (in order). This gives writeSeries a stable sort.
func keyLess(k1, k2 counterKey, labels []string) bool {
	for _, label := range labels {
		a, b := labelValue(k1, label), labelValue(k2, label)
		if a != b {
			return a < b
		}
	}
	return false
}

// formatLabels renders the Prometheus label set for a key, including
// only the named labels in the given order.
func formatLabels(k counterKey, labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, label := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(label)
		b.WriteString(`="`)
		b.WriteString(sanitizeLabel(labelValue(k, label)))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// labelValue extracts the field from k that corresponds to label.
func labelValue(k counterKey, label string) string {
	switch label {
	case "route":
		return k.route
	case "source":
		return k.source
	case "confidence_bucket":
		return k.confBucket
	case "task_type":
		return k.taskType
	default:
		return ""
	}
}

// sanitizeLabel escapes characters that are invalid in Prometheus
// label values (backslash, double-quote, newline).
func sanitizeLabel(s string) string {
	if !strings.ContainsAny(s, `\"\`) && !strings.Contains(s, "\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
