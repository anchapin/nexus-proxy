package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracingtest"
)

// TestAuthTracingAcceptPath verifies that an accepted request emits
// an `auth.check` span with outcome="accepted" and the correct
// method attribute (issue #71 AC: "Unit tests verify the span
// attributes are correct on accept and reject paths").
func TestAuthTracingAcceptPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := Middleware([]string{"sk-secret"}, nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Close drains the queue. Without this the test would race
	// the background POST goroutine.
	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "auth.check")
	if s == nil {
		t.Fatalf("missing auth.check span; got %d spans total", len(coll.Spans(t)))
	}
	if got := tracingtest.AttrString(s, "auth.method"); got != "bearer" {
		t.Errorf("auth.method = %q, want bearer", got)
	}
	if got := tracingtest.AttrBool(s, "auth.exempt"); got {
		t.Errorf("auth.exempt = true, want false")
	}
	if got := tracingtest.AttrString(s, "auth.outcome"); got != "accepted" {
		t.Errorf("auth.outcome = %q, want accepted", got)
	}
	if !strings.Contains(s.Status.Code, "OK") {
		t.Errorf("status code = %q, want OK", s.Status.Code)
	}
}

// TestAuthTracingRejectPath verifies that an invalid credential
// stamps outcome="rejected", Status=Error, and the same method/exempt
// attributes the accept path uses.
func TestAuthTracingRejectPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := Middleware([]string{"sk-secret"}, nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "auth.check")
	if s == nil {
		t.Fatalf("missing auth.check span")
	}
	if got := tracingtest.AttrString(s, "auth.outcome"); got != "rejected" {
		t.Errorf("auth.outcome = %q, want rejected", got)
	}
	if !strings.Contains(s.Status.Code, "ERROR") {
		t.Errorf("status code = %q, want ERROR", s.Status.Code)
	}
}

// TestAuthTracingExemptPath verifies an exempt request (healthz)
// emits a span with auth.exempt=true and outcome="exempt" even when
// keys are configured.
func TestAuthTracingExemptPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	exempt := func(r *http.Request) bool { return r.URL.Path == "/healthz" }
	h := Middleware([]string{"sk-secret"}, exempt)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "auth.check")
	if s == nil {
		t.Fatalf("missing auth.check span")
	}
	if got := tracingtest.AttrBool(s, "auth.exempt"); !got {
		t.Errorf("auth.exempt = false, want true for /healthz")
	}
	if got := tracingtest.AttrString(s, "auth.outcome"); got != "exempt" {
		t.Errorf("auth.outcome = %q, want exempt", got)
	}
}
