package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	h := Recover()(http.HandlerFunc(panicHandler(t, false, "boom in middleware")))

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
	h := Recover()(http.HandlerFunc(panicHandler(t, true, "stream blew up")))

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
	h := Recover()(inner)

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
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", "req-test-123")
	h := Recover()(http.HandlerFunc(panicHandler(t, false, 42)))

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
