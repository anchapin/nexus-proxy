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

// BucketCount returns 0 for nil limiter.
func TestAuthLimiter_BucketCount_Nil(t *testing.T) {
	var al *AuthLimiter
	if al.BucketCount() != 0 {
		t.Error("nil limiter should return 0 buckets")
	}
}

// BucketCount returns 0 when limiter is disabled.
func TestAuthLimiter_BucketCount_Disabled(t *testing.T) {
	al := NewAuthLimiter(0, 3, 5*time.Minute, nil)
	if al.BucketCount() != 0 {
		t.Error("disabled limiter should return 0 buckets")
	}
}

// BucketCount returns correct count of tracked IPs.
func TestAuthLimiter_BucketCount_Active(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	if al.BucketCount() != 0 {
		t.Error("fresh limiter should have 0 buckets")
	}

	al.RecordFailure("10.0.0.1")
	if al.BucketCount() != 1 {
		t.Errorf("BucketCount = %d, want 1", al.BucketCount())
	}

	al.RecordFailure("10.0.0.2")
	al.RecordFailure("10.0.0.3")
	if al.BucketCount() != 3 {
		t.Errorf("BucketCount = %d, want 3", al.BucketCount())
	}
}

// BucketCount reflects entries removed by reaper eviction.
func TestAuthLimiter_BucketCount_AfterReap(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	// Add entries directly to the map
	now := time.Now()
	al.mu.Lock()
	al.failures["10.0.0.1"] = &authFailure{lastSeen: now.Add(-15 * time.Minute)} // idle > 10 min
	al.failures["10.0.0.2"] = &authFailure{lastSeen: now.Add(-5 * time.Minute)}  // idle < 10 min
	al.mu.Unlock()

	if al.BucketCount() != 2 {
		t.Errorf("before reaping: BucketCount = %d, want 2", al.BucketCount())
	}

	// Manually trigger reaper eviction logic (simulates what reaper goroutine does on tick)
	al.mu.Lock()
	for ip, f := range al.failures {
		f.mu.Lock()
		al.pruneLocked(f, now)
		idle := now.Sub(f.lastSeen)
		f.mu.Unlock()
		if idle > 10*time.Minute && len(f.ts) == 0 {
			delete(al.failures, ip)
		}
	}
	al.mu.Unlock()

	if al.BucketCount() != 1 {
		t.Errorf("after reaping idle: BucketCount = %d, want 1", al.BucketCount())
	}
}

// Enabled returns false for nil limiter.
func TestAuthLimiter_Enabled_Nil(t *testing.T) {
	var al *AuthLimiter
	if al.Enabled() {
		t.Error("nil limiter should not be enabled")
	}
}

// Enabled returns false when rpm <= 0.
func TestAuthLimiter_Enabled_Disabled(t *testing.T) {
	al := NewAuthLimiter(0, 3, 5*time.Minute, nil)
	if al.Enabled() {
		t.Error("disabled limiter (rpm=0) should not be enabled")
	}
}

// Enabled returns true when rpm > 0.
func TestAuthLimiter_Enabled_Active(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	if !al.Enabled() {
		t.Error("active limiter should be enabled")
	}
}

// Stop is safe to call on nil limiter.
func TestAuthLimiter_Stop_Nil(t *testing.T) {
	var al *AuthLimiter
	al.Stop() // must not panic
}

// Stop is safe to call on disabled limiter.
func TestAuthLimiter_Stop_Disabled(t *testing.T) {
	al := NewAuthLimiter(0, 3, 5*time.Minute, nil)
	al.Stop() // must not panic
}

// Stop signals reaper goroutine to exit without hanging.
func TestAuthLimiter_Stop_ExitsGoroutine(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)

	done := make(chan struct{})
	go func() {
		al.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop completed successfully
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not complete within 2s — possible goroutine leak")
	}
}

// Reaper evicts entries that are both idle > 10 min and have no failure timestamps.
func TestAuthLimiter_Reaper_Eviction(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	now := time.Now()

	// Three entries: idle >10min with no failures, idle <10min, and active
	al.mu.Lock()
	al.failures["stale-empty"] = &authFailure{
		ts:       nil,
		lastSeen: now.Add(-15 * time.Minute), // idle > 10 min, no failures → evicted
	}
	al.failures["stale-with-failures"] = &authFailure{
		ts:       []time.Time{now.Add(-5 * time.Minute)}, // has recent failure → kept
		lastSeen: now.Add(-15 * time.Minute),
	}
	al.failures["recent"] = &authFailure{
		ts:       nil,
		lastSeen: now.Add(-5 * time.Minute), // idle < 10 min → kept
	}
	al.mu.Unlock()

	if al.BucketCount() != 3 {
		t.Fatalf("initial BucketCount = %d, want 3", al.BucketCount())
	}

	// Simulate one reaper tick: prune and evict
	al.mu.Lock()
	for ip, f := range al.failures {
		f.mu.Lock()
		al.pruneLocked(f, now)
		idle := now.Sub(f.lastSeen)
		f.mu.Unlock()
		if idle > 10*time.Minute && len(f.ts) == 0 {
			delete(al.failures, ip)
		}
	}
	al.mu.Unlock()

	if al.BucketCount() != 2 {
		t.Errorf("after reaping: BucketCount = %d, want 2 (stale-empty evicted)", al.BucketCount())
	}
	if al.IsBlocked("stale-empty") {
		t.Error("stale-empty should have been evicted")
	}
	if al.IsBlocked("stale-with-failures") {
		t.Error("stale-with-failures should still exist")
	}
	if al.IsBlocked("recent") {
		t.Error("recent should still exist")
	}
}

// SetOnReap callback fires when the reaper evicts an idle IP.
func TestAuthLimiter_SetOnReap_Fires(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	al := NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	defer al.Stop()

	var evictions int64
	al.SetOnReap(func() {
		atomic.AddInt64(&evictions, 1)
	})

	now := time.Now()

	// Three entries: two will be evicted, one will not
	al.mu.Lock()
	al.failures["stale-evict1"] = &authFailure{
		ts:       nil,
		lastSeen: now.Add(-15 * time.Minute), // idle > 10 min, no failures → evicted
	}
	al.failures["stale-evict2"] = &authFailure{
		ts:       nil,
		lastSeen: now.Add(-20 * time.Minute), // idle > 10 min, no failures → evicted
	}
	al.failures["stale-kept"] = &authFailure{
		ts:       []time.Time{now.Add(-1 * time.Minute)}, // has recent failure → kept
		lastSeen: now.Add(-15 * time.Minute),
	}
	al.mu.Unlock()

	if al.BucketCount() != 3 {
		t.Fatalf("initial BucketCount = %d, want 3", al.BucketCount())
	}

	// Trigger one reaper tick via Reap()
	al.Reap()

	if evictions != 2 {
		t.Errorf("evictions = %d, want 2", evictions)
	}
	if al.BucketCount() != 1 {
		t.Errorf("after Reap: BucketCount = %d, want 1", al.BucketCount())
	}
}

// Reap is safe to call on nil limiter.
func TestAuthLimiter_Reap_Nil(t *testing.T) {
	var al *AuthLimiter
	al.Reap() // must not panic
}

// Reap is safe to call on disabled limiter.
func TestAuthLimiter_Reap_Disabled(t *testing.T) {
	al := NewAuthLimiter(0, 3, 5*time.Minute, nil)
	al.Reap() // must not panic
}

// SetOnReap is safe to call on nil limiter.
func TestAuthLimiter_SetOnReap_Nil(t *testing.T) {
	var al *AuthLimiter
	al.SetOnReap(func() {}) // must not panic
}
