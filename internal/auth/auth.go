// Package auth implements the inbound API-key gateway (issue #37/#109).
//
// When NEXUS_PROXY_API_KEY is set, every non-exempt endpoint requires
// a matching Bearer token in the Authorization header. Endpoints used
// by infrastructure probes (/healthz, /metrics) are exempt so K8s
// liveness probes and Prometheus scrapers continue to work without
// credentials. The /status endpoint is exempt only when
// NEXUS_STATUS_PUBLIC=true (default false).
//
// Auth brute-force protection (issue #296): when an auth limiter is wired
// in, failed auth attempts are tracked per client IP and the client is
// temporarily blocked after too many failures.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/ratelimit"
	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// AuthObserver is the interface for receiving auth lifecycle callbacks.
// The observability.Collector implements this interface (issue #295).
type AuthObserver interface {
	IncAuthAccepted(clientIP string)
	IncAuthRejectedInvalid(clientIP string)
	IncAuthRejectedMissing(clientIP string)
}

// clientSlot tracks an in-progress auth attempt for one client IP.
// The channel is closed when the auth attempt completes, allowing
// a new slot to be acquired. This prevents a slow attacker from
// holding a slot indefinitely and blocking other IPs (issue #1062).
type clientSlot struct {
	ch       chan struct{} // closed when auth attempt completes
	lastSeen time.Time
}

// Middleware gates HTTP requests behind a bearer token. When key is
// empty the middleware is a pass-through (auth disabled), so a
// development proxy with no NEXUS_PROXY_API_KEY behaves identically
// to the pre-auth binary.
//
// Multi-key support (issue #1154): when creds is non-nil and non-empty,
// the middleware validates the token against every credential in the set
// and, on match, resolves the tenant identifier and places it on the
// request context via auth.WithTenant. The multi-key path takes
// precedence over the single-key path so an operator can migrate from
// NEXUS_PROXY_API_KEY to NEXUS_API_KEYS_FILE without downtime.
//
// Pluggable authenticator (issue #1152): when authenticator is non-nil,
// JWT/OIDC token validation is delegated to it. The authenticator path
// is checked after multi-key and before the single-key fallback.
type Middleware struct {
	key           string
	creds         *CredentialSet // multi-key set; nil means single-key legacy path
	authenticator Authenticator  // optional: issue #1152 pluggable JWT/OIDC auth
	exempt        func(*http.Request) bool
	authLimiter   *ratelimit.AuthLimiter
	observer      AuthObserver
	resolver      *ratelimit.ClientIPResolver

	mu    sync.Mutex
	slots map[string]*clientSlot // keyed by client IP

	// nexus_auth_requests_total{mode,result} counter (issue #1305).
	// mode: "multikey" | "authenticator" | "single"
	// result: "success" | "invalid" | "missing"
	authRequestsTotal map[string]*uint64 // keyed by "mode:result"
}

// incAuthRequest increments the nexus_auth_requests_total counter for the given
// mode and result. Safe for concurrent use.
func (m *Middleware) incAuthRequest(mode, result string) {
	key := mode + ":" + result
	m.mu.Lock()
	p, ok := m.authRequestsTotal[key]
	if !ok {
		v := uint64(0)
		p = &v
		m.authRequestsTotal[key] = p
	}
	m.mu.Unlock()
	atomic.AddUint64(p, 1)
}

// WritePrometheusMetrics writes the nexus_auth_requests_total counter family
// to w in Prometheus text exposition format. Safe for concurrent use; nil
// receivers are a no-op.
func (m *Middleware) WritePrometheusMetrics(w io.Writer) {
	if m == nil || m.authRequestsTotal == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Write HELP/TYPE header.
	//nolint:errcheck // Writer error cannot be handled after partial write.
	fmt.Fprintf(w, "# HELP nexus_auth_requests_total Authentication requests by mode and result (issue #1305).\n")
	//nolint:errcheck // Writer error cannot be handled after partial write.
	fmt.Fprintf(w, "# TYPE nexus_auth_requests_total counter\n")

	// Collect and sort keys for deterministic output.
	keys := make([]string, 0, len(m.authRequestsTotal))
	for k := range m.authRequestsTotal {
		keys = append(keys, k)
	}
	for _, k := range keys {
		parts := strings.SplitN(k, ":", 2)
		if len(parts) != 2 {
			continue
		}
		mode, result := parts[0], parts[1]
		v := atomic.LoadUint64(m.authRequestsTotal[k])
		//nolint:errcheck // Writer error cannot be handled after partial write.
		fmt.Fprintf(w, "nexus_auth_requests_total{mode=%q,result=%q} %d\n", mode, result, v)
	}
}

// NewMiddleware returns a middleware that rejects requests without a
// matching Bearer token, unless exempt(r) returns true. A empty key
// disables auth entirely (Wrap returns the handler unchanged).
// When authLimiter is non-nil, brute-force protection is enabled:
// the client IP is checked against the limiter before auth, and failures
// are reported to the limiter.
// When observer is non-nil, auth rejection counters are incremented on
// 401 responses (issue #295).
func NewMiddleware(key string, exempt func(*http.Request) bool, authLimiter *ratelimit.AuthLimiter, observer AuthObserver) *Middleware {
	var resolver *ratelimit.ClientIPResolver
	if authLimiter != nil {
		resolver = authLimiter.Resolver()
	} else {
		resolver = ratelimit.NewClientIPResolver(nil)
	}
	if resolver == nil {
		resolver = ratelimit.NewClientIPResolver(nil)
	}
	return &Middleware{
		key:               key,
		exempt:            exempt,
		authLimiter:       authLimiter,
		observer:          observer,
		resolver:          resolver,
		slots:             make(map[string]*clientSlot),
		authRequestsTotal: make(map[string]*uint64),
	}
}

// NewMultiKeyMiddleware returns a middleware that validates the token
// against a CredentialSet and resolves the tenant on match (issue #1154).
// When creds is nil or empty, falls back to single-key mode with key.
func NewMultiKeyMiddleware(key string, creds *CredentialSet, exempt func(*http.Request) bool, authLimiter *ratelimit.AuthLimiter, observer AuthObserver) *Middleware {
	m := NewMiddleware(key, exempt, authLimiter, observer)
	m.creds = creds
	return m
}

// SetCredentials replaces the credential set at runtime (SIGHUP
// hot-reload). Safe to call while the middleware is serving requests.
// Passing nil disables the multi-key path.
func (m *Middleware) SetCredentials(creds *CredentialSet) {
	m.mu.Lock()
	m.creds = creds
	m.mu.Unlock()
}

// NewMiddlewareWithAuthenticator returns a middleware that validates
// tokens using the provided Authenticator (issue #1152). The key
// parameter should be non-empty so Enabled() returns true; it is used
// only for the enabled/disabled gate. When the authenticator is set,
// token validation is delegated to it instead of the constant-time
// comparison.
func NewMiddlewareWithAuthenticator(key string, authenticator Authenticator, exempt func(*http.Request) bool, authLimiter *ratelimit.AuthLimiter, observer AuthObserver) *Middleware {
	m := NewMiddleware(key, exempt, authLimiter, observer)
	m.authenticator = authenticator
	return m
}

// Enabled reports whether the middleware actually enforces auth.
func (m *Middleware) Enabled() bool {
	m.mu.Lock()
	hasCreds := m.creds != nil && m.creds.Len() > 0
	hasAuth := m.authenticator != nil
	m.mu.Unlock()
	return m.key != "" || hasCreds || hasAuth
}

// acquireSlot acquires an auth slot for the given IP. If the IP already has
// a slot with an open channel (previous auth attempt still in progress),
// the old channel is closed and a new slot is created. This prevents a slow
// attacker from holding a slot indefinitely and blocking other IPs (issue #1062).
func (m *Middleware) acquireSlot(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slot, ok := m.slots[ip]; ok {
		select {
		case <-slot.ch:
			delete(m.slots, ip)
		default:
			close(slot.ch)
			m.slots[ip] = &clientSlot{ch: make(chan struct{}), lastSeen: time.Now()}
		}
	} else {
		m.slots[ip] = &clientSlot{ch: make(chan struct{}), lastSeen: time.Now()}
	}
}

// renewSlot closes the current auth slot for the given IP and creates a new one.
// Called when auth completes (success or failure) so subsequent requests
// from the same IP can proceed.
func (m *Middleware) renewSlot(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slot, ok := m.slots[ip]; ok {
		select {
		case <-slot.ch:
		default:
			close(slot.ch)
		}
		m.slots[ip] = &clientSlot{ch: make(chan struct{}), lastSeen: time.Now()}
	}
}

// Wrap returns an http.Handler that enforces the bearer-token gate.
// When auth is disabled (empty key) the handler is returned as-is.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if !m.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var span *tracing.Span
		if tracing.Enabled() {
			r2, s := tracing.StartSpanFromContext(r.Context(), "auth.check")
			span = s
			r = r.WithContext(r2)
			defer span.End()
		}

		clientIP := m.resolver.Resolve(r)
		exempt := m.exempt != nil && m.exempt(r)
		token := BearerToken(r)
		tokenPresent := token != ""

		if span != nil {
			span.SetAttr("auth.exempt", exempt)
			span.SetAttr("auth.token_present", tokenPresent)
		}

		if m.authLimiter != nil && m.authLimiter.Enabled() {
			if m.authLimiter.IsBlocked(clientIP) {
				if span != nil {
					span.SetAttr("auth.outcome", "reject")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "60")
				w.Header().Set("X-Nexus-RateLimit-Key-Type", "auth-brute-force")
				w.WriteHeader(http.StatusTooManyRequests)
				enc := json.NewEncoder(w)
				_ = enc.Encode(map[string]any{
					"error": map[string]any{
						"type":    "auth_rate_limit_exceeded",
						"message": "too many authentication failures for this client",
					},
				})
				slog.Warn("auth rate limit exceeded",
					slog.String("client_ip", clientIP),
				)
				return
			}
		}

		m.acquireSlot(clientIP)

		if exempt {
			if span != nil {
				span.SetAttr("auth.outcome", "accept")
			}
			next.ServeHTTP(w, r)
			m.renewSlot(clientIP)
			return
		}
		if token == "" {
			if span != nil {
				span.SetAttr("auth.outcome", "reject")
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"missing or malformed Authorization header"}`)
			if m.observer != nil {
				m.observer.IncAuthRejectedMissing(clientIP)
			}
			if m.authLimiter != nil && m.authLimiter.Enabled() {
				m.authLimiter.RecordFailure(clientIP, "missing")
			}
			m.incAuthRequest("unknown", "missing")
			m.renewSlot(clientIP)
			return
		}
		// Validate the token. Three paths (checked in order):
		// 1. Multi-key (issue #1154): match against the CredentialSet,
		//    resolve tenant, and place it on the request context.
		// 2. Pluggable authenticator (issue #1152): delegate to the
		//    Authenticator (e.g. JWT/OIDC validator).
		// 3. Single-key (legacy): constant-time compare against m.key.
		m.mu.Lock()
		creds := m.creds
		authenticator := m.authenticator
		m.mu.Unlock()

		if creds != nil && creds.Len() > 0 {
			tenant, ok := creds.Match(token)
			if !ok {
				if span != nil {
					span.SetAttr("auth.outcome", "invalid")
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy", error="invalid_token"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"error":"invalid API key"}`)
				if m.observer != nil {
					m.observer.IncAuthRejectedInvalid(clientIP)
				}
				if m.authLimiter != nil && m.authLimiter.Enabled() {
					m.authLimiter.RecordFailure(clientIP, "invalid")
				}
				m.incAuthRequest("multikey", "invalid")
				m.renewSlot(clientIP)
				return
			}
			if span != nil {
				span.SetAttr("auth.outcome", "accept")
				span.SetAttr("auth.tenant", tenant)
			}
			r = r.WithContext(WithTenant(r.Context(), tenant))
			next.ServeHTTP(w, r)
			if m.observer != nil {
				m.observer.IncAuthAccepted(clientIP)
			}
			m.incAuthRequest("multikey", "success")
			m.renewSlot(clientIP)
			return
		}

		// Pluggable authenticator path (issue #1152). When an
		// Authenticator is wired (JWT/OIDC), delegate token validation
		// to it instead of the constant-time comparison.
		if authenticator != nil {
			if err := authenticator.Authenticate(token); err != nil {
				if span != nil {
					span.SetAttr("auth.outcome", "invalid")
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy", error="invalid_token"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"error":"invalid token"}`)
				if m.observer != nil {
					m.observer.IncAuthRejectedInvalid(clientIP)
				}
				if m.authLimiter != nil && m.authLimiter.Enabled() {
					m.authLimiter.RecordFailure(clientIP, "invalid")
				}
				m.incAuthRequest("authenticator", "invalid")
				m.renewSlot(clientIP)
				return
			}
			if span != nil {
				span.SetAttr("auth.outcome", "accept")
			}
			next.ServeHTTP(w, r)
			if m.observer != nil {
				m.observer.IncAuthAccepted(clientIP)
			}
			m.incAuthRequest("authenticator", "success")
			m.renewSlot(clientIP)
			return
		}

		// Use crypto/subtle.ConstantTimeCompare to prevent timing attacks
		// (issue #228). The == 0 return value means the strings differ.
		if subtle.ConstantTimeCompare([]byte(token), []byte(m.key)) == 0 {
			if span != nil {
				span.SetAttr("auth.outcome", "invalid")
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy", error="invalid_token"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"invalid API key"}`)
			if m.observer != nil {
				m.observer.IncAuthRejectedInvalid(clientIP)
			}
			if m.authLimiter != nil && m.authLimiter.Enabled() {
				m.authLimiter.RecordFailure(clientIP, "invalid")
			}
			m.incAuthRequest("single", "invalid")
			m.renewSlot(clientIP)
			return
		}
		if span != nil {
			span.SetAttr("auth.outcome", "accept")
		}
		next.ServeHTTP(w, r)
		if m.observer != nil {
			m.observer.IncAuthAccepted(clientIP)
		}
		m.incAuthRequest("single", "success")
		m.renewSlot(clientIP)
	})
}

// BearerToken extracts the token from the Authorization header.
// Returns "" if the header is absent, malformed, or not a Bearer
// scheme. The comparison is case-insensitive on the scheme name.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
