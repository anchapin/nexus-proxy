package tracing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewExporterEmptyEndpointDisables(t *testing.T) {
	// Spec acceptance criterion: NEXUS_TRACING_ENDPOINT="" disables
	// tracing entirely (no goroutine, no allocation beyond nil-check).
	e := NewExporter(ExporterConfig{Endpoint: ""})
	if e != nil {
		t.Fatalf("NewExporter(\"\") = %v, want nil", e)
	}
	// Methods on the nil exporter must be no-ops.
	e.StartSpan(Context{}, "x") // must not panic
	e.Submit(&Span{})           // must not panic
	if err := e.Close(); err != nil {
		t.Errorf("Close on nil = %v, want nil", err)
	}
}

func TestNewExporterStarts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewExporter(ExporterConfig{
		Endpoint:  srv.URL + "/v1/traces",
		QueueSize: 4,
	})
	if e == nil {
		t.Fatal("NewExporter returned nil for non-empty endpoint")
	}
	defer e.Close()

	_, s := e.StartSpan(Context{TraceID: NewTraceID()}, "test")
	s.SetAttr("hello", "world")
	s.End()

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("collector received no requests")
	}
	if e.Endpoint() != srv.URL+"/v1/traces" {
		t.Errorf("Endpoint() = %q", e.Endpoint())
	}
}

func TestExporterNonBlockingOnFullBuffer(t *testing.T) {
	// Build a server that hangs forever so the export loop never
	// drains the buffer. Submit must drop, not block.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	e := NewExporter(ExporterConfig{
		Endpoint:  srv.URL,
		QueueSize: 1,
	})
	defer e.Close()

	// Saturate the queue: the export goroutine reads items faster
	// than a tight submit loop can fill it, so we issue many spans
	// in parallel to force overflow on the contended buffer slot.
	const total = 200
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			_, s := e.StartSpan(Context{TraceID: NewTraceID()}, "overflow")
			s.End()
		}()
	}
	wg.Wait()
	if e.Dropped() == 0 {
		t.Errorf("Dropped() = 0, want > 0 under contention (queue=1, %d submits)", total)
	}
}

func TestExporterSampling(t *testing.T) {
	var hits atomic.Int32
	var spans atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		// Count spans actually received, not POSTs. The exporter
		// batches so a single POST may carry many spans.
		var payload otlpPayload
		if json.Unmarshal(body, &payload) == nil {
			for _, rs := range payload.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					spans.Add(int32(len(ss.Spans)))
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// SampleRate = 0 must disable every trace.
	e0 := NewExporter(ExporterConfig{Endpoint: srv.URL, Sampler: NewProbabilitySampler(0)})
	for i := 0; i < 32; i++ {
		_, s := e0.StartSpan(Context{TraceID: NewTraceID()}, "op")
		s.End()
	}
	if err := e0.Close(); err != nil {
		t.Fatalf("e0 Close: %v", err)
	}
	if spans.Load() != 0 {
		t.Errorf("spans with rate=0 = %d, want 0", spans.Load())
	}

	// SampleRate = 1 must record every trace.
	hits.Store(0)
	spans.Store(0)
	e1 := NewExporter(ExporterConfig{Endpoint: srv.URL, Sampler: NewProbabilitySampler(1)})
	for i := 0; i < 8; i++ {
		_, s := e1.StartSpan(Context{TraceID: NewTraceID()}, "op")
		s.End()
	}
	if err := e1.Close(); err != nil {
		t.Fatalf("e1 Close: %v", err)
	}
	if spans.Load() != 8 {
		t.Errorf("spans with rate=1 = %d, want 8 (hits=%d)", spans.Load(), hits.Load())
	}
	if hits.Load() == 0 {
		t.Errorf("expected at least one POST when rate=1")
	}
}

func TestExporterSamplingRate(t *testing.T) {
	// ProbabilitySampler at rate=0.5 must record roughly half the
	// trace ids. With 1000 ids the sample should be in [400, 600]
	// with overwhelming probability (binomial, n=1000, p=0.5).
	s := NewProbabilitySampler(0.5)
	const n = 1000
	var picked int
	for i := 0; i < n; i++ {
		if s.ShouldSample(NewTraceID()) {
			picked++
		}
	}
	if picked < 400 || picked > 600 {
		t.Errorf("sampling rate 0.5 picked %d/%d, want ~500", picked, n)
	}
}

func TestExporterSamplingDeterministic(t *testing.T) {
	// The sampler MUST be deterministic across calls with the same
	// trace id — otherwise distributed traces would lose spans
	// depending on which replica made the decision.
	s := NewProbabilitySampler(0.3)
	id := "0af7651916cd43dd8448eb211c80319c"
	first := s.ShouldSample(id)
	for i := 0; i < 10; i++ {
		if s.ShouldSample(id) != first {
			t.Errorf("sampler not deterministic for %q", id)
		}
	}
}

func TestProbabilitySamplerClamps(t *testing.T) {
	if _, ok := NewProbabilitySampler(-0.1).(NeverSample); !ok {
		t.Errorf("negative rate should return NeverSample")
	}
	if _, ok := NewProbabilitySampler(1.5).(AlwaysSample); !ok {
		t.Errorf("rate>=1 should return AlwaysSample")
	}
	if NewProbabilitySampler(0).ShouldSample("abc") {
		t.Errorf("rate=0 sampled abc")
	}
	if !NewProbabilitySampler(1).ShouldSample("abc") {
		t.Errorf("rate=1 dropped abc")
	}
}

func TestExporterOTLPBodyShape(t *testing.T) {
	// Verify the OTLP/JSON envelope: resourceSpans[] / scopeSpans[]
	// / spans[] with the documented field names. Collectors are
	// strict about the schema — any drift here breaks export.
	var raw []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		raw, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewExporter(ExporterConfig{Endpoint: srv.URL})
	defer e.Close()

	traceID := "0af7651916cd43dd8448eb211c80319c"
	parent := Context{TraceID: traceID, SpanID: "b7ad6b7169203331"}
	_, root := e.StartSpan(parent, "nexus.chat_completions")
	root.SetAttr("route", "frontier")
	root.SetAttr("ttft_ms", int64(120))
	root.End()

	// Allow batch flush (batchCap=64 so we must Close to drain).
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(raw) == 0 {
		t.Fatal("collector received no body")
	}
	var payload otlpPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, raw)
	}
	if len(payload.ResourceSpans) != 1 {
		t.Fatalf("resourceSpans = %d, want 1", len(payload.ResourceSpans))
	}
	rs := payload.ResourceSpans[0]
	if len(rs.ScopeSpans) != 1 {
		t.Fatalf("scopeSpans = %d, want 1", len(rs.ScopeSpans))
	}
	ss := rs.ScopeSpans[0]
	if ss.Scope.Name != ScopeName {
		t.Errorf("scope name = %q, want %q", ss.Scope.Name, ScopeName)
	}
	if len(ss.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(ss.Spans))
	}
	sp := ss.Spans[0]
	if sp.TraceID != traceID {
		t.Errorf("traceId = %q, want %q", sp.TraceID, traceID)
	}
	if sp.ParentSpanID != "b7ad6b7169203331" {
		t.Errorf("parentSpanId = %q, want b7ad6b7169203331", sp.ParentSpanID)
	}
	if sp.Name != "nexus.chat_completions" {
		t.Errorf("name = %q", sp.Name)
	}
	if sp.Kind != "SPAN_KIND_INTERNAL" {
		t.Errorf("kind = %q, want SPAN_KIND_INTERNAL", sp.Kind)
	}
	if sp.Status.Code != "STATUS_CODE_OK" {
		t.Errorf("status code = %q, want STATUS_CODE_OK", sp.Status.Code)
	}
	if sp.StartTimeUnixNano == "" || sp.EndTimeUnixNano == "" {
		t.Errorf("missing timestamps: start=%q end=%q", sp.StartTimeUnixNano, sp.EndTimeUnixNano)
	}
	// Attributes: route (string), ttft_ms (int).
	var sawRoute, sawTTFT bool
	for _, a := range sp.Attributes {
		switch a.Key {
		case "route":
			if a.Value.StringValue == nil || *a.Value.StringValue != "frontier" {
				t.Errorf("route attr = %+v", a.Value)
			}
			sawRoute = true
		case "ttft_ms":
			if a.Value.IntValue == nil || *a.Value.IntValue != 120 {
				t.Errorf("ttft_ms attr = %+v", a.Value)
			}
			sawTTFT = true
		}
	}
	if !sawRoute || !sawTTFT {
		t.Errorf("attributes missing: route=%v ttft=%v attrs=%+v", sawRoute, sawTTFT, sp.Attributes)
	}
	// Service name on the resource.
	var sawService bool
	for _, a := range rs.Resource.Attributes {
		if a.Key == "service.name" && a.Value.StringValue != nil && *a.Value.StringValue == ServiceName {
			sawService = true
		}
	}
	if !sawService {
		t.Errorf("service.name attribute missing: %+v", rs.Resource.Attributes)
	}
}

func TestExporterErrorPropagatesFromCollector(t *testing.T) {
	// A 500 from the collector must NOT block the request path;
	// it is logged and dropped inside the export goroutine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewExporter(ExporterConfig{Endpoint: srv.URL})
	defer e.Close()
	_, s := e.StartSpan(Context{TraceID: NewTraceID()}, "op")
	s.End()
	// Close waits for the background goroutine; a 500 just logs.
	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestExporterCloseIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewExporter(ExporterConfig{Endpoint: srv.URL})
	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Submit after Close is a no-op.
	_, s := e.StartSpan(Context{TraceID: NewTraceID()}, "op")
	s.End() // must not panic
}

func TestExporterStartSpanAttachesToParent(t *testing.T) {
	e := NewExporter(ExporterConfig{Endpoint: "http://unused"})
	defer e.Close()
	parent := Context{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331"}
	ctx, s := e.StartSpan(parent, "child")
	if ctx.TraceID != parent.TraceID {
		t.Errorf("trace = %q", ctx.TraceID)
	}
	if s.ParentSpanID != parent.SpanID {
		t.Errorf("parent = %q", s.ParentSpanID)
	}
}

func TestExporterStartSpanZeroParent(t *testing.T) {
	e := NewExporter(ExporterConfig{Endpoint: "http://unused"})
	defer e.Close()
	ctx, s := e.StartSpan(Context{}, "root")
	if ctx.TraceID == "" {
		t.Error("zero parent should still produce a trace id")
	}
	if s.ParentSpanID != "" {
		t.Errorf("root parent = %q, want empty", s.ParentSpanID)
	}
}

func TestExporterNoopExports(t *testing.T) {
	// Submit on a nil exporter must drop silently.
	var e *Exporter
	e.Submit(nil)
	e.Submit(&Span{Name: "x"})
	if e.Dropped() != 0 {
		t.Errorf("nil exporter Dropped() = %d", e.Dropped())
	}
}

func TestEncodeAttrTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		// check runs on the encoded value to assert the right
		// JSON type was used.
		check func(t *testing.T, v otlpAttrValue)
	}{
		{"string", "hello", func(t *testing.T, v otlpAttrValue) {
			if v.StringValue == nil || *v.StringValue != "hello" {
				t.Errorf("got %+v", v)
			}
		}},
		{"bool", true, func(t *testing.T, v otlpAttrValue) {
			if v.BoolValue == nil || !*v.BoolValue {
				t.Errorf("got %+v", v)
			}
		}},
		{"int", 7, func(t *testing.T, v otlpAttrValue) {
			if v.IntValue == nil || *v.IntValue != 7 {
				t.Errorf("got %+v", v)
			}
		}},
		{"float", 1.5, func(t *testing.T, v otlpAttrValue) {
			if v.DoubleValue == nil || *v.DoubleValue != 1.5 {
				t.Errorf("got %+v", v)
			}
		}},
		{"unknown", struct{ X int }{X: 1}, func(t *testing.T, v otlpAttrValue) {
			if v.StringValue == nil || !strings.Contains(*v.StringValue, "{") {
				t.Errorf("expected struct fallback to string, got %+v", v)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, encodeAttr(tc.in))
		})
	}
}

// Compile-time guard that Exporter satisfies any expected interface.
var _ io.Closer = (*Exporter)(nil)

func TestExporterPerCallTimeout(t *testing.T) {
	// A stalled collector must NOT block Close indefinitely — the
	// per-call context cancels the in-flight POST.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	e := NewExporter(ExporterConfig{
		Endpoint: srv.URL,
		Timeout:  50 * time.Millisecond,
	})
	defer e.Close()
	_, s := e.StartSpan(Context{TraceID: NewTraceID()}, "op")
	s.End()

	// Close should return promptly (well under the 50ms per-call
	// timeout * 64-spans-in-batch — in practice one batch flush).
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ctx
		done <- e.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked longer than 2s")
	}
}
