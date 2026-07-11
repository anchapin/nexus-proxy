package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// captureObserver is a thread-safe hook that records every scope /
// allowed combination the limiter emits. Tests inspect the recorded
// counts to verify the observer fires on the expected decision
// paths (issue #70).
type captureObserver struct {
	allowedGlobal     atomic.Uint32
	rejectedGlobal    atomic.Uint32
	allowedPerClient  atomic.Uint32
	rejectedPerClient atomic.Uint32
	unknown           atomic.Uint32
}

func (o *captureObserver) observe(scope string, allowed bool) {
	switch {
	case scope == "global" && allowed:
		o.allowedGlobal.Add(1)
	case scope == "global" && !allowed:
		o.rejectedGlobal.Add(1)
	case scope == "per_client" && allowed:
		o.allowedPerClient.Add(1)
	case scope == "per_client" && !allowed:
		o.rejectedPerClient.Add(1)
	default:
		o.unknown.Add(1)
	}
}

// TestRateLimitObserverAcceptPath (issue #70) verifies the observer
// fires with (scope="per_client", allowed=true) on a permitted
// request.
func TestRateLimitObserverAcceptPath(t *testing.T) {
	o := &captureObserver{}
	l := New(Config{
		PerClientRPM:   60,
		PerClientBurst: 5,
		Observer:       o.observe,
	})
	defer l.Close()

	h := l.Middleware(nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := o.allowedPerClient.Load(); got != 1 {
		t.Errorf("allowedPerClient = %d, want 1", got)
	}
	if got := o.unknown.Load(); got != 0 {
		t.Errorf("unknown scope fired: %d", got)
	}
}

// TestRateLimitObserverRejectPath (issue #70) drives a per-client
// bucket past burst capacity and verifies the observer emits
// (scope, allowed=false) for the rejected request.
func TestRateLimitObserverRejectPath(t *testing.T) {
	o := &captureObserver{}
	l := New(Config{
		PerClientRPM:   60,
		PerClientBurst: 1,
		Observer:       o.observe,
	})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	// Burn the single token.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}

	// Second request rejects.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}

	if got := o.allowedPerClient.Load(); got != 1 {
		t.Errorf("allowedPerClient = %d, want 1 (first request)", got)
	}
	if got := o.rejectedPerClient.Load(); got != 1 {
		t.Errorf("rejectedPerClient = %d, want 1 (second request)", got)
	}
}

// TestRateLimitObserverGlobalScope (issue #70) configures a
// global-only limiter and verifies the observer emits scope="global"
// for both allow and reject outcomes.
func TestRateLimitObserverGlobalScope(t *testing.T) {
	o := &captureObserver{}
	l := New(Config{
		GlobalRPM:   60,
		GlobalBurst: 1,
		Observer:    o.observe,
	})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	// Burn the global token.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("first: status = %d, want 200", rec.Code)
	}

	// Second request — different client, same global bucket — rejected.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status = %d, want 429", rec.Code)
	}

	if got := o.allowedGlobal.Load(); got != 1 {
		t.Errorf("allowedGlobal = %d, want 1", got)
	}
	if got := o.rejectedGlobal.Load(); got != 1 {
		t.Errorf("rejectedGlobal = %d, want 1", got)
	}
	if got := o.allowedPerClient.Load() + o.rejectedPerClient.Load(); got != 0 {
		t.Errorf("per_client counters fired on global-only limiter: %d", got)
	}
}

// TestRateLimitSetObserver (issue #70) verifies SetObserver can
// install the hook after construction. This matches main.go's flow
// where the limiter is built before the collector (so the closure
// has somewhere to point).
func TestRateLimitSetObserver(t *testing.T) {
	o := &captureObserver{}
	l := New(Config{PerClientRPM: 60, PerClientBurst: 5}) // no observer at construction
	defer l.Close()
	l.SetObserver(o.observe)
	h := l.Middleware(nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := o.allowedPerClient.Load(); got != 1 {
		t.Errorf("SetObserver did not install hook: allowedPerClient = %d, want 1", got)
	}
}

// TestRateLimitNilObserver (issue #70) verifies a nil observer is a
// no-op: the limiter keeps enforcing decisions without panicking.
func TestRateLimitNilObserver(t *testing.T) {
	l := New(Config{
		PerClientRPM:   60,
		PerClientBurst: 1,
		Observer:       nil, // explicit nil
	})
	defer l.Close()
	h := l.Middleware(nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("nil observer path: status = %d, want 200", rec.Code)
	}
}
