package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracingtest"
)

// TestBodySizeTracingAcceptPath verifies that a request under the
// cap emits a `request.body_size` span with max_bytes and bytes_read
// attributes (issue #71 AC: span coverage for the four middleware
// paths including the body-size pre-handler check).
func TestBodySizeTracingAcceptPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	deps, rt := baseDeps(t)
	deps.Tracer = exp
	// "please fix the css" -> DSL hits local.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "request.body_size")
	if s == nil {
		t.Fatalf("missing request.body_size span; got %d spans", len(coll.Spans(t)))
	}
	if got := tracingtest.AttrInt(s, "max_bytes"); got <= 0 {
		t.Errorf("max_bytes = %d, want > 0", got)
	}
	if got := tracingtest.AttrInt(s, "bytes_read"); got != int64(len(body)) {
		t.Errorf("bytes_read = %d, want %d", got, len(body))
	}
}

// TestBodySizeTracingRejectPath verifies that an over-cap request
// stamps outcome="oversized" and Status=Error on the
// request.body_size span so a 413 in the trace view is explainable.
func TestBodySizeTracingRejectPath(t *testing.T) {
	coll := tracingtest.NewCapturedSpans(t)
	exp := tracingtest.StartTestExporter(t, coll)
	defer exp.Close()

	deps, _ := baseDeps(t)
	deps.Tracer = exp
	const cap = 1024
	deps.Config.MaxBodyBytes = cap

	const prefix = `{"messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	contentLen := cap - len(prefix) - len(suffix) + 1 // +1 byte over
	body := prefix + strings.Repeat("x", contentLen) + suffix

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rw.Code, rw.Body.String())
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "request.body_size")
	if s == nil {
		t.Fatalf("missing request.body_size span; got %d", len(coll.Spans(t)))
	}
	if got := tracingtest.AttrString(s, "outcome"); got != "oversized" {
		t.Errorf("outcome = %q, want oversized", got)
	}
	if !strings.Contains(s.Status.Code, "ERROR") {
		t.Errorf("status code = %q, want ERROR", s.Status.Code)
	}
}
