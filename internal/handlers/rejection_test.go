package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// rejectionRecorder is a RejectionObserver test double that captures
// every dispatched RejectionEvent so assertions can read the reason
// labels that would flow to nexus_requests_rejected_total{reason}.
type rejectionRecorder struct {
	mu     sync.Mutex
	events []RejectionEvent
}

func (r *rejectionRecorder) ObserveRejection(e RejectionEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *rejectionRecorder) snapshot() []RejectionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RejectionEvent, len(r.events))
	copy(out, r.events)
	return out
}

// envelope captures the OpenAI-style error envelope shape
// (`{"error":{"message":...,"type":...,"code":...}}`) so envelope
// conformance tests can decode the body in one step.
type envelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// decodeEnvelope parses rw.Body() as an OpenAI-style error envelope
// and t.Fatals on any decoding failure so the per-test assertions
// can rely on a non-empty struct.
func decodeEnvelope(t *testing.T, rw *httptest.ResponseRecorder) envelope {
	t.Helper()
	if got := rw.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json (issue #453)", got)
	}
	var env envelope
	if err := json.Unmarshal(rw.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not a JSON envelope: %v body=%q", err, rw.Body.String())
	}
	if env.Error.Message == "" {
		t.Errorf("envelope error.message is empty (body=%q)", rw.Body.String())
	}
	if env.Error.Type == "" {
		t.Errorf("envelope error.type is empty (body=%q)", rw.Body.String())
	}
	return env
}

// rejectingSpendGuard is a SpendGuard test double that always rejects
// (returns true from Check) without ever recording spend. It lets the
// 429 budget path be exercised without spinning up the real budget
// tracker from internal/budget.
type rejectingSpendGuard struct{}

func (rejectingSpendGuard) Check(_ context.Context, _ float64) bool       { return true }
func (rejectingSpendGuard) Record(_ context.Context, _ float64, _ string) {}

// TestChatRejectionMethod verifies the 405 path fires the
// RejectionObserver with reason="method" and emits one telemetry
// record (issue #119).
func TestChatRejectionMethod(t *testing.T) {
	deps, _ := baseDeps(t)
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr
	rec := &recordingRecorder{}
	deps.Recorder = rec
	deps.MetricsObserver = nil // force the Recorder path

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rw.Code)
	}
	events := rr.snapshot()
	if len(events) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(events))
	}
	if events[0].Reason != RejectionMethod {
		t.Errorf("reason = %q, want %q", events[0].Reason, RejectionMethod)
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("recorder rows = %d, want 1", len(rows))
	}
	if rows[0].Route != "rejected" {
		t.Errorf("record route = %q, want \"rejected\"", rows[0].Route)
	}
	if rows[0].Error != RejectionMethod {
		t.Errorf("record error = %q, want %q", rows[0].Error, RejectionMethod)
	}
}

// TestChatRejectionBodyTooLarge verifies the 413 path fires the
// RejectionObserver with reason="body_too_large" and emits an
// OpenAI-style JSON envelope (issue #453).
func TestChatRejectionBodyTooLarge(t *testing.T) {
	deps, _ := baseDeps(t)
	deps.Config.MaxBodyBytes = 64 // tiny cap
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr

	huge := strings.Repeat("x", 10_000)
	body := `{"messages":[{"role":"user","content":"` + huge + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rw.Code, rw.Body.String())
	}
	events := rr.snapshot()
	if len(events) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(events))
	}
	if events[0].Reason != RejectionBodyTooLarge {
		t.Errorf("reason = %q, want %q", events[0].Reason, RejectionBodyTooLarge)
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeRequestTooLarge {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeRequestTooLarge)
	}
	if env.Error.Code == "" {
		t.Errorf("envelope code is empty (body=%q)", rw.Body.String())
	}
}

// TestChatRejectionBadJSON verifies the 400 invalid-JSON path fires
// the RejectionObserver with reason="bad_request" and emits an
// OpenAI-style JSON envelope (issue #453).
func TestChatRejectionBadJSON(t *testing.T) {
	deps, _ := baseDeps(t)
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("not json at all"))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rw.Code)
	}
	events := rr.snapshot()
	if len(events) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(events))
	}
	if events[0].Reason != RejectionBadRequest {
		t.Errorf("reason = %q, want %q", events[0].Reason, RejectionBadRequest)
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeInvalidRequest {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeInvalidRequest)
	}
}

// TestChatRejectionMissingMessages verifies the 400 missing-messages
// path fires the RejectionObserver with reason="bad_request" and
// emits an OpenAI-style JSON envelope (issue #453).
func TestChatRejectionMissingMessages(t *testing.T) {
	deps, _ := baseDeps(t)
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"foo":"bar"}`))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rw.Code)
	}
	events := rr.snapshot()
	if len(events) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(events))
	}
	if events[0].Reason != RejectionBadRequest {
		t.Errorf("reason = %q, want %q", events[0].Reason, RejectionBadRequest)
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeInvalidRequest {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeInvalidRequest)
	}
}

// TestChatRejectionMethodEnvelope covers the 405 path with the new
// JSON envelope (issue #453): status 405, content-type application/json,
// and error.type == "method_not_allowed".
func TestChatRejectionMethodEnvelope(t *testing.T) {
	deps, _ := baseDeps(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rw.Code)
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeMethodNotAllowed {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeMethodNotAllowed)
	}
}

// TestChatRejectionUsesMetricsObserver confirms that when a
// MetricsObserver is wired, the rejection is dispatched to it (with
// route="rejected"). With the fix for issue #164 both the
// MetricsObserver and the Recorder receive events when both are wired.
func TestChatRejectionUsesMetricsObserver(t *testing.T) {
	deps, _ := baseDeps(t)
	mo := &recordingMetricsObserver{}
	deps.MetricsObserver = mo
	deps.Recorder = &recordingRecorder{}
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	events := mo.snapshot()
	if len(events) != 1 {
		t.Fatalf("metrics events = %d, want 1", len(events))
	}
	if events[0].Route != "rejected" {
		t.Errorf("metrics route = %q, want \"rejected\"", events[0].Route)
	}
	if events[0].Error != RejectionMethod {
		t.Errorf("metrics error = %q, want %q", events[0].Error, RejectionMethod)
	}
	// Recorder also receives the rejection (issue #164 fix).
	rows := deps.Recorder.(*recordingRecorder).snapshot()
	if len(rows) != 1 {
		t.Fatalf("recorder rows = %d, want 1", len(rows))
	}
	if rows[0].Route != "rejected" {
		t.Errorf("record route = %q, want \"rejected\"", rows[0].Route)
	}
	if rows[0].Error != RejectionMethod {
		t.Errorf("record error = %q, want %q", rows[0].Error, RejectionMethod)
	}
}

// TestChatRejectionObserverNilSafe confirms the rejection recording
// closure does not panic when no observer is wired.
func TestChatRejectionObserverNilSafe(t *testing.T) {
	deps, _ := baseDeps(t)
	deps.RejectionObserver = nil
	deps.Recorder = telemetry.Noop{}

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req) // must not panic
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rw.Code)
	}
}

// TestRejectionObserverFuncAdapter exercises the func adapter so the
// wiring in main.go (RejectionObserverFunc) is covered by a unit test.
func TestRejectionObserverFuncAdapter(t *testing.T) {
	var got RejectionEvent
	f := RejectionObserverFunc(func(e RejectionEvent) { got = e })
	f.ObserveRejection(RejectionEvent{RequestID: "req-1", Reason: RejectionMethod})
	if got.RequestID != "req-1" || got.Reason != RejectionMethod {
		t.Errorf("adapter captured %+v", got)
	}
}

// TestRejectionQueueConstants verifies the queue overflow rejection
// constants have the expected string values (issue #168).
func TestRejectionQueueConstants(t *testing.T) {
	if RejectionJudgeQueue != "judge_queue" {
		t.Errorf("RejectionJudgeQueue = %q, want %q", RejectionJudgeQueue, "judge_queue")
	}
	if RejectionQualityQueue != "quality_queue" {
		t.Errorf("RejectionQualityQueue = %q, want %q", RejectionQualityQueue, "quality_queue")
	}
}

// TestRejectionObserverFuncQueueOverflow exercises the RejectionObserverFunc
// adapter with the queue overflow reasons so the wiring in main.go is covered.
func TestRejectionObserverFuncQueueOverflow(t *testing.T) {
	for _, reason := range []string{RejectionJudgeQueue, RejectionQualityQueue} {
		var got RejectionEvent
		f := RejectionObserverFunc(func(e RejectionEvent) { got = e })
		f.ObserveRejection(RejectionEvent{RequestID: "req-queue", Reason: reason})
		if got.RequestID != "req-queue" || got.Reason != reason {
			t.Errorf("adapter captured %+v for reason %q", got, reason)
		}
	}
}

// TestChatRejectionBudgetEnvelope covers the 429 budget-exhausted
// path (issue #220) and verifies the body is now an OpenAI-style
// JSON envelope (issue #453).
func TestChatRejectionBudgetEnvelope(t *testing.T) {
	deps, _ := baseDeps(t)
	deps.SpendGuard = rejectingSpendGuard{}
	// FrontierCostPer1K must be non-zero for the proxy to compute a
	// non-zero frontierCost; the SpendGuard.Check short-circuit
	// guards on `frontierCost > 0` (issue #220).
	deps.Config.FrontierCostPer1K = 0.002
	rr := &rejectionRecorder{}
	deps.RejectionObserver = rr

	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%q", rw.Code, rw.Body.String())
	}
	events := rr.snapshot()
	if len(events) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(events))
	}
	if events[0].Reason != RejectionBudget {
		t.Errorf("reason = %q, want %q", events[0].Reason, RejectionBudget)
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeBudgetExceeded {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeBudgetExceeded)
	}
}

// TestChatRejectionUpstreamEnvelope covers a 502 cascade-all-fail
// upstream path and verifies the body is now an OpenAI-style JSON
// envelope (issue #453).
func TestChatRejectionUpstreamEnvelope(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rw.Code, rw.Body.String())
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeUpstreamError {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeUpstreamError)
	}
}

// TestChatRejectionLocalBusyEnvelope covers the 503 VRAM/limiter
// rejection path (issue #81) and verifies the body is now an
// OpenAI-style JSON envelope (issue #453).
func TestChatRejectionLocalBusyEnvelope(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.LocalLimiter = stubLocalLimiter{
		acquire: func(_ context.Context) (func(), error) {
			return nil, errors.New("stub limiter busy")
		},
	}
	// If the handler mistakenly dispatches, this handler records the
	// call so the assertion can catch it.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%q", rw.Code, rw.Body.String())
	}
	if len(rt.Calls()) != 0 {
		t.Fatalf("expected zero upstream calls, got %d", len(rt.Calls()))
	}
	env := decodeEnvelope(t, rw)
	if env.Error.Type != ErrTypeLocalCapacityError {
		t.Errorf("envelope type = %q, want %q", env.Error.Type, ErrTypeLocalCapacityError)
	}
}
