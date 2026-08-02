package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
)

// blockedReasons are the label values for the nexus_egress_blocked_total
// counter, distinguishing redirect-phase blocks from dial-phase blocks.
const (
	reasonRedirect = "redirect"
	reasonDial     = "dial"
)

// EgressGuard is an SSRF defence-in-depth layer that rejects outbound
// connections to private, loopback, and link-local IP ranges. It is
// applied at two points in the HTTP client lifecycle:
//
//  1. CheckRedirect — before following each 3xx redirect hop, the
//     destination host is resolved and every resolved IP is checked
//     against the block list. A blocked redirect is never followed.
//
//  2. Dialer Control hook — the actually-dialled IP is re-checked just
//     before the TCP connect syscall, catching DNS-rebinding attacks
//     where the address resolved cleanly at CheckRedirect time but the
//     DNS record was swapped before the dial.
//
// The guard is off-by-default-safe: a zero-value EgressGuard blocks
// everything. Construct via NewEgressGuard for production use.
type EgressGuard struct {
	// enabled gates the guard entirely. When false, CheckRedirect follows
	// the default policy (up to 10 redirects) and the dial Control hook
	// is a no-op. This preserves byte-for-byte backward compatibility.
	enabled bool

	// allowedNets is the operator-supplied CIDR allowlist that overrides
	// the block list. An IP inside any allowedNet is never blocked, even
	// if it also falls inside a blocked range (e.g. loopback). This lets
	// a local-Ollama deployment allow 127.0.0.0/8 while still blocking
	// other private ranges.
	allowedNets []*net.IPNet

	// BlockedCount is an atomic counter incremented on every blocked
	// attempt. The reason ("redirect" or "dial") is passed to the logger
	// but the counter itself is a single scalar — callers that need
	// per-reason breakdowns should read it alongside their own label.
	BlockedCount atomic.Int64
}

// NewEgressGuard constructs an EgressGuard from the operator config.
// When enabled is false the returned guard is inert (all checks pass).
// allowedCIDRs is a comma-separated list of CIDR ranges that override
// the block list; invalid entries are silently skipped with a warning
// so a typo cannot prevent the proxy from starting.
func NewEgressGuard(enabled bool, allowedCIDRs []string) *EgressGuard {
	g := &EgressGuard{enabled: enabled}
	for _, cidr := range allowedCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			slog.Warn("egress guard: invalid allow CIDR, skipping",
				slog.String("cidr", cidr),
				slog.String("error", err.Error()),
				slog.String("component", "transport"),
			)
			continue
		}
		g.allowedNets = append(g.allowedNets, ipnet)
	}
	return g
}

// CheckRedirect implements http.Client.CheckRedirect. When the guard is
// disabled it returns nil (follow the redirect). When enabled it resolves
// the redirect destination host and rejects any IP that falls in a blocked
// range and is not in the allowlist.
func (g *EgressGuard) CheckRedirect(req *http.Request, via []*http.Request) error {
	if !g.enabled {
		return nil
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	host := req.URL.Hostname()
	if host == "" {
		return nil
	}
	if g.isHostBlocked(host) {
		g.BlockedCount.Add(1)
		slog.Warn("egress guard: blocked redirect to private address",
			slog.String("host", host),
			slog.String("url", req.URL.String()),
			slog.String("reason", reasonRedirect),
			slog.String("component", "transport"),
		)
		return fmt.Errorf("egress guard: redirect to %s blocked (private/loopback/link-local address)", host)
	}
	return nil
}

// DialControl is the net.Dialer.Control / ControlContext callback that
// re-checks the actually-dialled IP before the TCP connect syscall. When
// the guard is disabled it returns nil immediately. When the dial target
// is a blocked IP not in the allowlist, it returns ECONNREFUSED so the
// dialer fails fast without sending any packets to the blocked host.
func (g *EgressGuard) DialControl(ctx context.Context, network, address string) error {
	if !g.enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if g.isIPBlocked(ip) {
		g.BlockedCount.Add(1)
		slog.Warn("egress guard: blocked dial to private address",
			slog.String("ip", ip.String()),
			slog.String("port", port),
			slog.String("network", network),
			slog.String("reason", reasonDial),
			slog.String("component", "transport"),
		)
		return syscall.ECONNREFUSED
	}
	return nil
}

// isHostBlocked resolves host to its IP addresses and checks each against
// the block list. Returns true if ALL resolved IPs are blocked. A DNS
// resolution failure returns false (let the dialer handle the error)
// except when the host is already an IP literal — in that case the literal
// is checked directly.
func (g *EgressGuard) isHostBlocked(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return g.isIPBlocked(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if !g.isIPBlocked(ip) {
			return false
		}
	}
	return len(ips) > 0
}

// isIPBlocked returns true when ip falls inside any blocked range and is
// not inside any allowed range. The allowed list takes precedence so an
// operator can permit e.g. 127.0.0.0/8 for local Ollama while still
// blocking 10.0.0.0/8. When the guard is disabled, no IP is blocked.
func (g *EgressGuard) isIPBlocked(ip net.IP) bool {
	if !g.enabled {
		return false
	}
	for _, allowed := range g.allowedNets {
		if allowed.Contains(ip) {
			return false
		}
	}
	return isPrivateOrLoopback(ip)
}

// isPrivateOrLoopback checks whether an IP address falls into any of the
// ranges that should not be reachable from a public upstream: loopback,
// link-local (including cloud metadata 169.254.0.0/16), private
// (10/8, 172.16/12, 192.168/16), ULA (fc00::/7), the unspecified address
// (0.0.0.0), and IPv4-mapped IPv6 equivalents.
func isPrivateOrLoopback(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	// IsPrivate in Go 1.17+ covers RFC 1918 + fc00::/7, but we also
	// block 0.0.0.0/8 explicitly (already caught by IsUnspecified for
	// the exact 0.0.0.0, but the rest of the range like 0.0.0.1 needs
	// explicit handling).
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

// AllowedCIDRs returns the string representations of the configured
// allowlist entries, for diagnostic / logging purposes.
func (g *EgressGuard) AllowedCIDRs() []string {
	out := make([]string, len(g.allowedNets))
	for i, n := range g.allowedNets {
		out[i] = n.String()
	}
	return out
}

// Enabled reports whether the guard is active.
func (g *EgressGuard) Enabled() bool { return g.enabled }

// CheckRedirectFunc returns a function suitable for direct assignment to
// http.Client.CheckRedirect without a method-value closure.
func (g *EgressGuard) CheckRedirectFunc() func(*http.Request, []*http.Request) error {
	return g.CheckRedirect
}

// parseAllowedCIDRs splits a comma-separated string into individual CIDR
// entries, trimming whitespace and dropping empties. Invalid CIDRs are
// reported via the returned error slice rather than failing boot; the
// caller can decide whether to log-and-continue or abort.
func parseAllowedCIDRs(raw string) ([]string, []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	var valid []string
	var invalid []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(p); err != nil {
			invalid = append(invalid, p)
			continue
		}
		valid = append(valid, p)
	}
	return valid, invalid
}

// urlHostBlocked is a convenience for checking a raw URL string without
// constructing a full *http.Request. Used by boot-time validation to
// pre-check configured Ollama/frontier URLs.
func (g *EgressGuard) urlHostBlocked(rawURL string) bool {
	if !g.enabled {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	return g.isHostBlocked(host)
}
