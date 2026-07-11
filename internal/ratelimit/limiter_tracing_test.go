package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracingtest"
)

// TestRatelimitTracingAcceptPath verifies an allowed request emits
// a `ratelimit.check` span with ratelimit.allowed=true and the
// correct scope attribute.
func TestRatelimitTracingAcceptPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	l := New(Config{PerClientRPM: 60, PerClientBurst: 5})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "ratelimit.check")
	if s == nil {
		t.Fatalf("missing ratelimit.check span; got %d spans", len(coll.Spans(t)))
	}
	if got := tracingtest.AttrBool(s, "ratelimit.allowed"); !got {
		t.Errorf("ratelimit.allowed = false, want true")
	}
	if got := tracingtest.AttrString(s, "ratelimit.scope"); got != "per_client" {
		t.Errorf("ratelimit.scope = %q, want per_client", got)
	}
	if !strings.Contains(s.Status.Code, "OK") {
		t.Errorf("status code = %q, want OK", s.Status.Code)
	}
}

// TestRatelimitTracingRejectPath drives the limiter past its burst
// capacity and asserts the rejected 429 carries span attributes
// matching the issue #71 spec.
func TestRatelimitTracingRejectPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	l := New(Config{PerClientRPM: 60, PerClientBurst: 1})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	// Burn the single available token.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}

	// Second request must be rejected.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Two ratelimit.check spans were emitted; the second is the
	// rejected one (the exporter batches so order is preserved).
	var rejected *struct {
		name  string
		spans []tracingtest.CapturedSpan
	}
	_ = rejected // silence unused
	all := coll.Spans(t)
	var seen int
	var last *tracingtest.CapturedSpan
	for i := range all {
		if all[i].Name == "ratelimit.check" {
			seen++
			s := all[i]
			last = &s
		}
	}
	if seen != 2 {
		t.Fatalf("expected 2 ratelimit.check spans, got %d", seen)
	}
	if last == nil {
		t.Fatal("no ratelimit.check span captured")
	}
	if got := tracingtest.AttrBool(last, "ratelimit.allowed"); got {
		t.Errorf("ratelimit.allowed = true on rejected request, want false")
	}
	if got := tracingtest.AttrString(last, "ratelimit.scope"); got != "per_client" {
		t.Errorf("ratelimit.scope = %q, want per_client", got)
	}
	if !strings.Contains(last.Status.Code, "ERROR") {
		t.Errorf("status code = %q, want ERROR", last.Status.Code)
	}
}

// TestRatelimitTracingGlobalScope verifies the global bucket's
// scope attribute is "global" when only the global limiter is
// configured.
func TestRatelimitTracingGlobalScope(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	l := New(Config{GlobalRPM: 60, GlobalBurst: 1})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "ratelimit.check")
	if s == nil {
		t.Fatal("missing ratelimit.check span")
	}
	if got := tracingtest.AttrString(s, "ratelimit.scope"); got != "global" {
		t.Errorf("ratelimit.scope = %q, want global", got)
	}
}
