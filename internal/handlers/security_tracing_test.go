package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracingtest"
)

// TestSecurityHeadersTracingSpan verifies that the security.headers
// middleware emits a span with the security.tls_active attribute
// matching the tlsActive argument (issue #71 AC: span coverage for
// the four middleware paths).
func TestSecurityHeadersTracingSpan(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(SecurityHeaders(true)(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "security.headers")
	if s == nil {
		t.Fatalf("missing security.headers span; got %d spans", len(coll.Spans(t)))
	}
	if got := tracingtest.AttrBool(s, "security.tls_active"); !got {
		t.Errorf("security.tls_active = false, want true (constructed with true)")
	}
}

// TestSecurityHeadersTracingNoTLS verifies the span attribute
// reflects a plaintext deployment (security.tls_active=false) so a
// trace view surfaces the missing HSTS posture.
func TestSecurityHeadersTracingNoTLS(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(SecurityHeaders(false)(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "security.headers")
	if s == nil {
		t.Fatal("missing security.headers span")
	}
	if got := tracingtest.AttrBool(s, "security.tls_active"); got {
		t.Errorf("security.tls_active = true, want false")
	}
}
