// (continuation of package ratelimit; see clientip.go for the
// package-level docs and the trusted-proxy enforcement rationale.)

package ratelimit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Middleware is an http.Handler decorator that bounds the number of
// requests each effective client IP may issue per unit time, using a
// ClientIPResolver to decide which IP to bucket on. It implements a
// classic token bucket per client:
//
//   - RPM:  steady-state refill rate, in requests per minute.
//   - Burst: bucket capacity (max requests in a burst before throttling).
//
// When a client exhausts its bucket the middleware responds 429 Too Many
// Requests with a small JSON body and an X-Nexus-RateLimit-Reset header
// (seconds until the next token is available). Successful requests get
// X-Nexus-RateLimit-Remaining on the response.
//
// The middleware is safe for concurrent use. Buckets are created lazily
// on first sighting of an IP and pruned periodically by the background
// reaper to bound memory growth. The zero value is a no-op passthrough
// (RPM <= 0); always construct via NewMiddleware.
type Middleware struct {
	resolver *ClientIPResolver
	rpm      int           // steady-state requests per minute
	burst    int           // bucket capacity
	ttl      time.Duration // idle bucket retention before reaping

	// onReject, when non-nil, is invoked once for each request the
	// middleware rejects with 429 (issue #119). It is intended for
	// telemetry / observability hooks and must not block — the
	// request goroutine calls it inline. Set via SetRejectionHook
	// after construction so NewMiddleware stays a pure constructor.
	onReject func()

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is a per-client token bucket. The refiller is implicit: we
// compute tokens on each Acquire from the elapsed time since lastRefill
// rather than running a goroutine per client (which would be wasteful
// at scale).
type bucket struct {
	mu         sync.Mutex
	tokens     float64   // current token count (fractional under the hood)
	lastRefill time.Time // wall time of the last refill computation
	lastSeen   time.Time // for the idle reaper
}

// NewMiddleware constructs a rate-limit middleware. A non-positive rpm
// produces a no-op middleware (the wrapper is a transparent
// passthrough) so a stock deployment with no NEXUS_RATE_LIMIT_RPM is
// byte-for-byte identical to the pre-issue-#75 behaviour. burst <= 0
// falls back to rpm (one second's worth at full rate) so an operator
// who sets only RPM still gets a sane capacity. resolver may be nil; a
// nil resolver uses the direct peer IP (trust-nobody).
func NewMiddleware(rpm, burst int, resolver *ClientIPResolver) *Middleware {
	if rpm <= 0 {
		return &Middleware{rpm: 0}
	}
	if burst <= 0 {
		burst = rpm
	}
	return &Middleware{
		resolver: resolver,
		rpm:      rpm,
		burst:    burst,
		ttl:      10 * time.Minute, // reap buckets idle for 10 min
		buckets:  make(map[string]*bucket),
	}
}

// SetRejectionHook installs a callback invoked once per 429 rejection
// (issue #119). fn must be safe to call from many goroutines and must
// not block. Pass nil to remove a previously installed hook. The
// method is safe to call before Wrap binds the handler; main.go wires
// it between NewMiddleware and the server start.
func (m *Middleware) SetRejectionHook(fn func()) {
	if m == nil {
		return
	}
	m.onReject = fn
}

// Wrap returns an http.Handler that applies the rate limit before
// delegating to next. A disabled middleware (rpm <= 0) returns next
// unchanged so the hot path is zero-cost when rate limiting is off.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if m == nil || m.rpm <= 0 {
		return next
	}
	// Kick off the idle-bucket reaper once. It stops itself when the
	// process exits; there is no Close because the middleware lives for
	// the lifetime of the server.
	go m.reaper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := m.resolver.Resolve(r)
		if !m.allow(ip, time.Now()) {
			if m.onReject != nil {
				m.onReject()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.Header().Set("X-Nexus-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"type":    "rate_limit_exceeded",
					"message": "rate limit exceeded for this client",
				},
			})
			slog.Warn("rate limit exceeded",
				slog.String("client_ip", ip),
				slog.String("remote", r.RemoteAddr),
				slog.String("path", r.URL.Path),
			)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow reports whether ip may issue a request now, consuming one token
// if so. It lazily creates the bucket and refills it from the elapsed
// time since the last request.
func (m *Middleware) allow(ip string, now time.Time) bool {
	b := m.bucketFor(ip, now)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill: ratePerSecond tokens per second. We carry fractional
	// tokens so a client that waits a partial second still accrues.
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		refill := elapsed * float64(m.rpm) / 60.0
		b.tokens += refill
		if b.tokens > float64(m.burst) {
			b.tokens = float64(m.burst)
		}
		b.lastRefill = now
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// bucketFor returns the bucket for ip, creating it on first sighting.
// All bucket allocation and map insertion happen atomically inside the
// per-map critical section so no bucket is ever orphaned by a concurrent
// bucketFor call for the same IP. The per-bucket lock (in allow)
// serializes token consumption.
func (m *Middleware) bucketFor(ip string, now time.Time) *bucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.buckets[ip]; ok {
		return b
	}
	// Start full so a brand-new client gets its full burst.
	// Allocation is deliberately inside the critical section so that
	// the pointer is never accessible to another goroutine until it is
	// safely inserted into the map (fixes issue #248 race window).
	b := &bucket{
		tokens:     float64(m.burst),
		lastRefill: now,
		lastSeen:   now,
	}
	m.buckets[ip] = b
	return b
}

// reaper periodically evicts idle buckets to bound memory. It is the
// only goroutine that deletes from the map outside of allow (which
// only ever adds).
func (m *Middleware) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.reap(time.Now())
	}
}

// reap evicts buckets whose lastSeen is older than the TTL. Exposed
// (package-private) so tests can drive it deterministically without
// waiting a real minute.
func (m *Middleware) reap(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ip, b := range m.buckets {
		b.mu.Lock()
		idle := now.Sub(b.lastSeen)
		b.mu.Unlock()
		if idle > m.ttl {
			delete(m.buckets, ip)
		}
	}
}

// BucketCount returns the number of tracked client buckets. Exposed for
// /healthz diagnostics and tests.
func (m *Middleware) BucketCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}
