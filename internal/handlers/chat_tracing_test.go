package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// TestChatNoTracerIsNoOp confirms the hot path is unaffected when
// Tracer is nil — no panic, no allocation beyond a single nil-check,
// upstream call still succeeds. This is the critical
// "NEXUS_TRACING_ENDPOINT=\"\" → zero overhead" contract.
func TestChatNoTracerIsNoOp(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}

// TestChatInboundTraceparentPropagates verifies a W3C traceparent
// header from the caller survives the chat handler and is forwarded
// to the upstream call as the parent of the upstream span's own
// traceparent.
func TestChatInboundTraceparentPropagates(t *testing.T) {
	// 16 hex chars for the span id (W3C mandates 8 bytes / 16
	// hex); 20 chars would be rejected by ParseTraceparent.
	const inboundTP = "00-0af7651916cd43dd8448eb211c80319c-aaaaaaaaaaaaaaaa-01"

	// Spin up a throwaway collector so the exporter is non-nil —
	// the test only cares about the traceparent HEADER on the
	// outbound POST, not the OTLP body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	deps, rt := baseDeps(t)
	deps.Tracer = tracing.NewExporter(tracing.ExporterConfig{
		Endpoint:  srv.URL,
		QueueSize: 8,
	})
	defer deps.Tracer.Close()

	var seenTP string
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		seenTP = r.Header.Get("traceparent")
		_, _ = w.Write([]byte("ok"))
	})

	// Force the routing decision to frontier via the VRAM guardrail.
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("traceparent", inboundTP)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}

	if seenTP == "" {
		t.Fatal("upstream call did not carry traceparent header")
	}
	// The upstream traceparent MUST carry the inbound trace id so
	// distributed traces stay correlated across processes.
	const wantPrefix = "00-0af7651916cd43dd8448eb211c80319c-"
	if !strings.HasPrefix(seenTP, wantPrefix) {
		t.Errorf("upstream traceparent = %q, want prefix %q", seenTP, wantPrefix)
	}
	// The span id half must differ from the inbound one (otherwise
	// we are just echoing back the header).
	const inboundSpan = "aaaaaaaaaaaaaaaa"
	if strings.HasSuffix(seenTP, "-"+inboundSpan+"-01") {
		t.Errorf("upstream traceparent echoes inbound span id: %q", seenTP)
	}
}

// TestChatTracerWiresSpansToCollector is the end-to-end check that
// when the operator wires a real exporter, the chat handler actually
// submits a span tree. It captures the OTLP/JSON body via an
// httptest.Server so the assertions are robust against future
// exporter tweaks (field order, batch boundaries, ...).
func TestChatTracerWiresSpansToCollector(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := tracing.NewExporter(tracing.ExporterConfig{
		Endpoint:  srv.URL,
		QueueSize: 32,
		Timeout:   2 * time.Second,
	})
	if exp == nil {
		t.Fatal("exporter is nil")
	}
	defer exp.Close()

	deps, rt := baseDeps(t)
	deps.Tracer = exp
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// Force frontier via the guardrail (same shortcut as the other
	// tests in this file).
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}

	// Close drains the queue. Without this the test would race the
	// export goroutine.
	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("collector received no spans")
	}
	// Walk every captured body and collect span names + attrs. The
	// exporter batches spans, so multiple bodies is expected.
	seen := map[string]bool{}
	var attrs []map[string]any
	for _, raw := range bodies {
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name       string     `json:"name"`
						Attributes []spanAttr `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal: %v\nbody=%s", err, raw)
		}
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					seen[sp.Name] = true
					for _, a := range sp.Attributes {
						attrs = append(attrs, map[string]any{"name": sp.Name, "key": a.Key, "value": a.Value})
					}
				}
			}
		}
	}

	// Required span names from the issue.
	for _, want := range []string{"nexus.chat_completions", "route.guardrail", "upstream.frontier"} {
		if !seen[want] {
			t.Errorf("missing span %q; saw %v", want, mapKeys(seen))
		}
	}

	// Required attribute keys on the root span (from the issue AC).
	rootAttrs := attrsBySpan(attrs, "nexus.chat_completions")
	for _, key := range []string{"route", "model", "input_tokens", "ttft_ms", "total_latency_ms"} {
		if _, ok := rootAttrs[key]; !ok {
			t.Errorf("root span missing attribute %q; have %v", key, mapKeys(rootAttrs))
		}
	}
}

type spanAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func attrsBySpan(attrs []map[string]any, name string) map[string]any {
	out := map[string]any{}
	for _, a := range attrs {
		if a["name"] == name {
			if k, ok := a["key"].(string); ok {
				out[k] = a["value"]
			}
		}
	}
	return out
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestChatParseTraceparentRejectsMalformedInbound confirms a
// malformed inbound traceparent does not crash the handler — it
// falls through to a fresh trace id so the request still completes.
func TestChatParseTraceparentRejectsMalformedInbound(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("traceparent", "garbage")
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}
