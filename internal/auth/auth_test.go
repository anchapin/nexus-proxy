package auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/ratelimit"
)

// okHandler is a simple 200-OK handler used across tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func mustCIDRs(t *testing.T, raw string) []*net.IPNet {
	t.Helper()
	out, err := ratelimit.ParseTrustedCIDRs(raw)
	if err != nil {
		t.Fatalf("ParseTrustedCIDRs(%q): %v", raw, err)
	}
	return out
}

func TestDisabledWhenNoKey(t *testing.T) {
	m := NewMiddleware("", nil, nil, nil)
	if m.Enabled() {
		t.Error("Enabled() = true for empty key, want false")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("disabled middleware: status = %d, want 200", rr.Code)
	}
}

func TestRejectsWithoutToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil) // no exempt paths

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("WWW-Authenticate header not set on 401")
	}
}

func TestAuthMissingToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	if rr.Body.String() != `{"error":"missing or malformed Authorization header"}` {
		t.Errorf("body = %q, want %q", rr.Body.String(), `{"error":"missing or malformed Authorization header"}`)
	}
}

func TestRejectsWrongToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rr.Code)
	}
}

func TestAuthInvalidToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	if rr.Body.String() != `{"error":"invalid API key"}` {
		t.Errorf("body = %q, want %q", rr.Body.String(), `{"error":"invalid API key"}`)
	}
}

func TestAcceptsCorrectToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body = %q, want 'ok'", rr.Body.String())
	}
}

func TestExemptPathsBypassAuth(t *testing.T) {
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics"
	}
	m := NewMiddleware("secret-key", exempt, nil, nil)

	for _, path := range []string{"/healthz", "/metrics"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		m.Wrap(okHandler()).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("exempt path %s: status = %d, want 200", path, rr.Code)
		}
	}

	// Non-exempt path still requires auth.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-exempt path: status = %d, want 401", rr.Code)
	}
}

func TestStatusGatedByDefault(t *testing.T) {
	// /status is NOT in the exempt set — it must require auth.
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics"
	}
	m := NewMiddleware("secret-key", exempt, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("/status without token: status = %d, want 401", rr.Code)
	}
}

func TestStatusPublicWhenExempt(t *testing.T) {
	// Operator sets NEXUS_STATUS_PUBLIC=true → /status is exempt.
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics" || r.URL.Path == "/status"
	}
	m := NewMiddleware("secret-key", exempt, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("/status when public: status = %d, want 200", rr.Code)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"empty", "", ""},
		{"no scheme", "secret-key", ""},
		{"basic scheme", "Basic secret-key", ""},
		{"bearer lowercase", "bearer secret-key", "secret-key"},
		{"bearer uppercase", "BEARER secret-key", "secret-key"},
		{"bearer mixed case", "Bearer secret-key", "secret-key"},
		{"bearer no token", "Bearer ", ""},
		{"bearer extra spaces", "Bearer   multi-word-key", "multi-word-key"},
		{"bearer only", "Bearer", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got := BearerToken(req)
			if got != tc.want {
				t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestConstantTimeComparisonRegression is a regression test for issue #228.
// It verifies that keys of different lengths are compared using
// crypto/subtle.ConstantTimeCompare, which handles length differences
// without leaking information through timing. The test covers both
// shorter and longer wrong keys to ensure no early-exit shortcut
// bypasses the constant-time check.
func TestConstantTimeComparisonRegression(t *testing.T) {
	correctKey := "correct-secret-key-32byteslong!!"

	testCases := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"exact match", correctKey, http.StatusOK},
		{"empty token", "", http.StatusUnauthorized},
		{"wrong key same length", "wrong-secret-key-32byteslong!!", http.StatusUnauthorized},
		{"wrong key shorter", "wrong-short", http.StatusUnauthorized},
		{"wrong key longer", "wrong-secret-key-32byteslong!!EXTRA", http.StatusUnauthorized},
		{"single char diff", "correct-secret-key-32byteslong!X", http.StatusUnauthorized},
		{"first char wrong", "Worrect-secret-key-32byteslong!!", http.StatusUnauthorized},
		{"last char wrong", "correct-secret-key-32byteslong!W", http.StatusUnauthorized},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMiddleware(correctKey, nil, nil, nil)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			m.Wrap(okHandler()).ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("token %q: status = %d, want %d", tc.token, rr.Code, tc.wantStatus)
			}
		})
	}
}

// mockObserver implements AuthObserver for testing (issue #295).
type mockObserver struct {
	rejectedInvalid int
	rejectedMissing int
}

func (m *mockObserver) IncAuthRejectedInvalid() { m.rejectedInvalid++ }
func (m *mockObserver) IncAuthRejectedMissing() { m.rejectedMissing++ }

// TestAuthObserverMissingToken verifies that IncAuthRejectedMissing is called
// when a request arrives without a token (issue #295).
func TestAuthObserverMissingToken(t *testing.T) {
	obs := &mockObserver{}
	m := NewMiddleware("secret-key", nil, nil, obs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if obs.rejectedMissing != 1 {
		t.Errorf("IncAuthRejectedMissing call count = %d, want 1", obs.rejectedMissing)
	}
	if obs.rejectedInvalid != 0 {
		t.Errorf("IncAuthRejectedInvalid call count = %d, want 0", obs.rejectedInvalid)
	}
}

// TestAuthObserverInvalidToken verifies that IncAuthRejectedInvalid is called
// when a request arrives with an invalid token (issue #295).
func TestAuthObserverInvalidToken(t *testing.T) {
	obs := &mockObserver{}
	m := NewMiddleware("secret-key", nil, nil, obs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if obs.rejectedInvalid != 1 {
		t.Errorf("IncAuthRejectedInvalid call count = %d, want 1", obs.rejectedInvalid)
	}
	if obs.rejectedMissing != 0 {
		t.Errorf("IncAuthRejectedMissing call count = %d, want 0", obs.rejectedMissing)
	}
}

// TestAuthObserverNoCallbackOnSuccess verifies that neither observer method is
// called when auth succeeds (issue #295).
func TestAuthObserverNoCallbackOnSuccess(t *testing.T) {
	obs := &mockObserver{}
	m := NewMiddleware("secret-key", nil, nil, obs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if obs.rejectedInvalid != 0 {
		t.Errorf("IncAuthRejectedInvalid call count = %d, want 0", obs.rejectedInvalid)
	}
	if obs.rejectedMissing != 0 {
		t.Errorf("IncAuthRejectedMissing call count = %d, want 0", obs.rejectedMissing)
	}
}

// TestAuthLimiterBlockedIP gets a 429 when the limiter marks the IP blocked.
// The exempt check happens AFTER the block check, so blocked IPs always get 429.
func TestAuthLimiterBlockedIP(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	resolver := ratelimit.NewClientIPResolver(trusted)
	al := ratelimit.NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	m := NewMiddleware("secret-key", nil, al, nil)

	blockedIP := "203.0.113.50"
	for i := 0; i < 3; i++ {
		al.RecordFailure(blockedIP)
	}
	if !al.IsBlocked(blockedIP) {
		t.Fatal("IP should be blocked after 3 failures")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Real-IP", blockedIP)
	req.RemoteAddr = "10.0.0.5:12345"
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("blocked IP: status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header not set on 429")
	}
	if rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rr.Header().Get("Content-Type"))
	}
}

// TestAuthLimiterFailureIncrementsMap verifies that auth failures are recorded
// in the limiter's failure map.
func TestAuthLimiterFailureIncrementsMap(t *testing.T) {
	resolver := ratelimit.NewClientIPResolver(nil)
	al := ratelimit.NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	m := NewMiddleware("secret-key", nil, al, nil)

	clientIP := "198.51.100.20"
	if al.BucketCount() != 0 {
		t.Fatalf("initial bucket count = %d, want 0", al.BucketCount())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Real-IP", clientIP)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rr.Code)
	}
	if al.BucketCount() != 1 {
		t.Errorf("after 1 failure: bucket count = %d, want 1", al.BucketCount())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("Authorization", "Bearer wrong-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rr.Code)
	}
	if al.BucketCount() != 1 {
		t.Errorf("after 2 failures same IP: bucket count = %d, want 1", al.BucketCount())
	}
}

// TestAuthLimiterNoOpWhenDisabled verifies that a disabled limiter does not
// affect auth behavior (nil or rpm<=0 limiter is a no-op).
func TestAuthLimiterNoOpWhenDisabled(t *testing.T) {
	al := ratelimit.NewAuthLimiter(0, 3, 5*time.Minute, nil)
	m := NewMiddleware("secret-key", nil, al, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("disabled limiter: status = %d, want 401", rr.Code)
	}
	if al.BucketCount() != 0 {
		t.Errorf("disabled limiter: bucket count = %d, want 0", al.BucketCount())
	}
}

// TestAuthLimiterExemptPathBypassesLimiter verifies that exempt paths skip
// both the auth check and the limiter check.
func TestAuthLimiterExemptPathBypassesLimiter(t *testing.T) {
	resolver := ratelimit.NewClientIPResolver(nil)
	al := ratelimit.NewAuthLimiter(60, 1, 5*time.Minute, resolver)
	exempt := func(r *http.Request) bool { return r.URL.Path == "/healthz" }
	m := NewMiddleware("secret-key", exempt, al, nil)

	clientIP := "192.0.2.10"
	al.RecordFailure(clientIP)
	if !al.IsBlocked(clientIP) {
		t.Fatal("IP should be blocked after 1 failure with burst=1")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Real-IP", clientIP)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("exempt path: status = %d, want 200", rr.Code)
	}
}

// TestAuthLimiterCorrectTokenNoRecord verifies that successful auth does not
// record a failure.
func TestAuthLimiterCorrectTokenNoRecord(t *testing.T) {
	resolver := ratelimit.NewClientIPResolver(nil)
	al := ratelimit.NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	m := NewMiddleware("secret-key", nil, al, nil)

	clientIP := "203.0.113.99"
	if al.BucketCount() != 0 {
		t.Fatalf("initial bucket count = %d, want 0", al.BucketCount())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("Authorization", "Bearer secret-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200", rr.Code)
	}
	if al.BucketCount() != 0 {
		t.Errorf("after successful auth: bucket count = %d, want 0", al.BucketCount())
	}
}
