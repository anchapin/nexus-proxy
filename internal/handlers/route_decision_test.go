package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// routeDecisionRecorder is a RouteDecisionObserver test double that
// appends every observed event to a slice so the assertions can read
// the captured route-decision metadata. Issue #74: the observer hook
// is what propagates planner metadata to the in-process route
// counters and any other downstream instrumentation.
type routeDecisionRecorder struct {
	mu     sync.Mutex
	events []RouteDecisionEvent
}

func (r *routeDecisionRecorder) Observe(e RouteDecisionEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *routeDecisionRecorder) snapshot() []RouteDecisionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RouteDecisionEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestChatSetsRouteDecisionHeadersForDSLMatch is the smoke test for
// issue #74's response-header surface: every proxied request must
// carry X-Nexus-Route, X-Nexus-Route-Source, X-Nexus-Route-Reason,
// and X-Nexus-Route-Confidence. A DSL-triggered local route is the
// cheapest deterministic path — no SLM HTTP stub needed.
func TestChatSetsRouteDecisionHeadersForDSLMatch(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.RouteDecisionObserver = &routeDecisionRecorder{}

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}

	if got := rw.Header().Get("X-Nexus-Route"); got != "local" {
		t.Errorf("X-Nexus-Route = %q, want \"local\"", got)
	}
	if got := rw.Header().Get("X-Nexus-Route-Source"); got != "dsl" {
		t.Errorf("X-Nexus-Route-Source = %q, want \"dsl\"", got)
	}
	// DSL path leaves Reason empty; the header carries the sanitised
	// empty value rather than being omitted, so consumers can rely on
	// the header always being set.
	if got := rw.Header().Get("X-Nexus-Route-Reason"); got != "" {
		t.Errorf("X-Nexus-Route-Reason = %q, want \"\"", got)
	}
	// DSL bypasses the SLM, so the planner emits the neutral
	// confidence floor (0.50) which surfaces on the header.
	if got := rw.Header().Get("X-Nexus-Route-Confidence"); got != "0.50" {
		t.Errorf("X-Nexus-Route-Confidence = %q, want \"0.50\"", got)
	}

	rec := deps.RouteDecisionObserver.(*routeDecisionRecorder)
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("RouteDecisionObserver events = %d, want 1", len(events))
	}
	e := events[0]
	if e.Route != "local" || e.Source != "dsl" || e.Confidence != 0.50 {
		t.Errorf("observer event = %+v, want route=local source=dsl confidence=0.50", e)
	}
	if e.RequestID == "" {
		t.Error("observer event missing request id")
	}
}

// TestChatSetsRouteDecisionHeadersForGuardrail exercises the
// guardrail-forced-frontier path and confirms the Reason="vram"
// surface. Uses the 30k-char prompt fixture from the existing
// guardrail test so we know the guardrail will trip.
func TestChatSetsRouteDecisionHeadersForGuardrail(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.RouteDecisionObserver = &routeDecisionRecorder{}

	largeUser := strings.Repeat("a", 49000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "frontier stream")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if got := rw.Header().Get("X-Nexus-Route-Source"); got != "guardrail" {
		t.Errorf("X-Nexus-Route-Source = %q, want \"guardrail\"", got)
	}
	if got := rw.Header().Get("X-Nexus-Route-Reason"); got != "vram" {
		t.Errorf("X-Nexus-Route-Reason = %q, want \"vram\"", got)
	}
	if got := rw.Header().Get("X-Nexus-Route"); got != "frontier" {
		t.Errorf("X-Nexus-Route = %q, want \"frontier\"", got)
	}

	rec := deps.RouteDecisionObserver.(*routeDecisionRecorder)
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("observer events = %d, want 1", len(events))
	}
	if events[0].Reason != "vram" || events[0].Source != "guardrail" || events[0].Route != "frontier" {
		t.Errorf("observer event = %+v, want reason=vram source=guardrail route=frontier", events[0])
	}
}

// TestChatRouteDecisionObserverNilIsSafe confirms the
// RouteDecisionObserver hook is optional — leaving it nil must not
// panic and must not change the headers on the response.
func TestChatRouteDecisionObserverNilIsSafe(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.RouteDecisionObserver = nil

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()

	// Must not panic.
	Chat(deps).ServeHTTP(rw, req)

	if got := rw.Header().Get("X-Nexus-Route-Source"); got != "dsl" {
		t.Errorf("X-Nexus-Route-Source = %q, want \"dsl\" (nil observer should not affect headers)", got)
	}
}

// TestChatFormatConfidenceBounds clamps out-of-range floats so a
// runaway SLM cannot produce an unwieldy header. Boundary checks for
// 0 (no SLM confidence) and 1 (full confidence) plus a quick negative
// sweep.
func TestChatFormatConfidence(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00"},
		{0.5, "0.50"},
		{0.123, "0.12"},
		{0.999, "1.00"},
		{1, "1.00"},
		{-0.5, "0.00"},
		{2.5, "1.00"},
	}
	for _, c := range cases {
		if got := formatConfidence(c.in); got != c.want {
			t.Errorf("formatConfidence(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// recordingMetricsObserver captures every MetricsEvent so the test
// can assert the issue #74 fields (RouteSource / RouteReason /
// SLMConfidence / SLMTaskType) propagate from the planner through the
// handler. Mirrors the existing recorder patterns in chat_test.go.
type recordingMetricsObserver struct {
	mu     sync.Mutex
	events []MetricsEvent
}

func (r *recordingMetricsObserver) Submit(e MetricsEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recordingMetricsObserver) snapshot() []MetricsEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MetricsEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestChatMetricsEventCarriesRouteDecisionFields verifies the
// MetricsEvent observer hook receives the issue #74 fields populated
// from the planner Decision (guardrail path is deterministic — no
// SLM stub needed).
func TestChatMetricsEventCarriesRouteDecisionFields(t *testing.T) {
	deps, rt := baseDeps(t)
	obs := &recordingMetricsObserver{}
	deps.MetricsObserver = obs

	largeUser := strings.Repeat("a", 49000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "frontier stream")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	events := obs.snapshot()
	if len(events) != 1 {
		t.Fatalf("metrics observer events = %d, want 1", len(events))
	}
	e := events[0]
	if e.RouteSource != "guardrail" {
		t.Errorf("RouteSource = %q, want \"guardrail\"", e.RouteSource)
	}
	if e.RouteReason != "vram" {
		t.Errorf("RouteReason = %q, want \"vram\"", e.RouteReason)
	}
	if e.SLMConfidence != 0.5 {
		t.Errorf("SLMConfidence = %v, want 0.5", e.SLMConfidence)
	}
}

// recordingRecorder is a telemetry.Recorder test double used to
// verify the issue #74 fields reach the legacy JSONLRecorder-shaped
// sink. SQLite uses the same Record, so testing here proves both
// paths get the metadata.
type recordingRecorder struct {
	mu      sync.Mutex
	records []telemetry.Record
}

func (r *recordingRecorder) Record(rec telemetry.Record) {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
}
func (r *recordingRecorder) Close() error { return nil }
func (r *recordingRecorder) snapshot() []telemetry.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]telemetry.Record, len(r.records))
	copy(out, r.records)
	return out
}

// TestChatRecorderReceivesRouteDecisionFields walks the DSL path and
// confirms the legacy recorder (the path the sqlite store also reads
// from) carries the issue #74 fields.
func TestChatRecorderReceivesRouteDecisionFields(t *testing.T) {
	deps, rt := baseDeps(t)
	rec := &recordingRecorder{}
	deps.Recorder = rec
	// Wipe the MetricsObserver so the dispatch falls through to the
	// recorder.
	deps.MetricsObserver = nil

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("recorder rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.RouteSource != "dsl" {
		t.Errorf("RouteSource = %q, want \"dsl\"", got.RouteSource)
	}
	// DSL bypasses the SLM, so the planner emits neutral 0.5.
	if got.SLMConfidence != 0.5 {
		t.Errorf("SLMConfidence = %v, want 0.5", got.SLMConfidence)
	}
}

// TestSanitizeHeaderValueAdversarial exercises the route-decision
// header sanitiser against payload strings designed to inject CRLF,
// nulls, or extreme length (issue #74). The sanitiser is the
// boundary between the planner's Decision (which can carry
// attacker-influenced text via the SLM error path) and the wire
// header block.
func TestSanitizeHeaderValueAdversarial(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"clean", "frontier", "frontier"},
		{"crlf", "a\r\nb", "ab"},
		{"only_newline", "\n", ""},
		{"low_control", "a\x01b\x02c", "a b c"},
		{"pathological_long", strings.Repeat("x", MaxHeaderValue*4), strings.Repeat("x", MaxHeaderValue)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeHeaderValue(c.in); got != c.want {
				t.Errorf("SanitizeHeaderValue(%q) = %q (len %d), want %q (len %d)",
					c.in, got, len(got), c.want, len(c.want))
			}
		})
	}
}

// TestRouteDecisionObserverFuncAdapter exercises the func adapter so
// the closure wiring in cmd/nexus stays a one-liner.
func TestRouteDecisionObserverFuncAdapter(t *testing.T) {
	var captured RouteDecisionEvent
	RouteDecisionObserverFunc(func(e RouteDecisionEvent) { captured = e }).Observe(RouteDecisionEvent{
		RequestID:  "r-1",
		Route:      "frontier",
		Source:     "guardrail",
		Reason:     "vram",
		Confidence: 0.5,
		TaskType:   "",
	})
	if captured.RequestID != "r-1" || captured.Route != "frontier" || captured.Source != "guardrail" {
		t.Errorf("adapter dropped or mangled event: %+v", captured)
	}
}
