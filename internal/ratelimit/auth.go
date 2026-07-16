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

	onBlock func() // called when a client is blocked; must not block

	mu       sync.Mutex
	failures map[string]*authFailure // keyed by resolved client IP
}

// authFailure tracks failure timestamps for one client IP.
type authFailure struct {
	mu       sync.Mutex
	ts       []time.Time // failure timestamps within the window
	lastSeen time.Time   // for idle reaping
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
		failures: make(map[string]*authFailure),
	}
	go al.reaper()
	return al
}

// SetOnBlock installs a callback invoked when a client is blocked.
// The callback must not block. Pass nil to remove a previously installed
// callback.
func (al *AuthLimiter) SetOnBlock(fn func()) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	al.onBlock = fn
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
	return len(f.ts) >= al.burst
}

// RecordFailure notes one auth failure for the client at ip.
func (al *AuthLimiter) RecordFailure(ip string) {
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
	f.ts = append(f.ts, now)
	f.lastSeen = now
	f.mu.Unlock()

	if len(f.ts) >= al.burst {
		if al.onBlock != nil {
			al.onBlock()
		}
		slog.Warn("auth rate limit exceeded",
			slog.String("client_ip", ip),
		)
	}
}

// pruneLocked removes failure timestamps older than the window from f.
// Caller must hold f.mu.
func (al *AuthLimiter) pruneLocked(f *authFailure, now time.Time) {
	cutoff := now.Add(-al.window)
	i := 0
	for i < len(f.ts) && f.ts[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		f.ts = f.ts[i:]
	}
}

// reaper periodically evicts idle failure maps to bound memory.
func (al *AuthLimiter) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		al.mu.Lock()
		now := time.Now()
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
				al.onBlock()
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
