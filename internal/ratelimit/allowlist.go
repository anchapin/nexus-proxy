// Package ratelimit provides trusted-proxy-aware client-IP resolution
// (issue #75) and a per-client token-bucket rate limiter.
//
// This file adds the inbound IP allowlist middleware for local-only
// deployments (issue #1240).
package ratelimit

import (
	"log/slog"
	"net"
	"net/http"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// AllowCIDRsMiddleware is an http.Handler that enforces an inbound IP
// allowlist. It permits requests only when the client's resolved IP
// falls within at least one configured CIDR; all others receive HTTP
// 403. When no CIDRs are configured, the middleware is a transparent
// no-op passthrough so a stock deployment is byte-for-byte identical to
// the pre-issue-#1240 behaviour.
//
// The middleware is safe for concurrent use. The CIDR list is atomically
// swapped on SIGHUP via SetCIDRs so in-flight requests see a consistent
// view of the old list until the swap is visible to the next request.
type AllowCIDRsMiddleware struct {
	cidrs    []*net.IPNet
	exempt   func(*http.Request) bool // returns true for exempt paths
	resolver *ClientIPResolver
	OnBlock  func() // called when a request is blocked (issue #1361)
}

// NewAllowCIDRsMiddleware constructs an allowlist middleware. A nil or
// empty cidri list produces a transparent no-op middleware (the
// allowlist is disabled). exempt may be nil (no exemptions).
func NewAllowCIDRsMiddleware(cidrs []*net.IPNet, exempt func(*http.Request) bool, resolver *ClientIPResolver) *AllowCIDRsMiddleware {
	return &AllowCIDRsMiddleware{
		cidrs:    cidrs,
		exempt:   exempt,
		resolver: resolver,
	}
}

// SetCIDRs atomically replaces the allowlist CIDRs. Issue #1240: this
// allows the SIGHUP handler to update the allowlist without constructing
// a new middleware.
func (m *AllowCIDRsMiddleware) SetCIDRs(cidrs []*net.IPNet) {
	if m == nil {
		return
	}
	m.cidrs = cidrs
}

// Enabled reports whether the allowlist is active (CIDRs are configured).
func (m *AllowCIDRsMiddleware) Enabled() bool {
	if m == nil {
		return false
	}
	return len(m.cidrs) > 0
}

// Wrap returns an http.Handler that enforces the allowlist before
// delegating to next. A disabled middleware (no CIDRs) returns next
// unchanged so the hot path is zero-cost when the allowlist is off.
// The exempt function (when non-nil) is consulted first; exempt paths
// always pass through regardless of the allowlist state.
func (m *AllowCIDRsMiddleware) Wrap(next http.Handler) http.Handler {
	if m == nil || !m.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check exemptions first.
		if m.exempt != nil && m.exempt(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Resolve the client IP using the trusted-proxy-aware resolver.
		ip := m.resolver.Resolve(r)

		var span *tracing.Span
		if tracing.Enabled() {
			r2, s := tracing.StartSpanFromContext(r.Context(), "ratelimit.allowlist.check")
			span = s
			r = r.WithContext(r2)
			defer span.End()
			span.SetAttr("client_ip", ip)
		}

		// Check if the resolved IP is in any allowed CIDR.
		if !m.anyAllowed(ip) {
			slog.Warn("ip allowlist blocked request",
				slog.String("client_ip", ip),
				slog.String("remote", r.RemoteAddr),
				slog.String("path", r.URL.Path),
			)
			if span != nil {
				span.SetAttr("allowlist.matched", false)
			}
			if m.OnBlock != nil {
				m.OnBlock()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = jsonError(w, "access denied: client IP not in allowlist")
			return
		}

		if span != nil {
			span.SetAttr("allowlist.matched", true)
		}
		next.ServeHTTP(w, r)
	})
}

// anyAllowed reports whether ip falls within any configured allowlist CIDR.
func (m *AllowCIDRsMiddleware) anyAllowed(ipStr string) bool {
	if m == nil || len(m.cidrs) == 0 {
		return true // no allowlist = allow all
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range m.cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// jsonError writes a minimal JSON error body for 403 responses.
// Uses a hardcoded JSON template to avoid encoding/json overhead.
func jsonError(w http.ResponseWriter, msg string) error {
	_, err := w.Write([]byte(`{"error":{"type":"access_denied","message":"` + msg + `"}}`))
	return err
}
