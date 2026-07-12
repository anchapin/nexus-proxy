package ratelimit

import (
	"net"
	"net/http"
	"testing"
)

// helper: build a request with RemoteAddr + optional headers.
func mkReq(remoteAddr string, xff, xrip string) *http.Request {
	r := &http.Request{
		RemoteAddr: remoteAddr,
		Header:     http.Header{},
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if xrip != "" {
		r.Header.Set("X-Real-IP", xrip)
	}
	return r
}

func mustCIDRs(t *testing.T, raw string) []*net.IPNet {
	t.Helper()
	out, err := ParseTrustedCIDRs(raw)
	if err != nil {
		t.Fatalf("ParseTrustedCIDRs(%q): %v", raw, err)
	}
	return out
}

func TestParseTrustedCIDRs_Empty(t *testing.T) {
	out, err := ParseTrustedCIDRs("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil slice for empty input, got %v", out)
	}
}

func TestParseTrustedCIDRs_Whitespace(t *testing.T) {
	out, err := ParseTrustedCIDRs("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for whitespace-only, got %v", out)
	}
}

func TestParseTrustedCIDRs_TrimAndSkipEmpty(t *testing.T) {
	out, err := ParseTrustedCIDRs(" 10.0.0.0/8 , , 172.16.0.0/12,")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 CIDRs, got %d", len(out))
	}
}

func TestParseTrustedCIDRs_BareIP(t *testing.T) {
	out, err := ParseTrustedCIDRs("127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if ones, bits := out[0].Mask.Size(); ones != 32 || bits != 32 {
		t.Errorf("bare IPv4 should be /32, got /%d of %d", ones, bits)
	}
	if !out[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Error("/32 does not contain 127.0.0.1")
	}
}

func TestParseTrustedCIDRs_BareIPv6(t *testing.T) {
	out, err := ParseTrustedCIDRs("::1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if ones, bits := out[0].Mask.Size(); ones != 128 || bits != 128 {
		t.Errorf("bare IPv6 should be /128, got /%d of %d", ones, bits)
	}
}

func TestParseTrustedCIDRs_Invalid(t *testing.T) {
	cases := []string{
		"not-a-cidr",
		"10.0.0.0/8, bogus",
		"999.999.999.999",
		"10.0.0.0/99",
	}
	for _, c := range cases {
		if _, err := ParseTrustedCIDRs(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// Acceptance criterion: with no trusted proxies, changing XFF does not
// change the resolved IP.
func TestResolve_TrustNobody_IgnoresXFF(t *testing.T) {
	r := NewClientIPResolver(nil)
	got := r.Resolve(mkReq("203.0.113.5:4000", "10.0.0.1", "10.0.0.2"))
	if got != "203.0.113.5" {
		t.Errorf("expected direct peer, got %q", got)
	}
}

// Empty (but non-nil) trusted list also trusts nobody.
func TestResolve_EmptyTrusted_IgnoresXFF(t *testing.T) {
	r := NewClientIPResolver([]*net.IPNet{})
	got := r.Resolve(mkReq("203.0.113.5:4000", "10.0.0.1", ""))
	if got != "203.0.113.5" {
		t.Errorf("expected direct peer, got %q", got)
	}
}

// Untrusted peer: XFF must be ignored even when trusted CIDRs exist.
func TestResolve_UntrustedPeer_IgnoresXFF(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	// Peer 203.0.113.5 is NOT in 10.0.0.0/8, so XFF is spoofed noise.
	got := r.Resolve(mkReq("203.0.113.5:4000", "10.0.0.99", "10.0.0.50"))
	if got != "203.0.113.5" {
		t.Errorf("untrusted peer must use RemoteAddr, got %q", got)
	}
}

// Trusted peer, single-hop XFF: return the forwarded client.
func TestResolve_TrustedPeer_SingleHopXFF(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	got := r.Resolve(mkReq("10.0.0.1:4000", "203.0.113.5", ""))
	if got != "203.0.113.5" {
		t.Errorf("expected forwarded client, got %q", got)
	}
}

// Acceptance criterion: multi-hop chains resolve to the first untrusted
// client IP. XFF = "client, proxy1, proxy2"; peer = proxy2 (trusted).
func TestResolve_TrustedPeer_MultiHopChain(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	// peer 10.0.0.2 trusted; chain: client 203.0.113.5, proxy 10.0.0.1, proxy 10.0.0.2
	got := r.Resolve(mkReq("10.0.0.2:4000", "203.0.113.5, 10.0.0.1, 10.0.0.2", ""))
	if got != "203.0.113.5" {
		t.Errorf("expected leftmost untrusted client, got %q", got)
	}
}

// Multi-hop where every hop is ALSO a trusted proxy (internal proxying).
// The walk exhausts the chain and falls back to peer.
func TestResolve_TrustedPeer_AllHopsTrusted(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	got := r.Resolve(mkReq("10.0.0.2:4000", "10.0.0.5, 10.0.0.1, 10.0.0.2", ""))
	// Every hop is trusted; we return the peer (10.0.0.2) as the best
	// available identity — better than an empty string.
	if got != "10.0.0.2" {
		t.Errorf("expected peer fallback when all hops trusted, got %q", got)
	}
}

// Trusted peer, no XFF, X-Real-IP present: honour X-Real-IP.
func TestResolve_TrustedPeer_XRealIPFallback(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	got := r.Resolve(mkReq("10.0.0.1:4000", "", "198.51.100.7"))
	if got != "198.51.100.7" {
		t.Errorf("expected X-Real-IP, got %q", got)
	}
}

// Trusted peer, XFF empty and X-Real-IP malformed: fall back to peer.
func TestResolve_TrustedPeer_MalformedHeaders(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	got := r.Resolve(mkReq("10.0.0.1:4000", "not-an-ip", "also-not-an-ip"))
	if got != "10.0.0.1" {
		t.Errorf("expected peer fallback, got %q", got)
	}
}

// XFF precedence over X-Real-IP when both present and peer is trusted.
func TestResolve_XFFPrecedenceOverXRealIP(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	got := r.Resolve(mkReq("10.0.0.1:4000", "203.0.113.5", "198.51.100.7"))
	if got != "203.0.113.5" {
		t.Errorf("XFF should take precedence, got %q", got)
	}
}

// IPv6 peer + trusted ::1.
func TestResolve_IPv6(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "::1/128"))
	got := r.Resolve(mkReq("[::1]:4000", "2001:db8::1", ""))
	if got != "2001:db8::1" {
		t.Errorf("expected IPv6 forwarded client, got %q", got)
	}
}

// A malformed token in the middle of the chain stops the walk (no valid
// IP returned by rightmostUntrusted), so we fall back to the peer
// rather than skipping past the malformed entry (injection guard).
func TestResolve_MalformedMiddleToken_StopsWalk(t *testing.T) {
	r := NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8"))
	// peer 10.0.0.1 trusted; chain "client, bogus, 10.0.0.1".
	// Walking right-to-left: skip 10.0.0.1 (trusted), hit "bogus"
	// (invalid) -> rightmostUntrusted returns "" -> fall back to peer.
	got := r.Resolve(mkReq("10.0.0.1:4000", "203.0.113.5, bogus, 10.0.0.1", ""))
	if got != "10.0.0.1" {
		t.Errorf("expected peer fallback on malformed middle token, got %q", got)
	}
}

// RemoteAddr without a port should still resolve (defensive).
func TestResolve_RemoteAddrNoPort(t *testing.T) {
	r := NewClientIPResolver(nil)
	got := r.Resolve(mkReq("203.0.113.5", "", ""))
	if got != "203.0.113.5" {
		t.Errorf("expected host as-is, got %q", got)
	}
}

// Zero-value (nil) resolver behaves like trust-nobody.
func TestResolve_ZeroValue_TrustNobody(t *testing.T) {
	var r *ClientIPResolver // nil receiver
	got := r.Resolve(mkReq("203.0.113.5:4000", "10.0.0.1", "10.0.0.2"))
	if got != "203.0.113.5" {
		t.Errorf("nil resolver must return peer, got %q", got)
	}
}

func TestTrusted_Flag(t *testing.T) {
	if (&ClientIPResolver{}).Trusted() {
		t.Error("empty resolver should report Trusted()=false")
	}
	if NewClientIPResolver(nil).Trusted() {
		t.Error("nil-list resolver should report Trusted()=false")
	}
	if !NewClientIPResolver(mustCIDRs(t, "10.0.0.0/8")).Trusted() {
		t.Error("configured resolver should report Trusted()=true")
	}
}
