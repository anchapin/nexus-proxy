package ratelimit

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracingtest"
)

func parseCIDR(t *testing.T, s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

// TestAllowCIDRsMiddleware_DisabledIsNoOp verifies that a nil or
// empty-CIDR middleware returns next unchanged and creates no span.
func TestAllowCIDRsMiddleware_DisabledIsNoOp(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewAllowCIDRsMiddleware(nil, nil, resolver)

	called := false
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("disabled middleware should pass through")
	}
}

// TestAllowCIDRsMiddleware_EmptyCIDRListIsNoOp verifies that an
// explicitly empty CIDR list is also a no-op.
func TestAllowCIDRsMiddleware_EmptyCIDRListIsNoOp(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewAllowCIDRsMiddleware([]*net.IPNet{}, nil, resolver)

	called := false
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("empty-CIDR middleware should pass through")
	}
}

// TestAllowCIDRsMiddleware_RejectHasSpanAttributes verifies that a
// rejected request emits a "ratelimit.allowlist.check" span with
// allowlist.matched=false and client_ip attributes (issue #1365).
func TestAllowCIDRsMiddleware_RejectHasSpanAttributes(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	cidrs := []*net.IPNet{parseCIDR(t, "192.168.1.0/24")}
	m := NewAllowCIDRsMiddleware(cidrs, nil, resolver)

	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected request should not reach handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234" // not in 192.168.1.0/24
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	span := coll.FindSpan(t, "ratelimit.allowlist.check")
	if span == nil {
		t.Fatal("no ratelimit.allowlist.check span found")
	}
	if matched := tracingtest.AttrBool(span, "allowlist.matched"); matched {
		t.Error("allowlist.matched should be false for rejected request")
	}
	if clientIP := tracingtest.AttrString(span, "client_ip"); clientIP == "" {
		t.Error("client_ip attribute should be non-empty")
	}
}

// TestAllowCIDRsMiddleware_AllowHasSpanAttributes verifies that an
// allowed request emits a "ratelimit.allowlist.check" span with
// allowlist.matched=true (issue #1365).
func TestAllowCIDRsMiddleware_AllowHasSpanAttributes(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	cidrs := []*net.IPNet{parseCIDR(t, "10.0.0.0/8")}
	m := NewAllowCIDRsMiddleware(cidrs, nil, resolver)

	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1234" // in 10.0.0.0/8
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	span := coll.FindSpan(t, "ratelimit.allowlist.check")
	if span == nil {
		t.Fatal("no ratelimit.allowlist.check span found")
	}
	if matched := tracingtest.AttrBool(span, "allowlist.matched"); !matched {
		t.Error("allowlist.matched should be true for allowed request")
	}
	if clientIP := tracingtest.AttrString(span, "client_ip"); clientIP == "" {
		t.Error("client_ip attribute should be non-empty")
	}
}

// TestAllowCIDRsMiddleware_DisabledCreatesNoSpan verifies that when the
// allowlist is disabled (no CIDRs), no tracing span is created even
// when a global exporter is registered (issue #1365).
func TestAllowCIDRsMiddleware_DisabledCreatesNoSpan(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewAllowCIDRsMiddleware(nil, nil, resolver) // disabled

	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)

	called := false
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("handler should have been called")
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	// No spans should be emitted for a disabled allowlist.
	if spans := coll.Spans(t); len(spans) != 0 {
		t.Errorf("expected no spans for disabled allowlist, got %d", len(spans))
	}
}
