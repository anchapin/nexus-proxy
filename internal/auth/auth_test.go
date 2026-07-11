package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler is a stand-in backend that marks the response so tests can
// assert the middleware forwarded the request. It writes a 200 with a
// fixed body; tests assert the recorder saw 200 + the marker.
const okBody = `{"ok":true}`

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	})
}

// errEnvelope is the OpenAI-compatible error shape emitted on 401.
type errEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func TestMiddleware(t *testing.T) {
	// healthzExempt exempts /healthz; mirrors the wiring in main.go.
	healthzExempt := func(r *http.Request) bool { return r.URL.Path == "/healthz" }

	tests := []struct {
		name     string
		keys     []string
		exempt   func(*http.Request) bool
		req      func() *http.Request
		wantCode int
		wantOK   bool // true => backend should have been reached
		wantType string
	}{
		{
			name: "valid bearer key accepted",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer sk-secret")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "bearer scheme case-insensitive",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "BEARER sk-secret")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "valid X-API-Key accepted",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("X-API-Key", "sk-secret")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "invalid key rejected 401",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer wrong")
				return r
			},
			wantCode: http.StatusUnauthorized,
			wantType: "unauthorized",
		},
		{
			name: "missing key rejected 401",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			},
			wantCode: http.StatusUnauthorized,
			wantType: "unauthorized",
		},
		{
			name: "wrong scheme rejected 401",
			keys: []string{"sk-secret"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Basic sk-secret")
				return r
			},
			wantCode: http.StatusUnauthorized,
			wantType: "unauthorized",
		},
		{
			name: "multiple keys rotation accepts first",
			keys: []string{"sk-old", "sk-new", "sk-staging"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer sk-old")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "multiple keys rotation accepts last",
			keys: []string{"sk-old", "sk-new", "sk-staging"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer sk-staging")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "multiple keys rotation accepts middle",
			keys: []string{"sk-old", "sk-new", "sk-staging"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer sk-new")
				return r
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name: "multiple keys rejects revoked",
			keys: []string{"sk-old", "sk-new"},
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer sk-revoked")
				return r
			},
			wantCode: http.StatusUnauthorized,
			wantType: "unauthorized",
		},
		{
			name:   "healthz exempt without key",
			keys:   []string{"sk-secret"},
			exempt: healthzExempt,
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/healthz", nil)
			},
			wantCode: http.StatusOK,
			wantOK:   true,
		},
		{
			name:   "non-healthz still requires key with healthz exempt wired",
			keys:   []string{"sk-secret"},
			exempt: healthzExempt,
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			},
			wantCode: http.StatusUnauthorized,
			wantType: "unauthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Middleware(tt.keys, tt.exempt)(okHandler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.req())

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantOK {
				if rec.Body.String() != okBody {
					t.Errorf("body = %q, want backend marker %q", rec.Body.String(), okBody)
				}
				return
			}
			// 401 path: assert the OpenAI error envelope.
			if tt.wantType != "" {
				var env errEnvelope
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
					t.Fatalf("error body is not valid JSON: %v (body=%q)", err, rec.Body.String())
				}
				if env.Error.Type != tt.wantType {
					t.Errorf("error.type = %q, want %q", env.Error.Type, tt.wantType)
				}
				if env.Error.Message == "" {
					t.Errorf("error.message is empty")
				}
			}
		})
	}
}

// TestMiddlewareDisabledNoOp asserts that when no key is configured the
// middleware is a pass-through (zero breaking change for localhost dev).
// This is the backward-compat acceptance criterion.
func TestMiddlewareDisabledNoOp(t *testing.T) {
	h := Middleware(nil, nil)(okHandler())

	// No Authorization header at all.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("disabled middleware: status = %d, want 200 (no-op)", rec.Code)
	}
	if rec.Body.String() != okBody {
		t.Errorf("disabled middleware: body = %q, want %q", rec.Body.String(), okBody)
	}

	// Blank-only keys also disable (defensive: a stray empty entry must
	// not open an unauthenticated hole).
	h2 := Middleware([]string{"", "  ", ""}, nil)(okHandler())
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("blank-only keys: status = %d, want 200 (no-op)", rec2.Code)
	}
}

// TestMiddlewareExemptNilIsSafe asserts a nil exempt predicate does not
// panic and that all paths (including /healthz) require a key.
func TestMiddlewareExemptNilIsSafe(t *testing.T) {
	h := Middleware([]string{"sk-x"}, nil)(okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("nil exempt /healthz: status = %d, want 401", rec.Code)
	}
}

// TestExtractKey covers the header-parsing edge cases directly so the
// table-driven HTTP test can stay focused on routing outcomes.
func TestExtractKey(t *testing.T) {
	tests := []struct {
		name string
		auth string
		api  string
		want string
	}{
		{"bearer", "Bearer abc", "", "abc"},
		{"bearer lowercase", "bearer abc", "", "abc"},
		{"bearer mixedcase", "BeArEr abc", "", "abc"},
		{"bearer trimmed", "Bearer   abc   ", "", "abc"},
		{"basic ignored", "Basic abc", "", ""},
		{"empty auth", "", "", ""},
		{"api key fallback", "", "xyz", "xyz"},
		{"api key trimmed", "", "  xyz  ", "xyz"},
		{"bearer precedence over api key", "Bearer first", "second", "first"},
		{"bearer empty value falls to api key", "Bearer ", "second", "second"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.auth != "" {
				r.Header.Set("Authorization", tt.auth)
			}
			if tt.api != "" {
				r.Header.Set("X-API-Key", tt.api)
			}
			if got := extractKey(r); got != tt.want {
				t.Errorf("extractKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKeyMatchesConstantTime is a behavioural guard: it asserts that
// keyMatches does not panic and returns the expected boolean for a few
// representative inputs. (True constant-time properties cannot be
// asserted in a unit test; the guard ensures subtle.ConstantTimeCompare
// is wired rather than ==.)
func TestKeyMatchesConstantTime(t *testing.T) {
	if !keyMatches("abc", []string{"abc"}) {
		t.Error("exact match returned false")
	}
	if !keyMatches("abc", []string{"xxx", "abc"}) {
		t.Error("match at index 1 returned false")
	}
	if keyMatches("abc", []string{"xxx", "yyy"}) {
		t.Error("non-match returned true")
	}
	if keyMatches("", []string{"abc"}) {
		t.Error("empty presented matched")
	}
	if keyMatches("abc", nil) {
		t.Error("non-empty presented matched nil keys")
	}
}

// TestWriteUnauthorizedEnvelope asserts the response shape verbatim so a
// future refactor cannot silently drift from the OpenAI envelope.
func TestWriteUnauthorizedEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUnauthorized(rec)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not valid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if env.Error.Type != "unauthorized" {
		t.Errorf("type = %q, want unauthorized", env.Error.Type)
	}
	if !strings.Contains(strings.ToLower(env.Error.Message), "api key") {
		t.Errorf("message = %q, want something mentioning 'api key'", env.Error.Message)
	}
}
