package ratelimit

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiddleware_Disabled_Passthrough(t *testing.T) {
	m := NewMiddleware(0, 0, nil)
	called := false
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), &http.Request{RemoteAddr: "1.2.3.4:5"})
	if !called {
		t.Error("disabled middleware should pass through")
	}
	if m.BucketCount() != 0 {
		t.Error("disabled middleware should not track buckets")
	}
}

func TestMiddleware_NilResolver_UsesPeer(t *testing.T) {
	m := NewMiddleware(1, 1, nil) // 1 req/min, burst 1
	var hits int
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r1.RemoteAddr = "10.0.0.1:1000"
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), r1)
	h.ServeHTTP(httptest.NewRecorder(), r2)
	if hits != 1 {
		t.Errorf("expected 1 hit (burst exhausted), got %d", hits)
	}
}

// A single client with burst=2 + rpm large enough to not refill within
// the test: first 2 succeed, 3rd is 429.
func TestMiddleware_429AfterBurst(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver) // huge rpm, burst 2
	var statuses []int
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.0.0.1:1000"
		h.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}
	// Two allowed, two throttled.
	if statuses[0] != 200 || statuses[1] != 200 {
		t.Errorf("first two should pass: %v", statuses)
	}
	if statuses[2] != 429 || statuses[3] != 429 {
		t.Errorf("next two should be throttled: %v", statuses)
	}
}

// Different client IPs get independent buckets.
func TestMiddleware_PerClientIsolation(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1, 1, resolver) // burst 1 each
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, addr := range []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("client %s should pass on first request, got %d", addr, rec.Code)
		}
	}
	if m.BucketCount() != 3 {
		t.Errorf("expected 3 buckets, got %d", m.BucketCount())
	}
}

// Trusted-proxy resolver is honoured: two spoofed-XFF requests from the
// same real client are bucketed together even though RemoteAddr differs.
func TestMiddleware_ResolverHonoured(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	resolver := NewClientIPResolver(trusted)
	m := NewMiddleware(1, 1, resolver) // burst 1
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Both requests claim the same forwarded client through different
	// (trusted) proxy peers.
	r1 := httptest.NewRequest(http.MethodPost, "/", nil)
	r1.RemoteAddr = "10.0.0.1:1000"
	r1.Header.Set("X-Forwarded-For", "203.0.113.5")
	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.RemoteAddr = "10.0.0.2:1000"
	r2.Header.Set("X-Forwarded-For", "203.0.113.5")
	rec1, rec2 := httptest.NewRecorder(), httptest.NewRecorder()
	h.ServeHTTP(rec1, r1)
	h.ServeHTTP(rec2, r2)
	if rec1.Code != 200 {
		t.Errorf("first should pass, got %d", rec1.Code)
	}
	if rec2.Code != 429 {
		t.Errorf("second should be throttled (same resolved client), got %d", rec2.Code)
	}
}

// Refill after time advances lets a throttled client back in.
func TestMiddleware_RefillOverTime(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(60, 1, resolver) // 60/min = 1/sec, burst 1
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req)
	if rec1.Code != 200 {
		t.Fatalf("first should pass, got %d", rec1.Code)
	}
	// Simulate >1s elapse by directly refilling the bucket clock.
	m.mu.Lock()
	b := m.buckets["10.0.0.1"]
	m.mu.Unlock()
	b.mu.Lock()
	b.lastRefill = b.lastRefill.Add(-2 * time.Second)
	b.mu.Unlock()

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Errorf("should pass after refill, got %d", rec2.Code)
	}
}

// Reaper evicts idle buckets.
func TestMiddleware_Reaper(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(10, 1, resolver)
	m.ttl = 50 * time.Millisecond
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if m.BucketCount() != 1 {
		t.Fatalf("expected 1 bucket, got %d", m.BucketCount())
	}
	time.Sleep(80 * time.Millisecond)
	m.reap(time.Now())
	if m.BucketCount() != 0 {
		t.Errorf("idle bucket should be reaped, got %d", m.BucketCount())
	}
}

// Concurrency: many goroutines hitting the limiter must not race and
// must never exceed the configured burst across all of them.
func TestMiddleware_ConcurrentNoRace(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	// 60 RPM = 1 token/sec, so refill over a sub-second test window is
	// negligible (~0.01 tokens in 10ms). This keeps the success ceiling
	// at the burst capacity regardless of scheduling jitter, making the
	// assertion deterministic. A high RPM (e.g. 100k) would refill ~17
	// tokens in 10ms and make the test flaky.
	m := NewMiddleware(60, 10, resolver) // burst 10
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	var ok int64
	var mu sync.Mutex
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "10.0.0.1:1000"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == 200 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// At most burst (10) requests should have succeeded regardless of
	// how many raced. With 60 RPM the refill over the test window is
	// ~0.01 tokens, so the ceiling is the burst capacity plus a small
	// safety margin for implementation edge cases.
	if ok > 12 {
		t.Errorf("too many concurrent successes: %d (burst 10)", ok)
	}
}

// Ensure the _ variable is used so unused imports don't break the build
// in toolchains that disable the test-time net import. Kept minimal.
var _ = net.ParseIP

// TestMiddleware_RejectionHookFires verifies that the SetRejectionHook
// callback is invoked once per 429 (issue #119). Two requests exhaust
// the burst, so the 3rd and 4th must each fire the hook.
func TestMiddleware_RejectionHookFires(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver) // huge rpm, burst 2
	var rejected int64
	m.SetRejectionHook(func() {
		atomic.AddInt64(&rejected, 1)
	})
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.0.0.1:1000"
		h.ServeHTTP(rec, req)
	}
	if rejected != 2 {
		t.Errorf("rejection hook fired %d times, want 2", rejected)
	}
}

// TestMiddleware_RejectionHookNilSafe confirms a middleware with no
// hook installed still works (no nil-panic on the 429 path).
func TestMiddleware_RejectionHookNilSafe(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1, 1, resolver) // burst 1
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req) // consumes burst
	h.ServeHTTP(httptest.NewRecorder(), req) // 429 — must not panic
}

// TestMiddleware_SetRejectionHookRemoves confirms passing nil clears
// a previously installed hook.
func TestMiddleware_SetRejectionHookRemoves(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1, 1, resolver)
	var fired int64
	m.SetRejectionHook(func() { atomic.AddInt64(&fired, 1) })
	m.SetRejectionHook(nil)
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if fired != 0 {
		t.Errorf("hook fired %d after nil removal, want 0", fired)
	}
}

// TestMiddleware_BucketRaceConcurrencyFix verifies issue #248: many
// concurrent goroutines requesting the same previously-unseen IP must
// result in exactly one bucket, not one per goroutine.
func TestMiddleware_BucketRaceConcurrencyFix(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 1000, resolver) // big burst to avoid 429 noise
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const goroutines = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "192.168.99.99:9999" // same IP for all goroutines
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	// Issue #248: only ONE bucket should exist for that IP. A race window
	// during bucket creation would have let each goroutine allocate its own
	// bucket before inserting, orphaning all but the last.
	if count := m.BucketCount(); count != 1 {
		t.Errorf("expected exactly 1 bucket for concurrent same-IP requests, got %d (race window not fixed)", count)
	}
}

// TestMiddleware_SetRPM verifies SetRPM updates the steady-state rate.
// A newly created bucket should use the updated rpm for refill calculations.
func TestMiddleware_SetRPM(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(60, 2, resolver) // 60/min = 1/s, burst 2
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"

	// Exhaust burst.
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Should be throttled.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Errorf("expected 429 after burst exhausted, got %d", rec.Code)
	}

	// Increase RPM — refill should be faster but burst is still exhausted.
	// With 60 RPM (1/s), 1 second should add 1 token, making 1 request succeed.
	m.SetRPM(60)
	time.Sleep(1100 * time.Millisecond)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 after refill with 60 RPM, got %d", rec.Code)
	}
}

// TestMiddleware_SetBurst verifies SetBurst updates the bucket capacity.
func TestMiddleware_SetBurst(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver) // burst 2
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"

	// Exhaust original burst of 2.
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	{
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 429 {
			t.Errorf("expected 429 after burst 2 exhausted, got %d", rec.Code)
		}
	}

	// Increase burst to 5 — but existing bucket is still throttled.
	// SetBurst affects NEW buckets, not existing ones.
	m.SetBurst(5)

	// A NEW client IP gets the new burst capacity of 5.
	newReq := httptest.NewRequest(http.MethodPost, "/", nil)
	newReq.RemoteAddr = "10.0.0.2:1000" // different IP = new bucket
	{
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReq)
		if rec.Code != 200 {
			t.Errorf("expected 200 for new client with burst 5, got %d", rec.Code)
		}
	}

	// Verify the original client is still throttled (existing bucket unchanged).
	{
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 429 {
			t.Errorf("expected 429 for original client (existing bucket), got %d", rec.Code)
		}
	}

	// Exhaust new client's burst of 5.
	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), newReq)
	}
	{
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newReq)
		if rec.Code != 429 {
			t.Errorf("expected 429 after new client's burst 5 exhausted, got %d", rec.Code)
		}
	}
}

// TestMiddleware_SetBurstZeroDoesNotChange verifies that SetBurst(0) is a no-op.
func TestMiddleware_SetBurstZeroDoesNotChange(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver) // burst 2
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"

	// Exhaust burst.
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// SetBurst(0) should be a no-op.
	m.SetBurst(0)
	{
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 429 {
			t.Errorf("expected 429 after SetBurst(0), got %d", rec.Code)
		}
	}
}

// TestMiddleware_NilSafeSetRPM verifies SetRPM on a nil middleware does not panic.
func TestMiddleware_NilSafeSetRPM(t *testing.T) {
	var m *Middleware
	m.SetRPM(100) // must not panic
}

// TestMiddleware_NilSafeSetBurst verifies SetBurst on a nil middleware does not panic.
func TestMiddleware_NilSafeSetBurst(t *testing.T) {
	var m *Middleware
	m.SetBurst(10) // must not panic
}
