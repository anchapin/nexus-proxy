// Package ratelimit provides trusted-proxy-aware client-IP resolution
// and rate limiting. This file implements auth-brute-force protection
// (issue #296): a per-IP sliding-window failure counter that blocks
// clients after too many consecutive auth failures.
package ratelimit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AuthLimiter tracks per-client-IP auth failures and enforces a
// sliding-window block after AuthRateLimitBurst failures within the
// window. It is safe for concurrent use.
type AuthLimiter struct {
	rpm    int           // steady-state requests per minute (refill rate)
	burst  int           // max failures before block
	window time.Duration // sliding window for failure tracking

	onBlock   func(reason string) // called when a client is blocked with reason "missing" or "invalid"; must not block
	onReap    func()              // called when the reaper evicts an idle IP; must not block
	resolver  *ClientIPResolver   // resolves client IP for rate-limit bucketing

	mu       sync.Mutex
	failures map[string]*authFailure // keyed by resolved client IP
	stopCh   chan struct{}           // closed when reaper should exit
}

// authFailure tracks failure timestamps for one client IP.
type authFailure struct {
	mu          sync.Mutex
	missingTs   []time.Time // missing-token failure timestamps within the window
	invalidTs   []time.Time // invalid-token failure timestamps within the window
	blockReason string      // reason that triggered the block: "missing" or "invalid"
	lastSeen    time.Time   // for idle reaping
}

// NewAuthLimiter constructs an AuthLimiter. A non-positive rpm produces
// a no-op limiter (all IsBlocked calls return false). resolver may be nil;
// a nil resolver uses the direct peer IP (trust-nobody).
func NewAuthLimiter(rpm, burst int, window time.Duration, resolver *ClientIPResolver) *AuthLimiter {
	if rpm <= 0 {
		return &AuthLimiter{rpm: 0}
	}
	if burst <= 0 {
		burst = 3
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	al := &AuthLimiter{
		rpm:      rpm,
		burst:    burst,
		window:   window,
		resolver: resolver,
		failures: make(map[string]*authFailure),
		stopCh:   make(chan struct{}),
	}
	go al.reaper()
	return al
}

// SetOnBlock installs a callback invoked when a client is blocked.
// The callback receives the reason ("missing" or "invalid") that triggered
// the block and must not block. Pass nil to remove a previously installed
// callback.
func (al *AuthLimiter) SetOnBlock(fn func(reason string)) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	al.onBlock = fn
}

// SetOnReap installs a callback invoked each time the reaper evicts an
// idle IP from the failures map. The callback must not block. Pass nil
// to remove a previously installed callback.
func (al *AuthLimiter) SetOnReap(fn func()) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	al.onReap = fn
}

// SetRPM updates the steady-state failures per minute. A value <= 0
// disables the limiter (all IsBlocked calls return false). Safe to call
// while the server is running. Exposed for SIGHUP-based config hot reload
// (issue #895).
func (al *AuthLimiter) SetRPM(rpm int) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	al.rpm = rpm
}

// SetBurst updates the maximum failures before a block. A value <= 0
// leaves the burst unchanged. Safe to call while the server is running.
// Exposed for SIGHUP-based config hot reload (issue #895).
func (al *AuthLimiter) SetBurst(burst int) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	if burst > 0 {
		al.burst = burst
	}
}

// SetWindow updates the sliding window duration for failure tracking.
// A value <= 0 leaves the window unchanged. Safe to call while the
// server is running. Exposed for SIGHUP-based config hot reload
// (issue #895).
func (al *AuthLimiter) SetWindow(window time.Duration) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	if window > 0 {
		al.window = window
	}
}

// IsBlocked reports whether the client at ip is currently blocked due to
// too many auth failures.
func (al *AuthLimiter) IsBlocked(ip string) bool {
	if al == nil || al.rpm <= 0 {
		return false
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	f, ok := al.failures[ip]
	if !ok {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	al.pruneLocked(f, time.Now())
	return len(f.missingTs) >= al.burst || len(f.invalidTs) >= al.burst
}

// RecordFailure notes one auth failure for the client at ip.
// reason is "missing" (no credentials) or "invalid" (wrong credentials).
func (al *AuthLimiter) RecordFailure(ip string, reason string) {
	if al == nil || al.rpm <= 0 {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	now := time.Now()
	f, ok := al.failures[ip]
	if !ok {
		f = &authFailure{lastSeen: now}
		al.failures[ip] = f
	}
	f.mu.Lock()
	switch reason {
	case "missing":
		f.missingTs = append(f.missingTs, now)
	case "invalid":
		f.invalidTs = append(f.invalidTs, now)
	}
	f.lastSeen = now
	f.mu.Unlock()

	// Check if this specific failure type hit the burst threshold.
	count := len(f.missingTs)
	if reason == "invalid" {
		count = len(f.invalidTs)
	}
	if count >= al.burst {
		f.mu.Lock()
		if f.blockReason == "" {
			f.blockReason = reason
		}
		f.mu.Unlock()
		if al.onBlock != nil {
			al.onBlock(reason)
		}
		slog.Warn("auth rate limit exceeded",
			slog.String("client_ip", ip),
			slog.String("reason", reason),
		)
	}
}

// pruneLocked removes failure timestamps older than the window from f.
// Caller must hold f.mu.
func (al *AuthLimiter) pruneLocked(f *authFailure, now time.Time) {
	cutoff := now.Add(-al.window)

	// Prune missingTs.
	i := 0
	for i < len(f.missingTs) && f.missingTs[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		f.missingTs = f.missingTs[i:]
	}

	// Prune invalidTs.
	j := 0
	for j < len(f.invalidTs) && f.invalidTs[j].Before(cutoff) {
		j++
	}
	if j > 0 {
		f.invalidTs = f.invalidTs[j:]
	}
}

// reaper periodically evicts idle failure maps to bound memory.
func (al *AuthLimiter) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			al.mu.Lock()
			now := time.Now()
			for ip, f := range al.failures {
				f.mu.Lock()
				al.pruneLocked(f, now)
				idle := now.Sub(f.lastSeen)
				f.mu.Unlock()
				if idle > 10*time.Minute && len(f.missingTs) == 0 && len(f.invalidTs) == 0 {
					delete(al.failures, ip)
					if al.onReap != nil {
						al.onReap()
					}
				}
			}
			al.mu.Unlock()
		case <-al.stopCh:
			return
		}
	}
}

// Stop signals the reaper goroutine to exit. It is safe to call on
// a disabled limiter (rpm <= 0) or nil limiter; it is a no-op in those
// cases.
func (al *AuthLimiter) Stop() {
	if al == nil || al.rpm <= 0 {
		return
	}
	close(al.stopCh)
}

// Reap triggers one reaper eviction tick synchronously. Exposed for tests
// and for direct invocation in the acceptance test. It is safe to call
// on a disabled or nil limiter; it is a no-op in those cases.
func (al *AuthLimiter) Reap() {
	if al == nil || al.rpm <= 0 {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	now := time.Now()
	for ip, f := range al.failures {
		f.mu.Lock()
		al.pruneLocked(f, now)
		idle := now.Sub(f.lastSeen)
		f.mu.Unlock()
		if idle > 10*time.Minute && len(f.missingTs) == 0 && len(f.invalidTs) == 0 {
			delete(al.failures, ip)
			if al.onReap != nil {
				al.onReap()
			}
		}
	}
}

// BucketCount returns the number of tracked client IPs. Exposed for tests.
func (al *AuthLimiter) BucketCount() int {
	if al == nil {
		return 0
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	return len(al.failures)
}

// Enabled reports whether the auth limiter is active (RPM > 0).
func (al *AuthLimiter) Enabled() bool {
	if al == nil {
		return false
	}
	return al.rpm > 0
}

// Resolver returns the configured client IP resolver. May be nil.
func (al *AuthLimiter) Resolver() *ClientIPResolver {
	if al == nil {
		return nil
	}
	return al.resolver
}

// BlockedCount returns the number of IPs currently blocked (failure count
// has reached the burst threshold). Exposed for observability gauges.
func (al *AuthLimiter) BlockedCount() int {
	if al == nil || al.rpm <= 0 {
		return 0
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	now := time.Now()
	n := 0
	for _, f := range al.failures {
		f.mu.Lock()
		al.pruneLocked(f, now)
		if len(f.missingTs) >= al.burst || len(f.invalidTs) >= al.burst {
			n++
		}
		f.mu.Unlock()
	}
	return n
}

// Wrap returns an http.Handler that applies the auth rate limit before
// delegating to next. A disabled limiter (rpm <= 0) returns next
// unchanged so the hot path is zero-cost when auth rate limiting is off.
func (al *AuthLimiter) Wrap(next http.Handler, resolver *ClientIPResolver) http.Handler {
	if al == nil || al.rpm <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := resolver.Resolve(r)
		if al.IsBlocked(ip) {
			if al.onBlock != nil {
				al.mu.Lock()
				f := al.failures[ip]
				reason := "missing"
				if f != nil {
					f.mu.Lock()
					if f.blockReason != "" {
						reason = f.blockReason
					}
					f.mu.Unlock()
				}
				al.mu.Unlock()
				al.onBlock(reason)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			enc := json.NewEncoder(w)
			_ = enc.Encode(map[string]any{
				"error": map[string]any{
					"type":    "auth_rate_limit_exceeded",
					"message": "too many authentication failures for this client",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
