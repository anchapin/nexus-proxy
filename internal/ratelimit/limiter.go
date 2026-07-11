// Package ratelimit provides a stdlib-only token-bucket rate limiter
// (issue #38) that caps both per-client-IP and aggregate request rates.
// A single runaway client cannot flood the frontier API; the global
// bucket protects the aggregate budget across all clients.
//
// When all configured rates are zero the limiter is disabled (New
// returns nil) and the middleware is a no-op pass-through, preserving
// the pre-issue-#38 behaviour exactly.
package ratelimit

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config carries the rate-limit parameters consumed by New. Zero
// values on every field produce a nil Limiter (disabled), which is
// the backward-compatible default.
type Config struct {
	// PerClientRPM is the per-client-IP rate in requests per minute.
	// Zero disables the per-client limiter.
	PerClientRPM int

	// PerClientBurst is the token-bucket capacity for per-client
	// buckets. Zero defaults to PerClientRPM so a fresh client gets
	// a full minute's worth of requests up front.
	PerClientBurst int

	// GlobalRPM is the aggregate rate across all clients. Zero
	// disables the global limiter.
	GlobalRPM int

	// GlobalBurst is the token-bucket capacity for the global
	// bucket. Zero defaults to GlobalRPM.
	GlobalBurst int
}

// staleThreshold is how long a per-client bucket may sit idle before
// the cleanup goroutine evicts it. Ten minutes is generous enough
// that an interactive agent's bursty traffic keeps its bucket, yet
// short enough that a one-off scanner does not pin memory forever.
const staleThreshold = 10 * time.Minute

// cleanupInterval is how often the background goroutine scans the
// per-client map for stale entries.
const cleanupInterval = 5 * time.Minute

// bucket is a classic token bucket. Tokens accrue at refillPerSec
// up to capacity. Each successful allow consumes exactly one token.
// All methods are safe for concurrent use; the mutex serialises
// refill + consume so two racing goroutines cannot double-spend.
type bucket struct {
	mu           sync.Mutex
	tokens       float64   // current token count (fractional during refill)
	capacity     float64   // max tokens (burst)
	refillPerSec float64   // tokens added per second
	last         time.Time // last refill time; doubles as last-access for cleanup
}

// newBucket creates a full bucket (tokens == capacity).
func newBucket(capacity, refillPerSec float64, now time.Time) *bucket {
	return &bucket{
		tokens:       capacity,
		capacity:     capacity,
		refillPerSec: refillPerSec,
		last:         now,
	}
}

// allow refills the bucket based on elapsed time, then consumes one
// token if available. Returns ok=true when a token was consumed.
// When ok is false, retryAfter indicates how long the caller should
// wait before the next token becomes available.
func (b *bucket) allow(now time.Time) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Not enough tokens. Compute when one will be available.
	if b.refillPerSec <= 0 {
		return false, time.Minute
	}
	deficit := 1 - b.tokens
	secs := math.Ceil(deficit / b.refillPerSec)
	return false, time.Duration(secs * float64(time.Second))
}

// lastAccess returns the most recent time the bucket was touched
// (refilled or created). The cleanup goroutine uses this to decide
// which entries to evict.
func (b *bucket) lastAccess() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// Limiter is a per-key token-bucket rate limiter with an optional
// global bucket. The zero value is invalid; always construct via New.
//
// A nil Limiter is treated as "disabled" everywhere it is referenced,
// so callers can leave it unset (preserving the pre-issue-#38
// unbounded behaviour) and tests can opt in per-case without
// sprinkling nil-checks.
type Limiter struct {
	cfg     Config
	global  *bucket       // nil when GlobalRPM == 0
	clients sync.Map      // key (IP) -> *bucket
	stop    chan struct{} // closes to stop the cleanup goroutine
	done    chan struct{} // closes when the cleanup goroutine exits
}

// New returns a Limiter configured per cfg. When every rate is zero
// (the backward-compatible default) New returns nil so callers can
// gate "disabled" off a single nil-check at the use site, exactly
// like concurrencylimit.New.
func New(cfg Config) *Limiter {
	if cfg.PerClientRPM <= 0 && cfg.GlobalRPM <= 0 {
		return nil
	}

	burst := cfg.PerClientBurst
	if burst <= 0 {
		burst = cfg.PerClientRPM
	}
	if burst <= 0 {
		burst = 1 // only global is active; per-client still needs >= 1
	}

	l := &Limiter{
		cfg:  Config{PerClientRPM: cfg.PerClientRPM, PerClientBurst: burst, GlobalRPM: cfg.GlobalRPM, GlobalBurst: cfg.GlobalBurst},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	if cfg.GlobalRPM > 0 {
		gBurst := cfg.GlobalBurst
		if gBurst <= 0 {
			gBurst = cfg.GlobalRPM
		}
		l.global = newBucket(float64(gBurst), float64(cfg.GlobalRPM)/60.0, time.Now())
	}

	// Start the cleanup goroutine unconditionally. When per-client
	// limiting is off the map is empty and Range is a no-op, so the
	// goroutine costs essentially nothing. Starting it unconditionally
	// keeps Close() uniform — it always has a goroutine to reap.
	go l.cleanupLoop()

	return l
}

// bucketFor returns the per-client bucket for key, creating one on
// first use. sync.Map's LoadOrStore guarantees exactly one bucket
// exists per key even under concurrent first-access.
func (l *Limiter) bucketFor(key string) *bucket {
	now := time.Now()
	capacity := float64(l.cfg.PerClientBurst)
	if capacity <= 0 {
		capacity = 1
	}
	refill := float64(l.cfg.PerClientRPM) / 60.0
	if refill <= 0 {
		refill = float64(l.cfg.PerClientRPM) / 60.0 // guard against div-by-zero
	}
	b := newBucket(capacity, refill, now)
	actual, _ := l.clients.LoadOrStore(key, b)
	return actual.(*bucket)
}

// Decision carries the result of a rate-limit check.
type Decision struct {
	Allowed    bool          // true when the request may proceed
	RetryAfter time.Duration // valid only when Allowed is false
}

// Allow reports whether key may proceed, consuming a token from both
// the global bucket (when configured) and the per-client bucket.
// Non-blocking: returns immediately in all cases.
//
// A nil receiver returns true unconditionally so call sites do not
// have to nil-check before invoking Allow.
func (l *Limiter) Allow(key string) bool {
	return l.Check(key).Allowed
}

// Check performs a rate-limit check for key and returns the full
// Decision. The global bucket is checked first; when it denies, the
// per-client bucket is not touched. When both are configured and the
// global passes but the per-client denies, the consumed global token
// is not refunded — this is standard token-bucket behaviour and the
// wasted token is negligible (the bucket refills continuously).
//
// A nil receiver always returns Decision{Allowed: true}.
func (l *Limiter) Check(key string) Decision {
	if l == nil {
		return Decision{Allowed: true}
	}

	now := time.Now()

	if l.global != nil {
		if ok, ra := l.global.allow(now); !ok {
			return Decision{Allowed: false, RetryAfter: ra}
		}
	}

	// Per-client limiting is only active when PerClientRPM > 0.
	if l.cfg.PerClientRPM > 0 {
		b := l.bucketFor(key)
		if ok, ra := b.allow(now); !ok {
			return Decision{Allowed: false, RetryAfter: ra}
		}
	}

	return Decision{Allowed: true}
}

// Close stops the background cleanup goroutine. Safe to call on a
// nil Limiter or one whose cleanup goroutine was never started
// (global-only configuration). Idempotent.
func (l *Limiter) Close() {
	if l == nil || l.stop == nil {
		return
	}
	select {
	case <-l.stop:
		// Already closed.
	default:
		close(l.stop)
	}
	<-l.done
}

// cleanupLoop periodically evicts per-client buckets that have been
// idle longer than staleThreshold. This prevents a burst of unique
// IPs (e.g. a NAT pool or a scanner sweep) from growing the map
// without bound.
func (l *Limiter) cleanupLoop() {
	defer close(l.done)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.evictStale()
		case <-l.stop:
			return
		}
	}
}

// evictStale removes all per-client buckets whose last-access time
// is older than staleThreshold. sync.Map Range + Delete is safe for
// concurrent use; a bucket that is evicted while a request is in
// flight simply gets recreated on the client's next request (with a
// fresh full bucket), which is harmless.
func (l *Limiter) evictStale() {
	cutoff := time.Now().Add(-staleThreshold)
	l.clients.Range(func(key, val any) bool {
		b := val.(*bucket)
		if b.lastAccess().Before(cutoff) {
			l.clients.Delete(key)
		}
		return true
	})
}

// ClientCount returns the current number of per-client buckets. Used
// by tests to verify the cleanup goroutine evicts stale entries.
func (l *Limiter) ClientCount() int {
	if l == nil {
		return 0
	}
	var n int
	l.clients.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// --- Middleware ----------------------------------------------------------

// Middleware returns an http.Handler wrapper that applies the rate
// limit to every request. Requests that exceed the limit receive
// HTTP 429 with:
//   - Retry-After header (seconds until the next token is available)
//   - X-Nexus-RateLimit-Remaining: "0"
//   - X-Nexus-RateLimit-Reset: unix timestamp when the client can retry
//   - OpenAI-compatible JSON error envelope
//
// The exempt predicate (when non-nil) short-circuits the check for
// specific paths (e.g. /healthz). A nil Limiter returns the handler
// unchanged so callers can unconditionally wrap without nil-checking.
func (l *Limiter) Middleware(exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			ip := ClientIP(r)
			dec := l.Check(ip)
			if !dec.Allowed {
				writeRateLimited(w, dec.RetryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP extracts the originating client IP from a request. It
// honours X-Forwarded-For (first hop, most prevalent convention behind
// reverse proxies and load balancers) and falls back to the host
// portion of RemoteAddr (stripping the port). Exported so tests and
// other middleware can reuse the same extraction logic.
func ClientIP(r *http.Request) string {
	// X-Forwarded-For: "client, proxy1, proxy2" — the leftmost
	// entry is the original client. Some proxies append; some
	// prepend; the de-facto standard is leftmost = client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first comma-separated entry and trim whitespace.
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	// Fall back to RemoteAddr ("host:port" or "[v6]:port").
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// writeRateLimited emits the 429 response with rate-limit-specific
// headers. The JSON body matches the OpenAI error envelope shape
// used throughout the proxy (`{"error":{"message":...,"type":...}}`).
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	reset := time.Now().Add(time.Duration(secs) * time.Second)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("X-Nexus-RateLimit-Remaining", "0")
	w.Header().Set("X-Nexus-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": "Rate limit exceeded. Please retry after the Retry-After delay.",
			"type":    "rate_limit_exceeded",
		},
	})
}
