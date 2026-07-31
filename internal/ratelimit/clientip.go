// Package ratelimit provides trusted-proxy-aware client-IP resolution
// (issue #75) and a per-client token-bucket rate limiter.
//
// # Why this package exists
//
// Rate limiting is only effective if the client identity cannot be
// spoofed. If Nexus honours X-Forwarded-For unconditionally, any client
// that can reach the proxy directly can evade per-client limits by
// rotating that header. The resolver therefore honours forwarded
// headers (X-Forwarded-For / X-Real-IP) ONLY when the direct TCP peer
// is in a configured trusted-proxy CIDR allowlist. When nobody is
// trusted — the default — the resolver uses the direct peer IP and
// ignores forwarded headers entirely.
//
// # Multi-hop chains
//
// X-Forwarded-For is a comma-separated chain appended left-to-right as a
// request traverses proxies:
//
//	X-Forwarded-For: client, proxy1, proxy2
//
// where the leftmost entry is the original client and the rightmost is
// the closest proxy (the direct peer). The resolver walks the chain
// left-to-right from the original client, skipping every IP that is in
// the trusted CIDR list, and returns the first untrusted IP. When all
// hops are trusted the original client is returned; when the chain
// contains an untrusted proxy the first such untrusted hop is returned.
// This is the correct behaviour for split-trust chains (issue #979):
// the prior right-to-left walk allowed an attacker-controlled proxy to
// masquerade as the first untrusted hop by placing their IP before the
// trusted proxy in the chain.
//
// # Zero dependencies
//
// Only the Go standard library is used (net.ParseCIDR, net.ParseIP),
// per the PRD's "zero-dependency" constraint.
package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseTrustedCIDRs splits a comma-separated list of CIDRs (as emitted
// by the NEXUS_TRUSTED_PROXIES env var) into parsed *net.IPNet values.
// Whitespace around each entry is trimmed; empty entries are skipped so
// a trailing comma is harmless. A bare IP (no /prefix) is accepted and
// treated as a /32 for IPv4 or /128 for IPv6 — convenient for
// single-host trust entries like "127.0.0.1".
//
// An empty or whitespace-only input yields a nil slice and no error,
// which is the "trust nobody" default.
//
// Any entry that is neither a valid CIDR nor a valid IP returns an
// error naming the offending entry. This makes misconfiguration fail
// loudly at boot (config.Load) rather than silently degrading into the
// "trust nobody" path.
func ParseTrustedCIDRs(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(p)
		if err == nil {
			out = append(out, ipnet)
			continue
		}
		// Not a CIDR — try a bare IP and promote it to a host route
		// (/32 or /128). This keeps the common "trust this one host"
		// case ergonomic without forcing operators to type /32.
		if ip := net.ParseIP(p); ip != nil {
			ipnet = singleIPNet(ip)
			out = append(out, ipnet)
			continue
		}
		return nil, fmt.Errorf("ratelimit: invalid trusted proxy entry %q (expected CIDR or IP)", p)
	}
	return out, nil
}

// singleIPNet returns a /32 (IPv4) or /128 (IPv6) *net.IPNet covering
// exactly the given IP. Used to promote bare-IP trust entries to host
// routes so Contains works uniformly.
func singleIPNet(ip net.IP) *net.IPNet {
	if ip.To4() != nil {
		return &net.IPNet{IP: ip.To4(), Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip.To16(), Mask: net.CIDRMask(128, 128)}
}

// ClientIPResolver maps an inbound *http.Request to the effective client
// IP, applying trusted-proxy enforcement to forwarded headers. The zero
// value is a valid resolver that trusts nobody: it always returns the
// direct peer IP and never inspects X-Forwarded-For / X-Real-IP.
//
// Construct via NewClientIPResolver so the trusted CIDRs are parsed
// once at boot; Resolve is then allocation-free on the hot path except
// for the XFF header parse (which only happens when the peer is
// trusted).
type ClientIPResolver struct {
	trusted []*net.IPNet
}

// NewClientIPResolver constructs a resolver. A nil or empty trusted list
// produces a "trust nobody" resolver that uses the direct peer IP for
// every request — the safe default mandated by issue #75.
func NewClientIPResolver(trusted []*net.IPNet) *ClientIPResolver {
	return &ClientIPResolver{trusted: trusted}
}

// Trusted reports whether any trusted-proxy CIDRs are configured. Used
// by the boot-time warning in cmd/nexus to detect the "rate limit on +
// non-loopback bind + no trusted proxies" misconfiguration.
func (r *ClientIPResolver) Trusted() bool {
	return r != nil && len(r.trusted) > 0
}

// SetTrustedProxies atomically replaces the trusted-proxy CIDR allowlist.
// Issue #896: this allows the SIGHUP handler to update the resolver without
// constructing a new ClientIPResolver, so in-flight requests see a consistent
// view of the old list until the swap is visible to the next Resolve call.
func (r *ClientIPResolver) SetTrustedProxies(trusted []*net.IPNet) {
	if r == nil {
		return
	}
	r.trusted = trusted
}

// Resolve returns the effective client IP for the request.
//
// Decision tree (in order):
//  1. Extract the direct peer IP from r.RemoteAddr (host:port). If that
//     fails, return the raw RemoteAddr so callers always get a non-empty
//     string.
//  2. If no trusted CIDRs are configured (trust nobody), return the
//     peer IP. Forwarded headers are ignored.
//  3. If the peer IP is not in any trusted CIDR, return the peer IP.
//     Forwarded headers from an untrusted peer are ignored (spoofing
//     defence).
//  4. The peer IS trusted. Walk X-Forwarded-For left-to-right, skipping
//     trusted IPs; return the first untrusted, valid IP (the original
//     client when all hops are trusted).
//  5. If XFF is absent, empty, or every hop is trusted, fall back to
//     X-Real-IP (a single trusted-proxy-injected value).
//  6. If neither header yields a usable IP, return the peer IP (the
//     trusted proxy itself — better than nothing, and matches what a
//     direct connection from the proxy would look like).
//
// The returned string is always non-empty.
func (r *ClientIPResolver) Resolve(req *http.Request) string {
	peer := peerIP(req.RemoteAddr)

	// Trust-nobody fast path (also the zero-value receiver case).
	if r == nil || len(r.trusted) == 0 {
		return peer
	}

	// Only honour forwarded headers when the direct peer is trusted.
	if !r.anyTrusted(peer) {
		return peer
	}

	// Walk the XFF chain left-to-right, skipping trusted hops.
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := r.leftmostUntrusted(xff); ip != "" {
			return ip
		}
	}

	// X-Real-IP is a single value injected by the first trusted proxy.
	// Only honour it when we already trust the peer (checked above).
	if xrip := strings.TrimSpace(req.Header.Get("X-Real-IP")); xrip != "" {
		if ip := net.ParseIP(xrip); ip != nil {
			return ip.String()
		}
	}

	// Every hop in the chain was trusted (or no headers were present).
	// The best identity we can offer is the trusted proxy itself.
	return peer
}

// anyTrusted reports whether ip falls within any configured trusted
// CIDR. An empty trusted list returns false. An unparseable ip returns
// false (defensive — the caller already validated it via peerIP).
func (r *ClientIPResolver) anyTrusted(ipStr string) bool {
	if r == nil {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// leftmostUntrusted walks a comma-separated X-Forwarded-For value
// left-to-right (from the original client toward the direct peer) and
// returns the first IP that is NOT in the trusted list. Trusted tokens
// are skipped; the walk stops at the first untrusted valid IP. Returns
// the original client when all hops are trusted; returns "" when no
// valid IP is present.
//
// This replaces the prior rightmostUntrusted behaviour (right-to-left
// walk). Rightmost walking allows an attacker who controls any proxy in
// the chain to make their proxy the first untrusted hop, spoofing any
// IP for rate-limiting purposes. Leftmost walking ensures the original
// client is returned when all intervening proxies are trusted, and the
// first untrusted hop is returned when the chain contains an untrusted
// proxy — which correctly attributes the client to the first hop that
// is not a trusted proxy (issue #979).
func (r *ClientIPResolver) leftmostUntrusted(xff string) string {
	hops := strings.Split(xff, ",")
	// Iterate from the leftmost (original client) toward the direct peer.
	// Walk forward: the leftmost entry is the original client; each
	// subsequent entry is an additional proxy hop.
	for i := range hops {
		tok := strings.TrimSpace(hops[i])
		if tok == "" {
			continue
		}
		ip := net.ParseIP(tok)
		if ip == nil {
			// Malformed token. A trusted proxy should never emit one,
			// but we keep walking past it rather than returning the
			// peer — the first untrusted valid IP after this token
			// is the best identity we can extract from the chain.
			continue
		}
		if !r.anyTrusted(ip.String()) {
			return ip.String()
		}
		// Trusted hop — keep walking right toward the direct peer.
	}
	return ""
}

// peerIP extracts the host portion of an addr that may be either
// "host:port" (the usual RemoteAddr shape) or a bare host. It returns
// the input unchanged when it cannot split a port, so callers always
// receive a non-empty string (assuming a non-empty input). IPv6
// addresses in bracketed form ("[::1]:1234") are handled by
// net.SplitHostPort.
func peerIP(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	// No port — assume the whole string is the host. This covers test
	// fixtures and any non-standard RemoteAddr shape.
	return remoteAddr
}
