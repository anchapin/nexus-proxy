package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/observability"
	"github.com/anchapin/nexus-proxy/internal/testutil"
)

// panicHandler returns an http.HandlerFunc that panics with v after
// optionally writing headers/body when startStream is true (simulating a
// mid-stream panic).
func panicHandler(t *testing.T, startStream bool, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if startStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		panic(v)
	}
}

// TestRecover_PanicBeforeHeaderReturns500 verifies that a panic fired
// before any response bytes are written is surfaced as a clean 500 with
// the OpenAI-compatible JSON error envelope — not a TCP reset.
func TestRecover_PanicBeforeHeaderReturns500(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, false, "boom in middleware")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v\nbody: %s", err, rec.Body.String())
	}
	if body.Error.Message != "internal server error" {
		t.Errorf("error.message = %q, want %q", body.Error.Message, "internal server error")
	}
	if body.Error.Type != "internal_error" {
		t.Errorf("error.type = %q, want %q", body.Error.Type, "internal_error")
	}
}

// TestRecover_PanicAfterStreamWritesSSEErrorFrame verifies that when a
// panic occurs after the response has already started streaming
// (WriteHeader + body bytes flushed), the recover middleware appends a
// trailing SSE error frame and a [DONE] sentinel rather than attempting
// an impossible status-code change.
func TestRecover_PanicAfterStreamWritesSSEErrorFrame(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, true, "stream blew up")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// Status was already committed by the inner handler.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (already committed)", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: {\"partial\":true}") {
		t.Errorf("body missing pre-panic partial frame:\n%s", body)
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("body missing SSE error frame:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing trailing [DONE] sentinel:\n%s", body)
	}
}

// TestRecover_NoPanicPassThrough verifies the happy path is unaffected:
// a handler that returns normally produces its original status and body
// unchanged.
func TestRecover_NoPanicPassThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	})
	h := Recover(nil)(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestRecover_LogsStructuredPanic verifies the recover middleware emits a
// structured slog.Error carrying the component, the panic value, the
// request_id, and the path — the acceptance criterion from the issue.
func TestRecover_LogsStructuredPanic(t *testing.T) {
	var buf bytes.Buffer
	testutil.SetDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", "req-test-123")
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, false, 42)))

	h.ServeHTTP(httptest.NewRecorder(), r)

	logged := buf.String()
	for _, want := range []string{
		"level=ERROR",
		`msg="panic recovered"`,
		"component=recovery",
		"request_id=req-test-123",
		"path=/v1/chat/completions",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, logged)
		}
	}
}

// TestRecover_ObserverInvokedOnPanic verifies that the HandlerPanicObserver
// is invoked exactly once when a panic is recovered, receiving the request
// URL path (issue #480).
func TestRecover_ObserverInvokedOnPanic(t *testing.T) {
	var paths []string
	obs := HandlerPanicObserver(func(path string) {
		paths = append(paths, path)
	})
	h := Recover(obs)(http.HandlerFunc(panicHandler(t, false, "kaboom")))

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if len(paths) != 1 {
		t.Fatalf("observer called %d times, want 1", len(paths))
	}
	if paths[0] != "/v1/chat/completions" {
		t.Errorf("observer path = %q, want %q", paths[0], "/v1/chat/completions")
	}
}

// TestRecover_ObserverNotInvokedOnHappyPath verifies the observer is not
// called when the handler returns normally (issue #480).
func TestRecover_ObserverNotInvokedOnHappyPath(t *testing.T) {
	called := 0
	obs := HandlerPanicObserver(func(string) { called++ })
	h := Recover(obs)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if called != 0 {
		t.Fatalf("observer called %d times on happy path, want 0", called)
	}
}

// TestRecover_ObserverReceivesRouteTemplate verifies that when Recover
// wraps a ServeMux, the observer receives the mux route template (not the
// raw URL) so the Prometheus label cardinality stays bounded (issue #480).
func TestRecover_ObserverReceivesRouteTemplate(t *testing.T) {
	var observedPath string
	obs := HandlerPanicObserver(func(path string) { observedPath = path })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		panic("template test")
	})
	h := Recover(obs)(mux)

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if observedPath != "/v1/chat/completions" {
		t.Errorf("observer path = %q, want route template %q",
			observedPath, "/v1/chat/completions")
	}
}

func TestRedactPanicValue(t *testing.T) {
	tests := []struct {
		name       string
		panics     any
		wantRedact bool
	}{
		{
			name:       "string without secrets",
			panics:     "something went wrong",
			wantRedact: false,
		},
		{
			name:       "integer",
			panics:     42,
			wantRedact: false,
		},
		{
			name:       "Bearer token",
			panics:     "failed to parse Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			wantRedact: true,
		},
		{
			name:       "api_key pattern",
			panics:     "api_key=sk-12345abcdef",
			wantRedact: true,
		},
		{
			name:       "password pattern",
			panics:     "password=supersecret",
			wantRedact: true,
		},
		{
			name:       "AWS access key",
			panics:     "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			wantRedact: true,
		},
		{
			name:       "generic secret key pattern",
			panics:     "secret_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			wantRedact: true,
		},
		{
			name:       "URL with password",
			panics:     "https://user:password123@example.com/api",
			wantRedact: true,
		},
		{
			name:       "auth token pattern",
			panics:     "auth_token=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			wantRedact: true,
		},
		{
			name:       "error type with secret",
			panics:     testError{msg: "token=abc123secret"},
			wantRedact: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redacted, wasRedacted := redactPanicValue(tt.panics)
			if wasRedacted != tt.wantRedact {
				t.Errorf("redactPanicValue(%v) wasRedacted=%v, want %v", tt.panics, wasRedacted, tt.wantRedact)
			}
			if wasRedacted {
				if redacted == fmt.Sprintf("%v", tt.panics) {
					t.Errorf("redactPanicValue(%v) did not redact, got same value back", tt.panics)
				}
				if strings.Contains(redacted, "abc123") || strings.Contains(redacted, "eyJ") || strings.Contains(redacted, "supersecret") {
					t.Errorf("redactPanicValue(%v) = %q, still contains secret", tt.panics, redacted)
				}
			}
		})
	}
}

type testError struct {
	msg string
}

func (e testError) Error() string {
	return e.msg
}

// TestRedactPanicValue_PartialSecretRedaction verifies that genericSecretRe
// redacts the full secret value including any trailing content up to whitespace
// or end-of-string. The previous \S+-based pattern only matched non-whitespace
// characters, leaving trailing content (e.g., "def" in "token=sk-abc def") in the
// log. The fix uses a character class that includes space to capture the full
// secret value. The \b word boundary before the keyword prevents partial matches
// (e.g., "token" inside "auth_token") and ensures only whole keywords trigger
// redaction. See issue #931.
func TestRedactPanicValue_PartialSecretRedaction(t *testing.T) {
	tests := []struct {
		name       string
		panics     any
		wantRedact bool
		wantAbsent string // substring that should NOT be present after redaction
	}{
		{
			name:       "token with space-separated trailing text",
			panics:     "token=sk-abc def",
			wantRedact: true,
			wantAbsent: "def",
		},
		{
			name:       "auth_token with space-separated trailing text",
			panics:     "auth_token=ghp_xxx yyy",
			wantRedact: true,
			wantAbsent: "yyy",
		},
		{
			name:       "token at end of string with no trailing content",
			panics:     "token=sk-abc",
			wantRedact: true,
			wantAbsent: "sk-abc",
		},
		{
			name:       "token with slash in value and trailing text",
			panics:     "token=ghp_abc/def ghi",
			wantRedact: true,
			wantAbsent: "ghi",
		},
		{
			name:       "password with space in value",
			panics:     "password=secret pass word",
			wantRedact: true,
			wantAbsent: "pass word",
		},
		{
			name:       "credential key with dash and trailing text",
			panics:     "credential=key-123 abc",
			wantRedact: true,
			wantAbsent: "abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redacted, wasRedacted := redactPanicValue(tt.panics)
			if wasRedacted != tt.wantRedact {
				t.Errorf("redactPanicValue(%v) wasRedacted=%v, want %v", tt.panics, wasRedacted, tt.wantRedact)
			}
			if wasRedacted && tt.wantAbsent != "" {
				if strings.Contains(redacted, tt.wantAbsent) {
					t.Errorf("redactPanicValue(%v) = %q, should NOT contain absent portion %q", tt.panics, redacted, tt.wantAbsent)
				}
			}
		})
	}
}

func TestRecover_LogsRedactedPanicWithWarning(t *testing.T) {
	var buf bytes.Buffer
	testutil.SetDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, false, "Bearer sk-12345abcdef")))

	h.ServeHTTP(httptest.NewRecorder(), r)

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("log missing level=WARN, got:\n%s", logged)
	}
	if !strings.Contains(logged, "redacted before logging") {
		t.Errorf("log missing redaction notice, got:\n%s", logged)
	}
	if strings.Contains(logged, "sk-12345") {
		t.Errorf("log contains unreduced secret:\n%s", logged)
	}
}

func TestRecover_LogsNonRedactedPanicWithError(t *testing.T) {
	var buf bytes.Buffer
	testutil.SetDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, false, "something went wrong")))

	h.ServeHTTP(httptest.NewRecorder(), r)

	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("log missing level=ERROR for non-secret panic, got:\n%s", logged)
	}
	if !strings.Contains(logged, "panic recovered") {
		t.Errorf("log missing panic recovered message, got:\n%s", logged)
	}
}

// nonFlusherResponseWriter is an http.ResponseWriter that does NOT implement
// http.Flusher. It is used to verify that when a panic occurs after streaming
// has begun but the underlying writer lacks Flusher, the 500 envelope path is
// used instead of SSE framing (issue #686).
type nonFlusherResponseWriter struct {
	headers http.Header
	code    int
	written bool
	body    *bytes.Buffer
}

func newNonFlusherResponseWriter() *nonFlusherResponseWriter {
	return &nonFlusherResponseWriter{
		headers: make(http.Header),
		code:    200,
		body:    &bytes.Buffer{},
	}
}

func (n *nonFlusherResponseWriter) Header() http.Header { return n.headers }
func (n *nonFlusherResponseWriter) WriteHeader(code int) {
	n.code = code
	n.written = true
}
func (n *nonFlusherResponseWriter) Write(b []byte) (int, error) {
	n.written = true
	return n.body.Write(b)
}

// TestRecover_NonFlusherResponseWriterUses500Envelope verifies that when a
// panic occurs after streaming has begun but the underlying ResponseWriter
// does NOT implement http.Flusher, the recover middleware uses the 500 JSON
// envelope path instead of SSE framing (which would silently fail and hang
// the client). This is the acceptance criterion from issue #686.
func TestRecover_NonFlusherResponseWriterUses500Envelope(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		// Do NOT flush — and the underlying writer has no Flusher.
		panic("stream panic on non-flusher writer")
	})
	h := Recover(nil)(handler)

	// Use nonFlusherResponseWriter directly (no http.Flusher support).
	nfw := newNonFlusherResponseWriter()
	h.ServeHTTP(nfw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// Must use 500 envelope, not SSE framing.
	if nfw.code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (non-flusher should use 500 envelope)", nfw.code, http.StatusInternalServerError)
	}
	if ct := nfw.headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (non-flusher should use 500 envelope)", ct)
	}
	// Must NOT contain SSE frames since Flusher was not available.
	body := nfw.body.String()
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("body should not contain SSE [DONE] sentinel for non-flusher writer, got: %s", body)
	}
	if strings.Contains(body, "data: {\"error\":") {
		t.Errorf("body should not contain SSE error frame for non-flusher writer, got: %s", body)
	}
	// Must contain the 500 envelope.
	if !strings.Contains(body, `"message":"internal server error"`) {
		t.Errorf("body missing 500 envelope, got: %s", body)
	}
}

// TestRecover_FlusherResponseWriterUsesSSEPath verifies that when a panic
// occurs after streaming has begun AND the underlying ResponseWriter
// implements http.Flusher (e.g. httptest.ResponseRecorder), the recover
// middleware uses the SSE error frame path as expected.
func TestRecover_FlusherResponseWriterUsesSSEPath(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(panicHandler(t, true, "stream panic with flusher")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// Status was already committed by the inner handler.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (already committed)", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: {\"partial\":true}") {
		t.Errorf("body missing pre-panic partial frame:\n%s", body)
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("body missing SSE error frame:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing trailing [DONE] sentinel:\n%s", body)
	}
}

// failingFlusherResponseWriter is an http.ResponseWriter that implements
// http.Flusher but fails on Write after a configurable number of bytes
// have been written. It is used to simulate SSE write failures in the
// panic recovery path.
type failingFlusherResponseWriter struct {
	headers      http.Header
	code         int
	written      bool
	failAfterN   int
	bytesWritten int
	flusher      http.Flusher
}

func newFailingFlusherResponseWriter(failAfterN int) *failingFlusherResponseWriter {
	return &failingFlusherResponseWriter{
		headers:    make(http.Header),
		code:       200,
		failAfterN: failAfterN,
	}
}

func (f *failingFlusherResponseWriter) Header() http.Header { return f.headers }
func (f *failingFlusherResponseWriter) WriteHeader(code int) {
	f.code = code
	f.written = true
}
func (f *failingFlusherResponseWriter) Write(b []byte) (int, error) {
	f.written = true
	if f.bytesWritten+len(b) > f.failAfterN {
		f.bytesWritten = f.failAfterN + 1
		return len(b), errors.New("SSE write failure")
	}
	f.bytesWritten += len(b)
	return len(b), nil
}

func (f *failingFlusherResponseWriter) Flush() {
	if f.flusher != nil {
		f.flusher.Flush()
	}
}

func (f *failingFlusherResponseWriter) SetFlusher(flusher http.Flusher) {
	f.flusher = flusher
}

// TestRecover_SSEWriteFailureLogsAndIncrementsCounter verifies that when a
// panic occurs after streaming has begun and the SSE error frame write
// fails, the error is logged via slog.Error with request_id and
// component='recovery', and the panic SSE write failures counter is
// incremented. This is the acceptance criterion from issue #1115.
func TestRecover_SSEWriteFailureLogsAndIncrementsCounter(t *testing.T) {
	var logBuf bytes.Buffer
	testutil.SetDefault(t, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var counter uint64
	observability.SetPanicSSEWriteFailuresCounter(&counter)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("stream panic")
	})
	h := Recover(nil)(handler)

	fw := newFailingFlusherResponseWriter(5)
	fw.SetFlusher(http.Flusher(nil))
	h.ServeHTTP(fw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	logged := logBuf.String()
	if !strings.Contains(logged, "component=recovery") {
		t.Errorf("log missing component=recovery:\n%s", logged)
	}
	if !strings.Contains(logged, "panic SSE") {
		t.Errorf("log missing panic SSE error message:\n%s", logged)
	}

	if counter == 0 {
		t.Errorf("panicSSEWriteFailures counter = %d, want > 0", counter)
	}
}

func TestDebug(t *testing.T) {
	result, _ := redactPanicValue("auth_token=ghp_xxx yyy")
	t.Logf("Result: %q", result)

	// Also print the regex
	t.Logf("genericSecretRe pattern: %s", genericSecretRe.String())
}
