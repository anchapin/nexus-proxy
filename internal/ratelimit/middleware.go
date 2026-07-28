// (continuation of package ratelimit; see clientip.go for the
// package-level docs and the trusted-proxy enforcement rationale.)

package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anchapin/nexus-proxy/internal/tracing"
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
	stopCh   chan struct{} // closed when reaper should exit

	// keyFn computes the bucket key from an inbound request. It composes
	// on top of the ClientIPResolver. The default (nil) uses IP-only;
	// when set to APIKeyAwareKeyFunc it uses SHA256(IP + ":" + APIKey)
	// truncated to 8 hex chars (issue #776).
	keyFn func(*http.Request) string

	// keyType records which key mode is active, exposed via the
	// X-Nexus-RateLimit-Key-Type response header.
	keyType string

	// onReject, when non-nil, is invoked once for each request the
	// middleware rejects with 429 (issue #119). It is intended for
	// telemetry / observability hooks and must not block —
	// request goroutine calls it inline. Set via SetRejectionHook
	// after construction so NewMiddleware stays a pure constructor.
	onReject func()

	// onAllow, when non-nil, is invoked once for each request the
	// middleware allows (issue #746). It receives the hashed bucket ID
	// and the fractional token utilization (tokens/burst) at the moment
	// of acquisition, before the token is consumed. Intended for the
	// rate-limit bucket utilization histogram. Must not block.
	onAllow func(bucketID string, utilizationPct float64)

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
//
// keyFn, when non-nil, computes the per-request bucket key. It receives
// the resolved client IP and the inbound request; it returns the string
// used as the bucket map key. The default (nil) uses IP-only. When
// NEXUS_RATE_LIMIT_BY_API_KEY=true the caller passes APIKeyAwareKeyFunc
// so different API keys behind the same IP occupy separate buckets
// (issue #776).
func NewMiddleware(rpm, burst int, resolver *ClientIPResolver, keyFn func(*http.Request) string) *Middleware {
	if rpm <= 0 {
		return &Middleware{rpm: 0}
	}
	if burst <= 0 {
		burst = rpm
	}
	keyType := "ip"
	if keyFn != nil {
		keyType = "apikey"
	}
	return &Middleware{
		resolver: resolver,
		rpm:      rpm,
		burst:    burst,
		ttl:      10 * time.Minute, // reap buckets idle for 10 min
		stopCh:   make(chan struct{}),
		buckets:  make(map[string]*bucket),
		keyFn:    keyFn,
		keyType:  keyType,
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

// SetAllowHook installs a callback invoked once per allowed request
// before the token is consumed (issue #746). fn receives the hashed
// bucket ID and the fractional token utilization (tokens/burst) at the
// moment of acquisition. Pass nil to remove a previously installed hook.
func (m *Middleware) SetAllowHook(fn func(bucketID string, utilizationPct float64)) {
	if m == nil {
		return
	}
	m.onAllow = fn
}

// hashedBucketKey returns a SHA256 hash of input truncated to 8 hex characters,
// suitable for use as a high-cardinality-safe bucket identifier in
// telemetry labels.
func hashedBucketKey(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:4])
}

// APIKeyAwareKeyFunc returns a key-fn that composes the resolved client
// IP with the Bearer API key from the Authorization header, producing
// SHA256(IP + ":" + APIKey) truncated to 8 hex chars. When no Authorization
// header is present the key falls back to IP-only so unauthenticated
// requests are still bucketed by IP (the auth middleware runs before the
// rate limiter so this is only hit in health-check / metrics paths).
//
// The returned function is safe for concurrent use.
func APIKeyAwareKeyFunc(ip string, r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ip
	}
	// Strip "Bearer " prefix if present (case-insensitive); also normalise
	// token to lowercase so "bearer MYKEY" and "Bearer mykey" share a bucket.
	var key string
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		key = strings.ToLower(strings.TrimSpace(auth[7:]))
	} else {
		key = auth
	}
	h := sha256.Sum256([]byte(ip + ":" + key))
	return hex.EncodeToString(h[:4])
}

// Wrap returns an http.Handler that applies the rate limit before
// delegating to next. A disabled middleware (rpm <= 0) returns next
// unchanged so the hot path is zero-cost when rate limiting is off.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if m == nil || m.rpm <= 0 {
		return next
	}
	// Kick off the idle-bucket reaper once. It exits when Close() / Stop()
	// is called (issue #739). The middleware lives for the lifetime of the
	// server, so Wrap is called exactly once per middleware instance.
	go m.reaper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := m.resolver.Resolve(r)
		bucketKey := ip
		if m.keyFn != nil {
			bucketKey = m.keyFn(r)
		}
		var span *tracing.Span
		if tracing.Enabled() {
			r2, s := tracing.StartSpanFromContext(r.Context(), "ratelimit.check")
			span = s
			r = r.WithContext(r2)
			defer span.End()
			span.SetAttr("ratelimit.key_type", m.keyType)
		}
		allowed := m.allow(bucketKey, ip, time.Now())
		if span != nil {
			span.SetAttr("ratelimit.allowed", allowed)
		}
		if !allowed {
			if span != nil {
				span.SetAttr("ratelimit.reason", "rate_exceeded")
			}
			if m.onReject != nil {
				m.onReject()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.Header().Set("X-Nexus-RateLimit-Remaining", "0")
			w.Header().Set("X-Nexus-RateLimit-Key-Type", m.keyType)
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
		w.Header().Set("X-Nexus-RateLimit-Key-Type", m.keyType)
		next.ServeHTTP(w, r)
	})
}

// allow reports whether the client identified by bucketKey may issue a
// request now, consuming one token if so. ip is used only for the
// telemetry callback (bucketID is computed from bucketKey). It lazily
// creates the bucket and refills it from the elapsed time since the
// last request.
func (m *Middleware) allow(bucketKey, ip string, now time.Time) bool {
	b := m.bucketFor(bucketKey, now)

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
		if m.onAllow != nil {
			m.onAllow(hashedBucketKey(bucketKey), float64(b.tokens)/float64(m.burst))
		}
		b.tokens--
		return true
	}
	return false
}

// bucketFor returns the bucket for bucketKey, creating it on first sighting.
// All bucket allocation and map insertion happen atomically inside the
// per-map critical section so no bucket is ever orphaned by a concurrent
// bucketFor call for the same key. The per-bucket lock (in allow)
// serializes token consumption.
func (m *Middleware) bucketFor(bucketKey string, now time.Time) *bucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.buckets[bucketKey]; ok {
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
	m.buckets[bucketKey] = b
	return b
}

// reaper periodically evicts idle buckets to bound memory. It is the
// only goroutine that deletes from the map outside of allow (which
// only ever adds).
func (m *Middleware) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.reap(time.Now())
		case <-m.stopCh:
			return
		}
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

// Stop signals the reaper goroutine to exit. It is safe to call on
// a disabled limiter (rpm <= 0) or nil limiter; it is a no-op in those
// cases.
func (m *Middleware) Stop() {
	if m == nil || m.rpm <= 0 {
		return
	}
	close(m.stopCh)
}

// Close is an alias for Stop, provided to mirror the closer interface
// pattern used by other shutdown-aware components.
func (m *Middleware) Close() {
	m.Stop()
}

// SetRPM updates the steady-state requests per minute. A value <= 0
// disables the limiter (transparent passthrough). Safe to call while
// the server is running; the new value is used on the next Allow call.
// Exposed for SIGHUP-based config hot reload (issue #306).
func (m *Middleware) SetRPM(rpm int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rpm = rpm
}

// SetBurst updates the bucket capacity. A value <= 0 leaves the burst
// unchanged. Safe to call while the server is running. Exposed for
// SIGHUP-based config hot reload (issue #306).
func (m *Middleware) SetBurst(burst int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if burst > 0 {
		m.burst = burst
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

// RPM returns the configured requests-per-minute rate. Returns 0 when
// the middleware is disabled.
func (m *Middleware) RPM() int {
	if m == nil {
		return 0
	}
	return m.rpm
}

// Burst returns the configured token-bucket burst capacity. Returns 0
// when the middleware is disabled.
func (m *Middleware) Burst() int {
	if m == nil {
		return 0
	}
	return m.burst
}

// Enabled reports whether rate limiting is active (RPM > 0).
func (m *Middleware) Enabled() bool {
	if m == nil {
		return false
	}
	return m.rpm > 0
}
