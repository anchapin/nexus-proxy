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
// Three metric families are exposed:
//   - nexus_route_decisions_total{route,source}
//   - nexus_slm_decisions_total{route,confidence_bucket,task_type}
//   - nexus_slm_low_confidence_escalations_total{task_type}
//
// A fourth family (issue #119) records requests the proxy rejected
// before they reached an upstream:
//   - nexus_requests_rejected_total{reason}
//
// A fifth family (issue #118) records upstream streams the proxy
// terminated early because of a mid-stream TCP drop:
//   - nexus_stream_truncated_total{route}
//
// The reason label values are short, bounded strings (method,
// body_too_large, bad_request, rate_limit, ...) defined as constants
// in internal/handlers so the chat handler and the rate-limit
// middleware agree on the vocabulary without importing this package.
type RouteCounters struct {
	mu sync.Mutex

	routeDecisions           map[counterKey]*uint64
	slmDecisions             map[counterKey]*uint64
	lowConfidenceEscalations map[counterKey]*uint64
	rejections               map[string]*uint64

	// streamTruncations tracks the total number of upstream streams
	// the proxy terminated early because of a mid-stream TCP drop
	// (issue #118), partitioned by route so operators can see whether
	// local or frontier upstreams are the source of drops. Same
	// lock-then-atomic pattern as the rejections map.
	streamTruncations map[string]*uint64

	// judgeDropped tracks the total number of judge samples dropped
	// because the evaluation queue was full (issue #111). It is a
	// single label-free counter — the evaluator's atomic Dropped()
	// value is synced into this slot by the onDrop callback.
	judgeDropped *uint64
}

// NewRouteCounters returns a ready-to-use RouteCounters.
func NewRouteCounters() *RouteCounters {
	v := uint64(0)
	return &RouteCounters{
		routeDecisions:           make(map[counterKey]*uint64),
		slmDecisions:             make(map[counterKey]*uint64),
		lowConfidenceEscalations: make(map[counterKey]*uint64),
		rejections:               make(map[string]*uint64),
		streamTruncations:        make(map[string]*uint64),
		judgeDropped:             &v,
	}
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
func (rc *RouteCounters) Observe(route, source string, confidence float64, taskType string) {
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

// ObserveJudgeDrop synchronises the evaluator's running drop total
// into the nexus_judge_dropped_total Prometheus counter (issue #111).
// The evaluator's onDrop callback passes the cumulative count so we
// use Store (not Add) to mirror the authoritative value without
// double-counting.
func (rc *RouteCounters) ObserveJudgeDrop(total uint64) {
	if rc == nil {
		return
	}
	atomic.StoreUint64(rc.judgeDropped, total)
}

// ObserveStreamTruncation records one upstream stream the proxy
// terminated early because of a mid-stream TCP drop (issue #118),
// partitioned by route so operators can see whether local or frontier
// upstreams are the source of drops. Call this from the chat handler
// via the StreamTruncationObserver hook when upstream.StreamWithContext
// returns ErrUpstreamTruncated. Safe for concurrent use; a nil
// receiver is a no-op. route is the short label value ("local" |
// "frontier" | "fusion") that appears in the Prometheus exposition.
func (rc *RouteCounters) ObserveStreamTruncation(route string) {
	if rc == nil {
		return
	}
	atomic.AddUint64(rc.routeTruncationSlot(route), 1)
}

// routeTruncationSlot returns the *uint64 for route, creating it if
// absent. Same lock-then-atomic pattern as reasonSlot: the mutex
// guards the map mutation only, the increment happens lock-free on
// the returned pointer.
func (rc *RouteCounters) routeTruncationSlot(route string) *uint64 {
	rc.mu.Lock()
	p, ok := rc.streamTruncations[route]
	if !ok {
		val := uint64(0)
		p = &val
		rc.streamTruncations[route] = p
	}
	rc.mu.Unlock()
	return p
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
// current counter snapshot.
func (rc *RouteCounters) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = rc.WriteTo(w)
	})
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
	if n, err := writeRejectionSeries(w, "nexus_requests_rejected_total",
		"Requests the proxy rejected before they reached an upstream.",
		"reason", rc.rejections); err != nil {
		return total, err
	} else {
		total += n
	}
	// Upstream mid-stream TCP-drop counter (issue #118). Labelled by
	// route so a single PromQL query breaks truncations out by
	// destination (local / frontier / fusion). Only the streaming
	// paths can truncate; the buffered / cascade paths emit their own
	// [DONE] and never hit this counter.
	if n, err := writeRejectionSeries(w, "nexus_stream_truncated_total",
		"Upstream streams the proxy terminated early because of a mid-stream TCP drop (issue #118).",
		"route", rc.streamTruncations); err != nil {
		return total, err
	} else {
		total += n
	}
	// Judge queue overflow counter (issue #111). Label-free — a
	// single monotonic counter tracks the cumulative total.
	v := atomic.LoadUint64(rc.judgeDropped)
	n, err := fmt.Fprintf(w, "# HELP nexus_judge_dropped_total Judge samples dropped because the evaluation queue was full.\n# TYPE nexus_judge_dropped_total counter\nnexus_judge_dropped_total %d\n", v)
	total += int64(n)
	if err != nil {
		return total, err
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

// writeRejectionSeries emits a string-keyed, single-label counter
// family. It is the string-keyed variant of writeSeries so the
// rejection counters (labelled only by reason) and the stream-
// truncation counters (labelled only by route) do not need to reuse
// the multi-field counterKey struct. label is the Prometheus label
// name ("reason" or "route"); output is sorted by key for
// deterministic scrape diffs.
func writeRejectionSeries(w io.Writer, name, help, label string, m map[string]*uint64) (int64, error) {
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
		n, err := fmt.Fprintf(w, "%s{%s=\"%s\"} %d\n", name, label, sanitizeLabel(k), v)
		if err != nil {
			return total + int64(n), err
		}
		total += int64(n)
	}
	return total, nil
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
