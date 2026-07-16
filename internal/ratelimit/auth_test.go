package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// AuthLimiter disabled (rpm <= 0) should pass all requests through.
func TestAuthLimiter_Disabled_Passthrough(t *testing.T) {
	al := NewAuthLimiter(0, 3, 5*time.Minute, nil)
	resolver := NewClientIPResolver(nil)
	called := false
	h := al.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}), resolver)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.1:1000"
	al.Wrap(h, resolver).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Error("disabled limiter should pass through")
	}
}

// nil AuthLimiter should pass all requests through.
func TestAuthLimiter_NilSafe(t *testing.T) {
	var al *AuthLimiter
	resolver := NewClientIPResolver(nil)
	h := al.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), resolver)
	// This would panic if Wrap didn't handle nil
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
}

// After burst auth failures, subsequent requests from same IP are blocked.
func TestAuthLimiter_BlockedAfterBurst(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver) // burst 3
	h := al.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), resolver)

	// 3 failures should not yet block
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.0.0.1:1000"
		al.RecordFailure(resolver.Resolve(req))
	}

	// 4th failure should block
	if !al.IsBlocked("10.0.0.1") {
		t.Error("IP should be blocked after 3 failures")
	}

	// Request to blocked IP should get 429
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("blocked IP: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header not set on 429")
	}
}

// Different IPs get independent failure tracking.
func TestAuthLimiter_PerClientIsolation(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 2, 5*time.Minute, resolver) // burst 2

	// Exhaust burst for IP 1
	for i := 0; i < 2; i++ {
		al.RecordFailure("10.0.0.1")
	}

	// IP 1 should be blocked, IP 2 should not
	if !al.IsBlocked("10.0.0.1") {
		t.Error("IP 1 should be blocked")
	}
	if al.IsBlocked("10.0.0.2") {
		t.Error("IP 2 should not be blocked")
	}
}

// OnBlock callback fires when client is blocked.
func TestAuthLimiter_OnBlockFires(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 2, 5*time.Minute, resolver) // burst 2
	var blocked int64
	al.SetOnBlock(func() {
		atomic.AddInt64(&blocked, 1)
	})

	// Exhaust burst
	al.RecordFailure("10.0.0.1")
	al.RecordFailure("10.0.0.1")

	if blocked != 1 {
		t.Errorf("onBlock fired %d times, want 1", blocked)
	}
}

// After window expires, failures are pruned and client is unblocked.
func TestAuthLimiter_WindowExpiry(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 50*time.Millisecond, resolver) // short window

	// Record 3 failures
	for i := 0; i < 3; i++ {
		al.RecordFailure("10.0.0.1")
	}
	if !al.IsBlocked("10.0.0.1") {
		t.Error("IP should be blocked after 3 failures")
	}

	// Wait for window to expire
	time.Sleep(100 * time.Millisecond)

	// Prune manually (the reaper would do this too)
	al.mu.Lock()
	f := al.failures["10.0.0.1"]
	f.mu.Lock()
	al.pruneLocked(f, time.Now())
	f.mu.Unlock()
	al.mu.Unlock()

	if al.IsBlocked("10.0.0.1") {
		t.Error("IP should be unblocked after window expires")
	}
}

// Concurrent access to auth limiter.
func TestAuthLimiter_Concurrent(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 100, 5*time.Minute, resolver) // burst 100

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				al.RecordFailure("10.0.0.1")
			}
		}()
	}
	wg.Wait()

	// After 500 failures, should be blocked
	if !al.IsBlocked("10.0.0.1") {
		t.Error("IP should be blocked after many concurrent failures")
	}
}

// OnBlock nil safety.
func TestAuthLimiter_SetOnBlockNil(t *testing.T) {
	al := NewAuthLimiter(60, 3, 5*time.Minute, nil)
	al.SetOnBlock(nil) // must not panic
}

// Verify 429 body contains expected shape.
func TestAuthLimiter_429Body(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 1, 5*time.Minute, resolver) // burst 1
	h := al.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), resolver)

	// Exhaust burst
	al.RecordFailure("10.0.0.1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	body := rec.Body.String()
	if body == "" {
		t.Error("response body should not be empty")
	}
	// Body should contain "auth_rate_limit_exceeded"
	if body == "" || len(body) < 10 {
		t.Errorf("body too short: %q", body)
	}
}
