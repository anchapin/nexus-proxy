// Package tracing provides W3C Trace Context-compatible distributed
// tracing with an OTLP/JSON HTTP exporter.
//
// The API is stdlib-only (crypto/rand, encoding/hex, encoding/json,
// net/http) and is designed so NEXUS_TRACING_ENDPOINT="" (the
// default) disables the entire subsystem: no background goroutines,
// no allocations beyond a nil-check on every StartSpan call.
//
// When enabled, the chat handler wraps each request phase (RAG, TOON,
// routing, upstream, streaming) in a Span tree. A background exporter
// buffers spans and POSTs them to the configured OTLP/JSON endpoint
// in batches so a stalled collector never blocks the request path.
//
// Inbound W3C `traceparent` headers are honoured so spans from an
// upstream caller attach to the same trace. Missing or malformed
// headers fall through to a fresh trace id, keeping the hot path
// unaffected when the harness does not propagate context.
//
// Reference: https://www.w3.org/TR/trace-context/ and
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding.
package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// traceIDLen / spanIDLen are the W3C Trace Context byte lengths (16 /
// 8 bytes -> 32 / 16 hex chars). Both IDs are rendered lowercase hex
// so the OTLP collector accepts them verbatim.
const (
	traceIDLen = 16
	spanIDLen  = 8
)

// Context carries the active trace and span id. It is threaded
// through the request lifecycle so child spans attach to the correct
// parent.
//
// A zero-value Context (TraceID == "") is the "no tracing in scope"
// sentinel; StartSpan on it produces a new root span instead of
// attaching to a non-existent parent.
type Context struct {
	TraceID string
	SpanID  string
}

// WithSpanID returns a new Context with SpanID replaced. Useful when
// a callee only needs the new span id without the full Context.
func (c Context) WithSpanID(spanID string) Context {
	return Context{TraceID: c.TraceID, SpanID: spanID}
}

// NewTraceID returns a fresh 32-hex-char trace id, sourced from
// crypto/rand. Falls back to a time-based id when the kernel RNG is
// unreadable (extremely rare on linux); the fallback is unique enough
// for log correlation but is not cryptographically unique.
func NewTraceID() string {
	var b [traceIDLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Last-resort fallback so a broken /dev/urandom cannot crash
		// the binary on a hostile host. Uniqueness only matters for
		// trace correlation in this branch.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewSpanID returns a fresh 16-hex-char span id. Same fallback as
// NewTraceID.
func NewSpanID() string {
	var b [spanIDLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ParseTraceparent parses a W3C traceparent header value. Returns
// (traceID, spanID, ok). The accepted form is:
//
//	"00-<32 hex>-<16 hex>-<2 hex>"
//
// Rejected (returns ok=false): wrong version, malformed hex, wrong
// field lengths, or all-zero trace/span ids (which the W3C spec
// reserves as invalid). The trace-flags byte is accepted but the
// sampled bit is ignored — the OTLP exporter always sends every
// sampled span.
//
// The full W3C form is 55 characters; longer headers are rejected
// because the spec defines only the four fields above.
func ParseTraceparent(h string) (traceID, spanID string, ok bool) {
	const want = 55 // 2 + 1 + 32 + 1 + 16 + 1 + 2
	if len(h) != want {
		return "", "", false
	}
	if h[0:2] != "00" || h[2] != '-' || h[35] != '-' || h[52] != '-' {
		return "", "", false
	}
	traceID = h[3:35]
	spanID = h[36:52]
	if !isLowerHex(traceID) || !isLowerHex(spanID) {
		return "", "", false
	}
	// The W3C spec reserves all-zero trace and span ids as invalid
	// so a downstream service cannot confuse them with a real id.
	if isAllZero(traceID) || isAllZero(spanID) {
		return "", "", false
	}
	return traceID, spanID, true
}

// FormatTraceparent renders the W3C traceparent header for the given
// trace / span ids, using version 00 and the "sampled" flag set so
// downstream collectors do not skip the propagated span.
//
// Returns "" when traceID or spanID is empty so a partial context
// cannot produce an invalid header.
func FormatTraceparent(traceID, spanID string) string {
	if traceID == "" || spanID == "" {
		return ""
	}
	return "00-" + traceID + "-" + spanID + "-01"
}

// isLowerHex reports whether s is exactly len(s) lowercase hex
// characters. Used by ParseTraceparent to validate the W3C id fields.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}

// isAllZero reports whether every byte of s is '0'. Used by
// ParseTraceparent to reject the W3C-reserved invalid ids.
func isAllZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// Status enumerates the W3C / OTLP span status codes.
type Status int

const (
	// StatusUnset is the zero value; the exporter renders it as
	// STATUS_CODE_UNSET per the OTLP enum. A span that ends
	// without a status is considered "in progress" by collectors.
	StatusUnset Status = iota
	// StatusOK indicates a successful operation.
	StatusOK
	// StatusError indicates a failure; the StatusMessage carries
	// the human-readable detail.
	StatusError
)

// String returns the canonical OTLP enum spelling for the status.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusError:
		return "ERROR"
	default:
		return "UNSET"
	}
}

// Span is one traced operation. Spans form a tree via ParentSpanID;
// the root span has an empty ParentSpanID.
//
// A Span is safe for concurrent SetAttr / SetStatus / RecordError
// calls. End must be called exactly once; subsequent calls are
// no-ops. The zero value is not usable — obtain Spans via
// Exporter.StartSpan or StartSpan.
type Span struct {
	// Public fields exposed for marshalling. They are read-only
	// from outside this package after End(); the mutex below
	// guards them while the span is in flight.
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Name          string
	StartTime     time.Time
	EndTime       time.Time
	Attributes    map[string]any
	Status        Status
	StatusMessage string

	// ended is set by End and prevents the post-End Submit path
	// from re-firing. mu guards every mutable field above.
	mu       sync.Mutex
	ended    bool
	exporter *Exporter // non-nil for spans created via a live exporter
}

// StartSpan creates a new span attached to parent. When parent is
// the zero value (no tracing in scope) the new span gets a fresh
// trace id and an empty ParentSpanID — it becomes a root span.
//
// StartSpan is the standalone entry point that produces a span
// without binding it to an exporter; the returned span's End is a
// no-op (the caller is responsible for forwarding it elsewhere if
// needed). For the chat-handler hot path use Exporter.StartSpan,
// which binds the span to the exporter so End submits it
// automatically.
func StartSpan(parent Context, name string) (Context, *Span) {
	if parent.TraceID == "" {
		parent.TraceID = NewTraceID()
	}
	sid := NewSpanID()
	s := &Span{
		TraceID:      parent.TraceID,
		SpanID:       sid,
		ParentSpanID: parent.SpanID, // empty when parent.SpanID == ""
		Name:         name,
		StartTime:    time.Now(),
		Attributes:   make(map[string]any, 4),
		Status:       StatusUnset,
	}
	return parent.WithSpanID(sid), s
}

// SetAttr stores key=val on the span. Safe from any goroutine;
// nil-receiver safe so callers can defer End() without a nil-check.
func (s *Span) SetAttr(key string, val any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Attributes[key] = val
	s.mu.Unlock()
}

// SetStatus stamps the span's status. Passing StatusUnset clears any
// prior status without setting one. Nil-receiver safe.
func (s *Span) SetStatus(code Status, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Status = code
	s.StatusMessage = msg
	s.mu.Unlock()
}

// RecordError stamps the span status to ERROR with the error's text.
// A nil error is ignored so callers can defer RecordError without a
// guard. Nil-receiver safe.
func (s *Span) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.Status = StatusError
	s.StatusMessage = err.Error()
	s.mu.Unlock()
}

// End marks the span complete and, when the span was created via an
// Exporter, submits it to the background OTLP/JSON POST loop. After
// End the span's public fields are stable and may be read
// concurrently; calling End twice is a no-op (the second call is
// silently dropped so a misplaced defer is harmless).
//
// If the span was never explicitly statused, End promotes the
// status from UNSET to OK so the OTLP record reflects success.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.EndTime = time.Now()
	if s.Status == StatusUnset {
		s.Status = StatusOK
	}
	exp := s.exporter
	s.mu.Unlock()
	if exp != nil {
		exp.Submit(s)
	}
}
