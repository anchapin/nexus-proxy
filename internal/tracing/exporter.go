package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ServiceName is the value stamped on every OTLP resource as the
// `service.name` attribute (OTel semantic conventions). Operators
// querying their collector for "show me everything from the
// proxy" can grep for this string.
const ServiceName = "nexus-proxy"

// ScopeName is the OTel instrumentation library name. Operators
// filtering by `otel.library.name` in their backend can match this
// exactly. Mirrors the Go import path so it is greppable from any
// collector log.
const ScopeName = "github.com/anchapin/nexus-proxy/internal/tracing"

// Exporter buffers spans and POSTs them as a single OTLP/JSON batch
// to the configured collector endpoint. Submit never blocks the
// caller — when the buffer is full the span is dropped with a
// warning, matching the non-blocking pattern used by
// telemetry.JSONLRecorder and the quality / judge observers.
//
// A nil *Exporter is a no-op for every method; the chat handler
// treats a nil Tracer as "tracing disabled", which preserves the
// pre-issue-#41 behaviour exactly when the operator leaves
// NEXUS_TRACING_ENDPOINT empty.
type Exporter struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration

	sampler Sampler

	queue chan *Span

	wg     sync.WaitGroup
	closed atomic.Bool
	// dropped is the count of spans shed because the buffer was
	// full. Useful for /metrics gauges and tests that assert
	// non-blocking behaviour under back-pressure.
	dropped atomic.Uint64
}

// ExporterConfig is the input to NewExporter.
type ExporterConfig struct {
	// Endpoint is the full OTLP/JSON HTTP URL, including the
	// `/v1/traces` path. Empty disables tracing entirely
	// (NewExporter returns nil).
	Endpoint string

	// Timeout bounds each POST. <=0 falls back to
	// defaultExporterTimeout.
	Timeout time.Duration

	// QueueSize is the buffered channel capacity. <=0 falls back
	// to defaultExporterQueue. Spans submitted after the buffer
	// is full are dropped with a warning.
	QueueSize int

	// Client is the *http.Client used for POSTs. Nil falls back
	// to a client with the configured Timeout. The exporter wraps
	// any provided client in a copy so its Timeout is honoured
	// without mutating the caller's.
	Client *http.Client

	// Sampler decides whether a trace is recorded. Nil falls back
	// to AlwaysSample. The probability sampler hashes the trace
	// id so the decision is deterministic across processes.
	Sampler Sampler
}

const (
	defaultExporterQueue   = 256
	defaultExporterTimeout = 10 * time.Second
	// batchCap bounds the in-memory batch size — once reached
	// the consumer flushes before draining more spans. Keeps the
	// worst-case body size predictable (256 spans × ~500 B ≈
	// 128 KB, well within OTLP collector defaults).
	batchCap = 64
)

// NewExporter starts the background POST loop and returns a ready
// Exporter. Returns nil (with no goroutine started) when endpoint
// is empty — the chat handler treats a nil exporter as "tracing
// disabled", matching the spec's NEXUS_TRACING_ENDPOINT="" contract.
func NewExporter(cfg ExporterConfig) *Exporter {
	if cfg.Endpoint == "" {
		return nil
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultExporterQueue
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultExporterTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	} else {
		// Don't mutate the caller's client; wrap a copy with the
		// configured timeout so the exporter is the only owner.
		client = &http.Client{
			Transport: client.Transport,
			Timeout:   cfg.Timeout,
		}
	}
	sampler := cfg.Sampler
	if sampler == nil {
		sampler = AlwaysSample{}
	}
	e := &Exporter{
		endpoint: cfg.Endpoint,
		client:   client,
		timeout:  cfg.Timeout,
		sampler:  sampler,
		queue:    make(chan *Span, cfg.QueueSize),
	}
	e.wg.Add(1)
	go e.run()
	return e
}

// Endpoint returns the OTLP/JSON URL the exporter POSTs to. Useful
// for boot logs.
func (e *Exporter) Endpoint() string {
	if e == nil {
		return ""
	}
	return e.endpoint
}

// Dropped returns the cumulative count of spans dropped because the
// buffer was full. Useful for /metrics gauges and tests.
func (e *Exporter) Dropped() uint64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

// Submit enqueues s for asynchronous POST. The call never blocks:
// when the buffer is full s is dropped, the dropped counter is
// incremented, and a warning is logged. A nil receiver or nil span
// is a no-op. Submit is a no-op once Close has been called.
func (e *Exporter) Submit(s *Span) {
	if e == nil || s == nil {
		return
	}
	if e.closed.Load() {
		return
	}
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1)
		slog.Warn("tracing buffer full, dropped span",
			slog.String("name", s.Name),
			slog.String("trace_id", s.TraceID),
		)
	}
}

// StartSpan creates a span bound to this exporter. The span's End
// submits to the exporter's background loop. When sampling decides
// "no" the returned span is a no-op (its SetAttr / End do nothing
// and Submit is skipped) so the caller's hot path is unaffected.
//
// A nil receiver produces a no-op span without sampling; this is
// the path the chat handler takes when tracing is disabled.
func (e *Exporter) StartSpan(parent Context, name string) (Context, *Span) {
	ctx := parent
	if ctx.TraceID == "" {
		ctx.TraceID = NewTraceID()
	}
	sid := NewSpanID()
	s := &Span{
		TraceID:      ctx.TraceID,
		SpanID:       sid,
		ParentSpanID: parent.SpanID,
		Name:         name,
		StartTime:    time.Now(),
		Attributes:   make(map[string]any, 4),
		Status:       StatusUnset,
	}
	if e != nil && e.endpoint != "" && e.sampler.ShouldSample(ctx.TraceID) {
		s.exporter = e
	}
	return ctx.WithSpanID(sid), s
}

// Close signals the background loop to drain and exit. Safe to call
// once; subsequent calls are no-ops. Blocks until the loop returns
// and the final flush attempt completes, so a graceful shutdown
// preserves all queued spans.
//
// A nil receiver returns nil without spawning or waiting on
// anything — matches the NewExporter("") == nil contract.
func (e *Exporter) Close() error {
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

// run is the background consumer. It batches queued spans and
// flushes when the batch reaches batchCap OR when the channel
// closes (final drain on shutdown). Batches that fail to POST log a
// warning but are otherwise dropped — the spec mandates
// non-blocking semantics on the request path, so the export loop
// must never queue unbounded retries.
func (e *Exporter) run() {
	defer e.wg.Done()
	batch := make([]*Span, 0, batchCap)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := e.flush(batch); err != nil {
			slog.Warn("tracing flush failed",
				slog.String("endpoint", e.endpoint),
				slog.Int("count", len(batch)),
				slog.Any("err", err),
			)
		}
		batch = batch[:0]
	}
	for s := range e.queue {
		batch = append(batch, s)
		if len(batch) >= batchCap {
			flush()
		}
	}
	flush()
}

// flush POSTs one batch as OTLP/JSON. The body shape matches the
// OTel collector's expected JSON envelope (resourceSpans[] /
// scopeSpans[] / spans[]). On error the batch is dropped — the
// run loop logs the failure and moves on.
func (e *Exporter) flush(batch []*Span) error {
	if len(batch) == 0 {
		return nil
	}
	body, err := buildOTLPJSON(batch)
	if err != nil {
		return fmt.Errorf("tracing: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("tracing: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("tracing: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("tracing: collector status %d", resp.StatusCode)
	}
	return nil
}

// --- OTLP/JSON envelope ----------------------------------------------------
//
// The OTLP HTTP/JSON schema wraps every span inside a
// resourceSpans[] / scopeSpans[] envelope. We attach one
// service.name="nexus-proxy" resource and one scopeSpans entry per
// batch. See:
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// Every numeric / bool / string value goes through encodeAttr so
// the JSON shape stays strictly typed — collectors reject
// untyped values.

type otlpAttrValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *int64   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpAttr struct {
	Key   string        `json:"key"`
	Value otlpAttrValue `json:"value"`
}

type otlpStatus struct {
	Code        string `json:"code"`
	Description string `json:"description,omitempty"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              string     `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes,omitempty"`
	Status            otlpStatus `json:"status"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResource struct {
	Attributes []otlpAttr `json:"attributes"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// buildOTLPJSON marshals the supplied spans into the OTLP/JSON
// envelope. The function is pure — safe to call from the export
// goroutine without locking.
func buildOTLPJSON(spans []*Span) ([]byte, error) {
	svc := ServiceName
	rs := otlpResourceSpans{
		Resource: otlpResource{
			Attributes: []otlpAttr{{
				Key:   "service.name",
				Value: otlpAttrValue{StringValue: &svc},
			}},
		},
	}
	ss := otlpScopeSpans{
		Scope: otlpScope{Name: ScopeName},
		Spans: make([]otlpSpan, 0, len(spans)),
	}
	for _, s := range spans {
		ss.Spans = append(ss.Spans, toOTLPSpan(s))
	}
	rs.ScopeSpans = []otlpScopeSpans{ss}
	payload := otlpPayload{ResourceSpans: []otlpResourceSpans{rs}}
	return json.Marshal(payload)
}

// toOTLPSpan converts a Span into the OTLP wire shape. Time values
// are rendered as decimal nanoseconds per the OTLP spec; status
// codes mirror the enum string set (so collectors that fail to
// parse them at least get a recognisable name).
func toOTLPSpan(s *Span) otlpSpan {
	out := otlpSpan{
		TraceID:           s.TraceID,
		SpanID:            s.SpanID,
		ParentSpanID:      s.ParentSpanID,
		Name:              s.Name,
		Kind:              "SPAN_KIND_INTERNAL",
		StartTimeUnixNano: fmt.Sprintf("%d", s.StartTime.UnixNano()),
		EndTimeUnixNano:   fmt.Sprintf("%d", s.EndTime.UnixNano()),
	}
	switch s.Status {
	case StatusOK:
		out.Status = otlpStatus{Code: "STATUS_CODE_OK"}
	case StatusError:
		out.Status = otlpStatus{Code: "STATUS_CODE_ERROR", Description: s.StatusMessage}
	default:
		out.Status = otlpStatus{Code: "STATUS_CODE_UNSET"}
	}
	if len(s.Attributes) > 0 {
		out.Attributes = make([]otlpAttr, 0, len(s.Attributes))
		for k, v := range s.Attributes {
			out.Attributes = append(out.Attributes, otlpAttr{
				Key:   k,
				Value: encodeAttr(v),
			})
		}
	}
	return out
}

// encodeAttr renders an attribute value as the strict OTLP/JSON
// typed shape. Unsupported types fall back to a string
// representation so a stray int / struct does not produce an
// invalid body.
func encodeAttr(v any) otlpAttrValue {
	var out otlpAttrValue
	switch x := v.(type) {
	case string:
		out.StringValue = &x
	case bool:
		out.BoolValue = &x
	case int:
		n := int64(x)
		out.IntValue = &n
	case int8:
		n := int64(x)
		out.IntValue = &n
	case int16:
		n := int64(x)
		out.IntValue = &n
	case int32:
		n := int64(x)
		out.IntValue = &n
	case int64:
		out.IntValue = &x
	case uint:
		n := int64(x)
		out.IntValue = &n
	case uint8:
		n := int64(x)
		out.IntValue = &n
	case uint16:
		n := int64(x)
		out.IntValue = &n
	case uint32:
		n := int64(x)
		out.IntValue = &n
	case uint64:
		if x <= 1<<63-1 {
			n := int64(x)
			out.IntValue = &n
		} else {
			f := float64(x)
			out.DoubleValue = &f
		}
	case float32:
		f := float64(x)
		out.DoubleValue = &f
	case float64:
		out.DoubleValue = &x
	default:
		// Fallback: stringify via fmt so an unknown type still
		// renders something the collector can index. Operators
		// adding new attribute types should extend the switch
		// above.
		s := fmt.Sprintf("%v", v)
		out.StringValue = &s
	}
	return out
}

// --- Sampling -------------------------------------------------------------

// Sampler decides whether a given trace id should be recorded. The
// decision is made once at the root span; child spans inherit the
// result by virtue of sharing the trace id.
type Sampler interface {
	ShouldSample(traceID string) bool
}

// AlwaysSample records every trace. Used when SampleRate >= 1.0 or
// when the operator has not configured a sampler.
type AlwaysSample struct{}

// ShouldSample returns true unconditionally.
func (AlwaysSample) ShouldSample(string) bool { return true }

// NeverSample drops every trace. Used when SampleRate <= 0 — the
// chat handler treats this case as "tracing fully disabled".
type NeverSample struct{}

// ShouldSample returns false unconditionally.
func (NeverSample) ShouldSample(string) bool { return false }

// ProbabilitySampler records a deterministic fraction of traces. The
// decision is hash-based on the trace id so multiple replicas
// (or multiple processes) all agree on whether to record a given
// trace — important for distributed correlation.
//
// rate is clamped to [0, 1]. rate<=0 yields NeverSample; rate>=1
// yields AlwaysSample.
type ProbabilitySampler struct {
	rate float64
}

// NewProbabilitySampler returns a ProbabilitySampler at the given
// rate. Negative values clamp to 0 (never); values >= 1 collapse to
// AlwaysSample for the cheap path.
func NewProbabilitySampler(rate float64) Sampler {
	if rate <= 0 {
		return NeverSample{}
	}
	if rate >= 1 {
		return AlwaysSample{}
	}
	return ProbabilitySampler{rate: rate}
}

// ShouldSample returns true for ~rate fraction of distinct trace ids.
// Uses FNV-1a on the lowercase hex trace id so the decision is
// stable across processes.
func (p ProbabilitySampler) ShouldSample(traceID string) bool {
	if traceID == "" {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(traceID))
	// Map the 32-bit hash to [0, 1) and compare against rate.
	// float64 math.MaxUint32 fits exactly in a float64 mantissa,
	// so the boundary values 0 and 1 behave predictably.
	return float64(h.Sum32())/float64(^uint32(0)) < p.rate
}
