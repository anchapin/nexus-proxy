package transport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsPrivateOrLoopback(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// Loopback
		{"127.0.0.1", true},
		{"127.0.0.0", true},
		{"127.255.255.255", true},
		{"::1", true},
		// Link-local (includes cloud metadata)
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},
		// Private (RFC 1918)
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.15.0.1", false}, // not in 172.16/12
		{"172.32.0.1", false}, // not in 172.16/12
		{"192.168.1.1", true},
		{"192.168.0.0", true},
		// ULA
		{"fc00::1", true},
		{"fd00::1", true},
		// 0.0.0.0/8
		{"0.0.0.0", true},
		{"0.0.0.1", true},
		// Public addresses
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"172.217.0.0", false},
		{"203.0.113.1", false},
		{"2606:4700:4700::1111", false},
		// nil
		{"<nil>", true},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			var ip net.IP
			if tc.ip != "<nil>" {
				ip = net.ParseIP(tc.ip)
				if ip == nil {
					t.Fatalf("failed to parse %q", tc.ip)
				}
			}
			got := isPrivateOrLoopback(ip)
			if got != tc.want {
				t.Errorf("isPrivateOrLoopback(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestEgressGuardIsIPBlocked(t *testing.T) {
	g := NewEgressGuard(true, nil)

	blocked := []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.168.1.1", "::1", "fc00::1"}
	for _, ip := range blocked {
		if !g.isIPBlocked(net.ParseIP(ip)) {
			t.Errorf("expected %s to be blocked", ip)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "203.0.113.42"}
	for _, ip := range allowed {
		if g.isIPBlocked(net.ParseIP(ip)) {
			t.Errorf("expected %s to NOT be blocked", ip)
		}
	}
}

func TestEgressGuardAllowlistOverride(t *testing.T) {
	g := NewEgressGuard(true, []string{"10.0.0.0/8", "127.0.0.0/8"})

	// Private IPs that are in the allowlist should pass.
	if g.isIPBlocked(net.ParseIP("10.0.0.5")) {
		t.Error("expected 10.0.0.5 to be allowed (in allowlist)")
	}
	if g.isIPBlocked(net.ParseIP("127.0.0.1")) {
		t.Error("expected 127.0.0.1 to be allowed (in allowlist)")
	}

	// Private IPs NOT in the allowlist should still be blocked.
	if !g.isIPBlocked(net.ParseIP("192.168.1.1")) {
		t.Error("expected 192.168.1.1 to be blocked (not in allowlist)")
	}
	if !g.isIPBlocked(net.ParseIP("169.254.169.254")) {
		t.Error("expected 169.254.169.254 to be blocked (not in allowlist)")
	}

	// Public IPs should always pass.
	if g.isIPBlocked(net.ParseIP("1.1.1.1")) {
		t.Error("expected 1.1.1.1 to be allowed")
	}
}

func TestEgressGuardDisabled(t *testing.T) {
	g := NewEgressGuard(false, nil)

	// When disabled, no IP should be blocked.
	blocked := []string{"127.0.0.1", "10.0.0.1", "169.254.169.254"}
	for _, ip := range blocked {
		if g.isIPBlocked(net.ParseIP(ip)) {
			t.Errorf("expected %s to NOT be blocked (guard disabled)", ip)
		}
	}
}

func TestEgressGuardCheckRedirectDisabled(t *testing.T) {
	g := NewEgressGuard(false, nil)
	target := mustParseReq(t, "GET", "http://169.254.169.254/latest/meta-data/")
	err := g.CheckRedirect(target, []*http.Request{mustParseReq(t, "GET", "http://example.com/")})
	if err != nil {
		t.Errorf("expected nil error when guard disabled, got %v", err)
	}
}

func TestEgressGuardCheckRedirectBlockedMetadata(t *testing.T) {
	g := NewEgressGuard(true, nil)
	target := mustParseReq(t, "GET", "http://169.254.169.254/latest/meta-data/")
	err := g.CheckRedirect(target, []*http.Request{mustParseReq(t, "GET", "http://example.com/")})
	if err == nil {
		t.Fatal("expected redirect to metadata IP to be blocked")
	}
	if g.BlockedCount.Load() != 1 {
		t.Errorf("expected BlockedCount=1, got %d", g.BlockedCount.Load())
	}
}

func TestEgressGuardCheckRedirectBlockedPrivateIP(t *testing.T) {
	g := NewEgressGuard(true, nil)
	target := mustParseReq(t, "GET", "http://10.0.0.1:8080/admin")
	err := g.CheckRedirect(target, []*http.Request{})
	if err == nil {
		t.Fatal("expected redirect to 10.0.0.1 to be blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("expected error to mention 'blocked', got: %v", err)
	}
}

func TestEgressGuardCheckRedirectBlockedLoopback(t *testing.T) {
	g := NewEgressGuard(true, nil)
	target := mustParseReq(t, "GET", "http://localhost:9090/")
	err := g.CheckRedirect(target, []*http.Request{})
	if err == nil {
		t.Fatal("expected redirect to localhost to be blocked")
	}
}

func TestEgressGuardCheckRedirectAllowedPrivate(t *testing.T) {
	g := NewEgressGuard(true, []string{"10.0.0.0/8"})
	target := mustParseReq(t, "GET", "http://10.0.0.5:8080/")
	err := g.CheckRedirect(target, []*http.Request{})
	if err != nil {
		t.Errorf("expected redirect to allowlisted 10.0.0.5 to pass, got %v", err)
	}
	if g.BlockedCount.Load() != 0 {
		t.Errorf("expected BlockedCount=0, got %d", g.BlockedCount.Load())
	}
}

func TestEgressGuardCheckRedirectPublicPasses(t *testing.T) {
	g := NewEgressGuard(true, nil)
	target := mustParseReq(t, "GET", "http://1.2.3.4/path")
	err := g.CheckRedirect(target, []*http.Request{})
	if err != nil {
		t.Errorf("expected redirect to public IP to pass, got %v", err)
	}
}

func TestEgressGuardCheckRedirectMaxRedirects(t *testing.T) {
	g := NewEgressGuard(true, nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = mustParseReq(t, "GET", fmt.Sprintf("http://example.com/%d", i))
	}
	target := mustParseReq(t, "GET", "http://example.com/final")
	err := g.CheckRedirect(target, via)
	if err == nil {
		t.Fatal("expected error after 10 redirects")
	}
}

func TestEgressGuardDialControlDisabled(t *testing.T) {
	g := NewEgressGuard(false, nil)
	err := g.DialControl(context.Background(), "tcp4", "127.0.0.1:11434")
	if err != nil {
		t.Errorf("expected nil error when guard disabled, got %v", err)
	}
}

func TestEgressGuardDialControlBlockedPrivate(t *testing.T) {
	g := NewEgressGuard(true, nil)
	err := g.DialControl(context.Background(), "tcp4", "10.0.0.1:11434")
	if err == nil {
		t.Fatal("expected dial to 10.0.0.1 to be blocked")
	}
	if g.BlockedCount.Load() != 1 {
		t.Errorf("expected BlockedCount=1, got %d", g.BlockedCount.Load())
	}
}

func TestEgressGuardDialControlBlockedMetadata(t *testing.T) {
	g := NewEgressGuard(true, nil)
	err := g.DialControl(context.Background(), "tcp4", "169.254.169.254:80")
	if err == nil {
		t.Fatal("expected dial to metadata endpoint to be blocked")
	}
}

func TestEgressGuardDialControlAllowedPrivate(t *testing.T) {
	g := NewEgressGuard(true, []string{"127.0.0.0/8"})
	err := g.DialControl(context.Background(), "tcp4", "127.0.0.1:11434")
	if err != nil {
		t.Errorf("expected dial to allowlisted loopback to pass, got %v", err)
	}
}

func TestEgressGuardDialControlPublicPasses(t *testing.T) {
	g := NewEgressGuard(true, nil)
	err := g.DialControl(context.Background(), "tcp4", "1.2.3.4:443")
	if err != nil {
		t.Errorf("expected dial to public IP to pass, got %v", err)
	}
}

func TestParseAllowedCIDRs(t *testing.T) {
	valid, invalid := parseAllowedCIDRs("10.0.0.0/8, 172.16.0.0/12,not-a-cidr, , 192.168.0.0/16")
	if len(invalid) != 1 || invalid[0] != "not-a-cidr" {
		t.Errorf("expected 1 invalid entry 'not-a-cidr', got %v", invalid)
	}
	if len(valid) != 3 {
		t.Fatalf("expected 3 valid entries, got %d: %v", len(valid), valid)
	}
	want := map[string]bool{
		"10.0.0.0/8":     true,
		"172.16.0.0/12":  true,
		"192.168.0.0/16": true,
	}
	for _, v := range valid {
		if !want[v] {
			t.Errorf("unexpected valid entry: %s", v)
		}
	}
}

func TestParseAllowedCIDRsEmpty(t *testing.T) {
	valid, invalid := parseAllowedCIDRs("")
	if valid != nil || invalid != nil {
		t.Errorf("expected nil slices for empty input, got valid=%v invalid=%v", valid, invalid)
	}
}

func TestParseAllowedCIDRsOnlySpaces(t *testing.T) {
	valid, _ := parseAllowedCIDRs("  ,  ,  ")
	if len(valid) != 0 {
		t.Errorf("expected 0 valid entries for spaces-only, got %d", len(valid))
	}
}

func TestEgressGuardURLHostBlocked(t *testing.T) {
	g := NewEgressGuard(true, []string{"127.0.0.0/8"})

	if !g.urlHostBlocked("http://10.0.0.1:8080/") {
		t.Error("expected 10.0.0.1 URL to be blocked")
	}
	if g.urlHostBlocked("http://127.0.0.1:11434/") {
		t.Error("expected allowlisted 127.0.0.1 URL to pass")
	}
	if g.urlHostBlocked("http://1.2.3.4/") {
		t.Error("expected public URL to pass")
	}
}

func TestEgressGuardAllowedCIDRs(t *testing.T) {
	g := NewEgressGuard(true, []string{"10.0.0.0/8", "172.16.0.0/12"})
	cidrs := g.AllowedCIDRs()
	if len(cidrs) != 2 {
		t.Fatalf("expected 2 CIDRs, got %d", len(cidrs))
	}
	if cidrs[0] != "10.0.0.0/8" || cidrs[1] != "172.16.0.0/12" {
		t.Errorf("unexpected CIDRs: %v", cidrs)
	}
}

func TestEgressGuardInvalidAllowCIDRSkipped(t *testing.T) {
	g := NewEgressGuard(true, []string{"10.0.0.0/8", "garbage", "192.168.0.0/16"})
	// Invalid entry should be silently skipped.
	if len(g.allowedNets) != 2 {
		t.Errorf("expected 2 valid nets (garbage skipped), got %d", len(g.allowedNets))
	}
}

// --- Integration tests with a real httptest.Server ---

func TestEgressGuardIntegrationRedirectBlocked(t *testing.T) {
	// Start a test server that redirects to a metadata endpoint.
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirectSrv.Close()

	g := NewEgressGuard(true, nil)
	client := &http.Client{
		CheckRedirect: g.CheckRedirectFunc(),
	}

	resp, err := client.Get(redirectSrv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error from blocked redirect, got nil")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("expected 'blocked' in error, got: %v", err)
	}
	if g.BlockedCount.Load() != 1 {
		t.Errorf("expected BlockedCount=1, got %d", g.BlockedCount.Load())
	}
}

func TestEgressGuardIntegrationRedirectAllowed(t *testing.T) {
	// Start a target server.
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer targetSrv.Close()

	// Start a redirect server pointing to the target.
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL, http.StatusFound)
	}))
	defer redirectSrv.Close()

	// httptest servers bind to loopback; allow 127.0.0.0/8 so the
	// guard doesn't block the test's own redirect chain.
	g := NewEgressGuard(true, []string{"127.0.0.0/8"})
	client := &http.Client{
		CheckRedirect: g.CheckRedirectFunc(),
	}

	resp, err := client.Get(redirectSrv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if g.BlockedCount.Load() != 0 {
		t.Errorf("expected BlockedCount=0 for allowed redirect, got %d", g.BlockedCount.Load())
	}
}

func TestEgressGuardGauges(t *testing.T) {
	g := NewEgressGuard(true, nil)

	samples := g.Gauges()
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}

	redirectSeen, dialSeen := false, false
	for _, s := range samples {
		if s.Name != "nexus_egress_blocked_total" {
			t.Errorf("expected name nexus_egress_blocked_total, got %s", s.Name)
		}
		reason, ok := s.Labels["reason"]
		if !ok {
			t.Error("expected labels to have 'reason' key")
		}
		switch reason {
		case "redirect":
			redirectSeen = true
			if s.Value != 0 {
				t.Errorf("expected redirect count=0 initially, got %f", s.Value)
			}
		case "dial":
			dialSeen = true
			if s.Value != 0 {
				t.Errorf("expected dial count=0 initially, got %f", s.Value)
			}
		default:
			t.Errorf("unexpected reason label value: %s", reason)
		}
	}
	if !redirectSeen {
		t.Error("expected redirect sample")
	}
	if !dialSeen {
		t.Error("expected dial sample")
	}
}

func TestEgressGuardGaugesAfterBlocks(t *testing.T) {
	g := NewEgressGuard(true, nil)

	g.DialControl(context.Background(), "tcp4", "10.0.0.1:11434")
	g.DialControl(context.Background(), "tcp4", "192.168.1.1:8080")
	g.CheckRedirect(mustParseReq(t, "GET", "http://169.254.169.254/"), []*http.Request{})

	samples := g.Gauges()
	redirectCount, dialCount := 0.0, 0.0
	for _, s := range samples {
		if s.Name != "nexus_egress_blocked_total" {
			continue
		}
		reason, ok := s.Labels["reason"]
		if !ok {
			continue
		}
		switch reason {
		case "redirect":
			redirectCount = s.Value
		case "dial":
			dialCount = s.Value
		}
	}
	if redirectCount != 1 {
		t.Errorf("expected redirect count=1, got %f", redirectCount)
	}
	if dialCount != 2 {
		t.Errorf("expected dial count=2, got %f", dialCount)
	}
}

func TestEgressGuardGaugesNil(t *testing.T) {
	var g *EgressGuard
	samples := g.Gauges()
	if samples != nil {
		t.Errorf("expected nil gauges for nil guard, got %v", samples)
	}
}

// mustParseReq is a test helper that creates an *http.Request for the
// given method and URL, panicking on bad URLs.
func mustParseReq(t *testing.T, method, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, raw, nil)
	if err != nil {
		t.Fatalf("failed to create request %q: %v", raw, err)
	}
	return req
}
