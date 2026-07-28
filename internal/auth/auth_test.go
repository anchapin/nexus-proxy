package auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/ratelimit"
	"github.com/anchapin/nexus-proxy/internal/tracingtest"
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
	accepted        int
	rejectedInvalid int
	rejectedMissing int
}

func (m *mockObserver) IncAuthAccepted()        { m.accepted++ }
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

// TestAuthObserverAccepted verifies that IncAuthAccepted is called
// when auth succeeds (issue #847).
func TestAuthObserverAccepted(t *testing.T) {
	obs := &mockObserver{}
	m := NewMiddleware("secret-key", nil, nil, obs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if obs.accepted != 1 {
		t.Errorf("IncAuthAccepted call count = %d, want 1", obs.accepted)
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
		al.RecordFailure(blockedIP, "missing")
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
	al.RecordFailure(clientIP, "invalid")
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

// TestAuthLimiterOnBlockCallback verifies that the SetOnBlock callback is
// invoked when the burst threshold is crossed (issue #831/#937). The callback
// fires each time the failure count reaches the burst threshold.
func TestAuthLimiterOnBlockCallback(t *testing.T) {
	resolver := ratelimit.NewClientIPResolver(nil)

	// burst=3: onBlock fires on the 3rd RecordFailure
	al := ratelimit.NewAuthLimiter(60, 3, 5*time.Minute, resolver)
	calls := 0
	al.SetOnBlock(func(reason string) { calls++ })

	clientIP := "198.51.100.5"
	al.RecordFailure(clientIP, "missing")
	if calls != 0 {
		t.Errorf("onBlock callback calls after 1 failure (burst=3) = %d, want 0", calls)
	}
	al.RecordFailure(clientIP, "missing")
	if calls != 0 {
		t.Errorf("onBlock callback calls after 2 failures (burst=3) = %d, want 0", calls)
	}
	al.RecordFailure(clientIP, "missing")
	if calls != 1 {
		t.Errorf("onBlock callback calls after 3rd failure (burst=3) = %d, want 1", calls)
	}

	// burst=2: onBlock fires on 2nd RecordFailure
	al2 := ratelimit.NewAuthLimiter(60, 2, 5*time.Minute, resolver)
	calls2 := 0
	al2.SetOnBlock(func(reason string) { calls2++ })
	al2.RecordFailure(clientIP, "invalid")
	if calls2 != 0 {
		t.Errorf("onBlock callback calls before threshold = %d, want 0", calls2)
	}
	al2.RecordFailure(clientIP, "invalid")
	if calls2 != 1 {
		t.Errorf("onBlock callback calls after reaching burst=2 = %d, want 1", calls2)
	}
	// Each subsequent failure within the window also fires onBlock since
	// the same reason count remains >= burst (2), so callers that want "once per
	// blocked transition" must de-duplicate at their level.
	al2.RecordFailure(clientIP, "invalid")
	if calls2 != 2 {
		t.Errorf("onBlock callback calls after 3rd failure (burst=2) = %d, want 2", calls2)
	}
}

// TestAuthLimiterOnBlockCallbackFiresEachThresholdCrossing verifies that
// onBlock fires each time the failure count re-enters the blocked state
// (issue #831/#937). This is the underlying mechanism that increments the
// nexus_auth_limiter_blocked_total counter.
func TestAuthLimiterOnBlockCallbackFiresEachThresholdCrossing(t *testing.T) {
	resolver := ratelimit.NewClientIPResolver(nil)
	al := ratelimit.NewAuthLimiter(60, 2, 5*time.Minute, resolver)
	calls := 0
	al.SetOnBlock(func(reason string) { calls++ })

	ip := "203.0.2.1"
	al.RecordFailure(ip, "missing")
	al.RecordFailure(ip, "missing") // burst reached → onBlock fires (calls=1)

	// Subsequent failures within window also trigger onBlock since
	// the same reason count >= burst is still true.
	for i := 0; i < 4; i++ {
		al.RecordFailure(ip, "missing")
	}
	// calls = 1 (2nd failure) + 4 (subsequent) = 5
	if calls != 5 {
		t.Errorf("onBlock calls after 6 total failures = %d, want 5", calls)
	}
}

// TestAuthSpanAttributesOnAccept verifies that when auth succeeds, it emits
// an "auth.check" span with auth.exempt=false, auth.token_present=true,
// and auth.outcome="accept" (issue #936).
func TestAuthSpanAttributesOnAccept(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	// Set up a tracing collector to capture spans.
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := m.Wrap(okHandler())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request should succeed, got %d", rec.Code)
	}

	// Close the exporter to drain the queue before checking spans.
	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	// Verify the span was captured with the correct attributes.
	span := coll.FindSpan(t, "auth.check")
	if span == nil {
		t.Fatal("no auth.check span found in captured spans")
	}
	if exempt := tracingtest.AttrBool(span, "auth.exempt"); exempt {
		t.Errorf("auth.exempt = true, want false")
	}
	if tokenPresent := tracingtest.AttrBool(span, "auth.token_present"); !tokenPresent {
		t.Errorf("auth.token_present = false, want true")
	}
	if outcome := tracingtest.AttrString(span, "auth.outcome"); outcome != "accept" {
		t.Errorf("auth.outcome = %q, want %q", outcome, "accept")
	}
}

// TestAuthSpanAttributesOnRejectMissing verifies that when auth fails due
// to missing token, it emits an "auth.check" span with auth.outcome="reject"
// (issue #936).
func TestAuthSpanAttributesOnRejectMissing(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	// Set up a tracing collector to capture spans.
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := m.Wrap(okHandler())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("request should be rejected, got %d", rec.Code)
	}

	// Close the exporter to drain the queue before checking spans.
	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	// Verify the span was captured with the correct attributes.
	span := coll.FindSpan(t, "auth.check")
	if span == nil {
		t.Fatal("no auth.check span found in captured spans")
	}
	if outcome := tracingtest.AttrString(span, "auth.outcome"); outcome != "reject" {
		t.Errorf("auth.outcome = %q, want %q", outcome, "reject")
	}
}

// TestAuthSpanAttributesOnRejectInvalid verifies that when auth fails due
// to invalid token, it emits an "auth.check" span with auth.outcome="invalid"
// (issue #936).
func TestAuthSpanAttributesOnRejectInvalid(t *testing.T) {
	m := NewMiddleware("secret-key", nil, nil, nil)

	// Set up a tracing collector to capture spans.
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := m.Wrap(okHandler())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("request should be rejected, got %d", rec.Code)
	}

	// Close the exporter to drain the queue before checking spans.
	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	// Verify the span was captured with the correct attributes.
	span := coll.FindSpan(t, "auth.check")
	if span == nil {
		t.Fatal("no auth.check span found in captured spans")
	}
	if outcome := tracingtest.AttrString(span, "auth.outcome"); outcome != "invalid" {
		t.Errorf("auth.outcome = %q, want %q", outcome, "invalid")
	}
}

// TestAuthSpanAttributesOnExempt verifies that when a request is exempt
// from auth, it emits an "auth.check" span with auth.exempt=true and
// auth.outcome="accept" (issue #936).
func TestAuthSpanAttributesOnExempt(t *testing.T) {
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz"
	}
	m := NewMiddleware("secret-key", exempt, nil, nil)

	// Set up a tracing collector to capture spans.
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	h := m.Wrap(okHandler())

	req := httptest.NewRequest("GET", "/healthz", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request should succeed, got %d", rec.Code)
	}

	// Close the exporter to drain the queue before checking spans.
	if err := exp.Close(); err != nil {
		t.Fatalf("exporter Close: %v", err)
	}

	// Verify the span was captured with the correct attributes.
	span := coll.FindSpan(t, "auth.check")
	if span == nil {
		t.Fatal("no auth.check span found in captured spans")
	}
	if exemptAttr := tracingtest.AttrBool(span, "auth.exempt"); !exemptAttr {
		t.Errorf("auth.exempt = false, want true")
	}
	if outcome := tracingtest.AttrString(span, "auth.outcome"); outcome != "accept" {
		t.Errorf("auth.outcome = %q, want %q", outcome, "accept")
	}
}
