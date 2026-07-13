package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// getContextGet issues a context-aware GET via client so the tests
// satisfy noctx (http.Get / (*http.Client).Get do not propagate a
// context). The response body is left open for the caller to close.
func getContextGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return resp
}

// TestSecurityHeadersStampedOnResponse asserts the three always-on
// security headers are present on a wrapped response (issue #39).
func TestSecurityHeadersStampedOnResponse(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(SecurityHeaders(false)(inner))
	defer srv.Close()

	resp := getContextGet(t, http.DefaultClient, srv.URL)
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// TestSecurityHeadersHSTSOnlyWithTLS asserts Strict-Transport-Security
// is emitted when tlsActive is true and absent when false (issue #39).
// Emitting HSTS over plaintext would be ignored by browsers and is a
// spec violation, so the negative case is just as important.
func TestSecurityHeadersHSTSOnlyWithTLS(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// No TLS → no HSTS.
	srv := httptest.NewServer(SecurityHeaders(false)(inner))
	defer srv.Close()
	resp := getContextGet(t, http.DefaultClient, srv.URL)
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS present without TLS: %q", got)
	}
	resp.Body.Close()

	// TLS active → HSTS with one-year max-age.
	srv2 := httptest.NewTLSServer(SecurityHeaders(true)(inner))
	defer srv2.Close()
	client := srv2.Client()
	resp2 := getContextGet(t, client, srv2.URL)
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("HSTS = %q, want max-age=31536000", got)
	}
}

// TestSecurityHeadersOnAllPaths asserts the headers apply to /healthz
// too, since the middleware wraps the whole mux (issue #39).
func TestSecurityHeadersOnAllPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	srv := httptest.NewServer(SecurityHeaders(false)(mux))
	defer srv.Close()

	resp := getContextGet(t, http.DefaultClient, srv.URL+"/healthz")
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options on /healthz = %q, want nosniff", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options on /healthz = %q, want DENY", got)
	}
}

// TestTLSServerWithValidCert spins up an HTTPS server with a
// self-signed ECDSA P-256 certificate generated with the stdlib crypto
// packages, wraps the handler with SecurityHeaders(true), and verifies
// a client can connect over TLS and receive the HSTS header (issue #39).
//
// Stdlib-only by design (crypto/ecdsa, crypto/elliptic, crypto/rand,
// crypto/tls, crypto/x509) — no external cert tooling required.
func TestTLSServerWithValidCert(t *testing.T) {
	cert, err := selfSignedCert()
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	// Wrap with the TLS-active security headers so HSTS is emitted.
	h := SecurityHeaders(true)(inner)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// Client configured to skip verification — the cert is self-signed
	// and not in any trust store. This proves the TLS handshake + the
	// HSTS header round-trip, not certificate validation.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp := getContextGet(t, client, "https://"+ln.Addr().String())
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("HSTS = %q, want max-age=31536000", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if resp.TLS == nil {
		t.Error("resp.TLS is nil; expected a negotiated TLS session")
	}
}

// selfSignedCert generates an ECDSA P-256 self-signed certificate valid
// for 127.0.0.1 and localhost, suitable for in-process TLS tests. Uses
// only stdlib crypto packages (issue #39 constraint: stdlib only).
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "nexus-proxy-test",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
