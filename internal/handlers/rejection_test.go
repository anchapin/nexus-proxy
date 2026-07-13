package handlers

import (
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
// RejectionObserver with reason="body_too_large".
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
}

// TestChatRejectionBadJSON verifies the 400 invalid-JSON path fires
// the RejectionObserver with reason="bad_request".
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
}

// TestChatRejectionMissingMessages verifies the 400 missing-messages
// path fires the RejectionObserver with reason="bad_request".
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
