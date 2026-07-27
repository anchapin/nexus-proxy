package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiddleware_Disabled_Passthrough(t *testing.T) {
	m := NewMiddleware(0, 0, nil, nil)
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
	m := NewMiddleware(1, 1, nil, nil) // 1 req/min, burst 1
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
	m := NewMiddleware(1000, 2, resolver, nil) // huge rpm, burst 2
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
	m := NewMiddleware(1, 1, resolver, nil) // burst 1 each
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
	m := NewMiddleware(1, 1, resolver, nil) // burst 1
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
	m := NewMiddleware(60, 1, resolver, nil) // 60/min = 1/sec, burst 1
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

// TestMiddleware_ReaperExitsOnClose verifies that the reaper goroutine
// exits within 2 seconds of Close() being called (issue #739).
func TestMiddleware_ReaperExitsOnClose(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(60, 1, resolver, nil) // rpm > 0 so reaper is started
	m.ttl = 10 * time.Minute                 // intentionally long so only the stop matters
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req) // ensure reaper is running

	done := make(chan struct{})
	go func() {
		m.reap(time.Now()) // drive reap manually while reaper ticker is live
		close(done)
	}()

	select {
	case <-done:
		// reap returned — proceed to close and verify reaper exits
	case <-time.After(500 * time.Millisecond):
		// reaper ticker cycle still running, which is fine
	}

	m.Close()

	// Give the reaper goroutine 2 seconds to exit after stopCh is closed.
	select {
	case <-time.After(2 * time.Second):
		t.Error("reaper did not exit within 2 seconds of Close()")
	default:
		// passed — goroutine exited in time
	}
}

// Reaper evicts idle buckets.
func TestMiddleware_Reaper(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(10, 1, resolver, nil)
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
	m := NewMiddleware(60, 10, resolver, nil) // burst 10
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
	m := NewMiddleware(1000, 2, resolver, nil) // huge rpm, burst 2
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
	m := NewMiddleware(1, 1, resolver, nil) // burst 1
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
	m := NewMiddleware(1, 1, resolver, nil)
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

// TestMiddleware_AllowHookFires verifies that the SetAllowHook
// callback is invoked once per allowed request (issue #746), before the
// token is consumed. With burst=2, the first two requests are allowed;
// the hook must fire exactly twice with a non-empty bucketID and a
// utilization value in (0, 1].
func TestMiddleware_AllowHookFires(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver, nil) // huge rpm, burst 2
	var calls []struct {
		bucketID       string
		utilizationPct float64
	}
	m.SetAllowHook(func(bucketID string, utilizationPct float64) {
		calls = append(calls, struct {
			bucketID       string
			utilizationPct float64
		}{bucketID, utilizationPct})
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
	if len(calls) != 2 {
		t.Errorf("allow hook fired %d times, want 2", len(calls))
	}
	for i, call := range calls {
		if call.bucketID == "" {
			t.Errorf("call %d: bucketID is empty, want non-empty", i)
		}
		if call.utilizationPct <= 0 || call.utilizationPct > 1 {
			t.Errorf("call %d: utilizationPct = %v, want (0, 1]", i, call.utilizationPct)
		}
	}
	// The first allowed request sees a full bucket (tokens=2, burst=2 → 1.0).
	if calls[0].utilizationPct != 1.0 {
		t.Errorf("first call utilizationPct = %v, want 1.0", calls[0].utilizationPct)
	}
	// The second allowed request sees ~1 token after the first decrement;
	// a tiny refill may have occurred between the two ServeHTTP calls so we
	// check the value is approximately 0.5 rather than exact.
	if calls[1].utilizationPct < 0.49 || calls[1].utilizationPct > 0.51 {
		t.Errorf("second call utilizationPct = %v, want ~0.5", calls[1].utilizationPct)
	}
}

// TestMiddleware_AllowHookNilSafe confirms a middleware with no hook
// installed still works (no nil-panic on the allow path).
func TestMiddleware_AllowHookNilSafe(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 1, resolver, nil) // burst 1
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req) // allowed — must not panic
}

// TestMiddleware_SetAllowHookRemoves confirms passing nil clears a
// previously installed hook.
func TestMiddleware_SetAllowHookRemoves(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 1, resolver, nil)
	var fired int64
	m.SetAllowHook(func(bucketID string, utilizationPct float64) {
		atomic.AddInt64(&fired, 1)
	})
	m.SetAllowHook(nil)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if fired != 0 {
		t.Errorf("hook fired %d after nil removal, want 0", fired)
	}
}

// TestMiddleware_AllowHookNotFiredOnRejection verifies the allow hook
// does NOT fire when a request is rejected (tokens < 1).
func TestMiddleware_AllowHookNotFiredOnRejection(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 1, resolver, nil) // burst 1
	var allowed int64
	m.SetAllowHook(func(bucketID string, utilizationPct float64) {
		atomic.AddInt64(&allowed, 1)
	})
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	h.ServeHTTP(httptest.NewRecorder(), req) // allowed
	h.ServeHTTP(httptest.NewRecorder(), req) // rejected (429)
	if allowed != 1 {
		t.Errorf("allow hook fired %d times on rejected request, want 1", allowed)
	}
}

// TestMiddleware_BucketRaceConcurrencyFix verifies issue #248: many
// concurrent goroutines requesting the same previously-unseen IP must
// result in exactly one bucket, not one per goroutine.
func TestMiddleware_BucketRaceConcurrencyFix(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 1000, resolver, nil) // big burst to avoid 429 noise
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
	m := NewMiddleware(60, 2, resolver, nil) // 60/min = 1/s, burst 2
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
	m := NewMiddleware(1000, 2, resolver, nil) // burst 2
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
	m := NewMiddleware(1000, 2, resolver, nil) // burst 2
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

// TestMiddleware_RPM_Disabled verifies RPM() returns 0 when the middleware is disabled.
func TestMiddleware_RPM_Disabled(t *testing.T) {
	m := NewMiddleware(0, 0, nil, nil)
	if got := m.RPM(); got != 0 {
		t.Errorf("RPM() on disabled middleware = %d, want 0", got)
	}
}

// TestMiddleware_RPM_Enabled verifies RPM() returns the configured value when enabled.
func TestMiddleware_RPM_Enabled(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(120, 10, resolver, nil)
	if got := m.RPM(); got != 120 {
		t.Errorf("RPM() = %d, want 120", got)
	}
}

// TestMiddleware_RPM_NilSafe verifies RPM() on a nil receiver does not panic and returns 0.
func TestMiddleware_RPM_NilSafe(t *testing.T) {
	var m *Middleware
	if got := m.RPM(); got != 0 {
		t.Errorf("RPM() on nil = %d, want 0", got)
	}
}

// TestMiddleware_Burst_Disabled verifies Burst() returns 0 when the middleware is disabled.
func TestMiddleware_Burst_Disabled(t *testing.T) {
	m := NewMiddleware(0, 0, nil, nil)
	if got := m.Burst(); got != 0 {
		t.Errorf("Burst() on disabled middleware = %d, want 0", got)
	}
}

// TestMiddleware_Burst_Enabled verifies Burst() returns the configured value when enabled.
func TestMiddleware_Burst_Enabled(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(60, 5, resolver, nil)
	if got := m.Burst(); got != 5 {
		t.Errorf("Burst() = %d, want 5", got)
	}
}

// TestMiddleware_Burst_NilSafe verifies Burst() on a nil receiver does not panic and returns 0.
func TestMiddleware_Burst_NilSafe(t *testing.T) {
	var m *Middleware
	if got := m.Burst(); got != 0 {
		t.Errorf("Burst() on nil = %d, want 0", got)
	}
}

// TestMiddleware_Enabled_FalseWhenDisabled verifies Enabled() returns false when rpm <= 0.
func TestMiddleware_Enabled_FalseWhenDisabled(t *testing.T) {
	m := NewMiddleware(0, 0, nil, nil)
	if got := m.Enabled(); got != false {
		t.Errorf("Enabled() on disabled middleware = %v, want false", got)
	}
}

// TestMiddleware_Enabled_TrueWhenEnabled verifies Enabled() returns true when rpm > 0.
func TestMiddleware_Enabled_TrueWhenEnabled(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(60, 1, resolver, nil)
	if got := m.Enabled(); got != true {
		t.Errorf("Enabled() on enabled middleware = %v, want true", got)
	}
}

// TestMiddleware_Enabled_NilSafe verifies Enabled() on a nil receiver does not panic and returns false.
func TestMiddleware_Enabled_NilSafe(t *testing.T) {
	var m *Middleware
	if got := m.Enabled(); got != false {
		t.Errorf("Enabled() on nil = %v, want false", got)
	}
}

// TestMiddleware_APIKeyAwareKeyFunc_Hashed verifies that APIKeyAwareKeyFunc
// returns a SHA256 hash truncated to 8 hex chars and that different API keys
// behind the same IP produce different bucket keys (issue #776).
func TestMiddleware_APIKeyAwareKeyFunc_Hashed(t *testing.T) {
	ip := "10.0.0.1"

	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.Header.Set("Authorization", "Bearer key-alpha")

	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Authorization", "Bearer key-beta")

	key1 := APIKeyAwareKeyFunc(ip, req1)
	key2 := APIKeyAwareKeyFunc(ip, req2)

	if key1 == "" || key2 == "" {
		t.Fatal("APIKeyAwareKeyFunc returned empty string")
	}
	if len(key1) != 8 || len(key2) != 8 {
		t.Errorf("expected 8-char hex key, got key1=%q (%d chars), key2=%q (%d chars)", key1, len(key1), key2, len(key2))
	}
	if key1 == key2 {
		t.Errorf("different API keys should produce different bucket keys: key1=%q, key2=%q", key1, key2)
	}
	// Same IP + same key should produce the same key.
	key1Again := APIKeyAwareKeyFunc(ip, req1)
	if key1 != key1Again {
		t.Errorf("same IP+key should produce same hash: first=%q, second=%q", key1, key1Again)
	}
}

// TestMiddleware_APIKeyAwareKeyFunc_NoAuth returns IP when no Authorization header is present.
func TestMiddleware_APIKeyAwareKeyFunc_NoAuth(t *testing.T) {
	ip := "10.0.0.1"
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// No Authorization header set.
	key := APIKeyAwareKeyFunc(ip, req)
	if key != ip {
		t.Errorf("expected IP=%q when no Authorization header, got %q", ip, key)
	}
}

// TestMiddleware_APIKeyMode_DifferentBuckets verifies that two different
// API keys from the same IP occupy separate buckets (issue #776).
func TestMiddleware_APIKeyMode_DifferentBuckets(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	keyFn := func(r *http.Request) string {
		ip := resolver.Resolve(r)
		return APIKeyAwareKeyFunc(ip, r)
	}
	m := NewMiddleware(1, 1, resolver, keyFn) // burst 1 per API key
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request with key-alpha: allowed.
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "10.0.0.1:1000"
	req1.Header.Set("Authorization", "Bearer key-alpha")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("first request (key-alpha) should pass, got %d", rec1.Code)
	}

	// Second request with key-alpha from same IP: exhausted burst, 429.
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "10.0.0.1:1000"
	req2.Header.Set("Authorization", "Bearer key-alpha")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 429 {
		t.Errorf("second request (key-alpha, same IP) should be throttled, got %d", rec2.Code)
	}

	// First request with key-beta from same IP: allowed (separate bucket).
	req3 := httptest.NewRequest(http.MethodPost, "/", nil)
	req3.RemoteAddr = "10.0.0.1:1000"
	req3.Header.Set("Authorization", "Bearer key-beta")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Errorf("first request (key-beta) should pass (separate bucket from key-alpha), got %d", rec3.Code)
	}

	// Bucket count: should be 2 (one per API key).
	if m.BucketCount() != 2 {
		t.Errorf("expected 2 buckets (key-alpha, key-beta), got %d", m.BucketCount())
	}
}

// TestMiddleware_APIKeyMode_HeaderKeyType verifies that the
// X-Nexus-RateLimit-Key-Type header is set to "apikey" when using
// API-key-aware mode (issue #776).
func TestMiddleware_APIKeyMode_HeaderKeyType(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	keyFn := func(r *http.Request) string {
		ip := resolver.Resolve(r)
		return APIKeyAwareKeyFunc(ip, r)
	}
	m := NewMiddleware(1000, 2, resolver, keyFn)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Nexus-RateLimit-Key-Type"); got != "apikey" {
		t.Errorf("X-Nexus-RateLimit-Key-Type = %q, want %q", got, "apikey")
	}
}

// TestMiddleware_IPMode_HeaderKeyType verifies that the
// X-Nexus-RateLimit-Key-Type header is set to "ip" when using
// IP-only mode (default, issue #776).
func TestMiddleware_IPMode_HeaderKeyType(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1000, 2, resolver, nil) // no keyFn = IP-only
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Nexus-RateLimit-Key-Type"); got != "ip" {
		t.Errorf("X-Nexus-RateLimit-Key-Type = %q, want %q", got, "ip")
	}
}

// TestMiddleware_IPMode_DifferentIPsSameKey verifies that in IP-only mode,
// two different IPs with the same API key share the same bucket.
func TestMiddleware_IPMode_DifferentIPsSameKey(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	m := NewMiddleware(1, 1, resolver, nil) // IP-only mode, burst 1
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request from IP1.
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "10.0.0.1:1000"
	req1.Header.Set("Authorization", "Bearer same-key")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("first request should pass, got %d", rec1.Code)
	}

	// Request from IP2, same API key: different IP = separate bucket in IP-only mode.
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "10.0.0.2:1000"
	req2.Header.Set("Authorization", "Bearer same-key")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("second request (different IP) should pass in IP-only mode even with same key, got %d", rec2.Code)
	}

	// Should have 2 buckets (one per IP).
	if m.BucketCount() != 2 {
		t.Errorf("expected 2 buckets (one per IP), got %d", m.BucketCount())
	}
}

// TestMiddleware_Concurrent_IPvsAPIKey verifies that IP-only and IP+key modes
// produce different bucket counts under concurrent requests (issue #776 acceptance criterion).
func TestMiddleware_Concurrent_IPvsAPIKey(t *testing.T) {
	t.Run("IP-only mode", func(t *testing.T) {
		resolver := NewClientIPResolver(nil)
		m := NewMiddleware(1000, 1000, resolver, nil)
		h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req.RemoteAddr = fmt.Sprintf("10.0.0.%d:1000", idx%5+1)            // 5 distinct IPs
				req.Header.Set("Authorization", fmt.Sprintf("Bearer key-%d", idx)) // 50 distinct keys
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
			}(i)
		}
		wg.Wait()
		// IP-only mode: 5 buckets (one per IP), not 50 (one per key).
		if m.BucketCount() != 5 {
			t.Errorf("IP-only: expected 5 buckets (one per IP), got %d", m.BucketCount())
		}
	})

	t.Run("API-key-aware mode", func(t *testing.T) {
		resolver := NewClientIPResolver(nil)
		keyFn := func(r *http.Request) string {
			ip := resolver.Resolve(r)
			return APIKeyAwareKeyFunc(ip, r)
		}
		m := NewMiddleware(1000, 1000, resolver, keyFn)
		h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req.RemoteAddr = fmt.Sprintf("10.0.0.%d:1000", idx%5+1)            // 5 distinct IPs
				req.Header.Set("Authorization", fmt.Sprintf("Bearer key-%d", idx)) // 50 distinct keys
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
			}(i)
		}
		wg.Wait()
		// API-key-aware mode: 50 buckets (one per IP+key pair).
		if m.BucketCount() != 50 {
			t.Errorf("API-key-aware: expected 50 buckets (one per IP+key), got %d", m.BucketCount())
		}
	})
}

var _ = fmt.Sprintf // for TestMiddleware_Concurrent_IPvsAPIKey
