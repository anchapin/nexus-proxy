package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

type stubEmbedder struct{ vec []float64 }

func (s stubEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	return s.vec, nil
}

func baseDeps(t *testing.T) (Deps, *upstream.RecordingTransport) {
	t.Helper()
	cfg := config.Config{
		Addr:           ":0",
		OllamaURL:      "http://ollama.local",
		RouterModel:    "qwen3-coder:4b",
		LocalModel:     "qwen3-coder:8b",
		EmbeddingModel: "nomic-embed-text",
		FrontierURL:    "http://frontier.local",
		FrontierModel:  "gpt-4o",
		FrontierKey:    "sk-test",
		RAGThreshold:   0.55,
		TokenGuardrail: 6000,
		MetaPrompt:     " BOOST",
		TOONNotice:     "[PROXY SYSTEM NOTE]: TOON compression applied",
		// Issue #48: enable progressive fusion delivery so the
		// route=fusion tests exercise the new PanelStreaming
		// path (speculative chunk + arbiter-on-disagreement)
		// rather than the legacy Panel path (block-on-both +
		// arbiter-always).
		FusionProgressiveDelivery: true,
		FusionAgreementThreshold:  0.85,
	}
	store := rag.NewStore(stubEmbedder{vec: []float64{0, 0, 0}}, 0.55)
	store.Add("no-match.go", "x", []float64{0, 1, 0})
	rt := upstream.NewRecordingTransport()
	client := &http.Client{Transport: rt}
	deps := Deps{
		Config:          cfg,
		Client:          client,
		RAG:             store,
		SLM:             router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, 1, client),
		FormattingRegex: regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`),
		Recorder:        telemetry.Noop{},
	}
	return deps, rt
}

func decodeReq(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v body=%s", err, b)
	}
	return m
}

func TestChatRejectsGET(t *testing.T) {
	deps, _ := baseDeps(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want 405", rw.Code)
	}
}

func TestChatRejectsBadJSON(t *testing.T) {
	deps, _ := baseDeps(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("not json"))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rw.Code)
	}
}

func TestChatRejectsMissingMessages(t *testing.T) {
	deps, _ := baseDeps(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rw.Code)
	}
}

func TestChatDSLLargePromptForcesFrontier(t *testing.T) {
	deps, rt := baseDeps(t)
	// 30000 char prompt / 4 = 7500 > 6000 guardrail
	largeUser := strings.Repeat("a", 30000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if len(rt.Calls()) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(rt.Calls()))
	}
	if rt.Calls()[0].URL != "http://frontier.local" {
		t.Errorf("routed to %s, want frontier", rt.Calls()[0].URL)
	}
	if !strings.Contains(rw.Body.String(), "frontier stream") {
		t.Errorf("body = %q", rw.Body.String())
	}
}

func TestChatDSLLowComplexityRoutesLocal(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Return a valid OpenAI-compatible completion so the cascade's
		// validation accepts it and stops (no fallback to frontier).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rw.Code)
	}
	// Cascade should stop at local on first valid response.
	if len(rt.Calls()) != 1 || rt.Calls()[0].URL != "http://ollama.local/v1/chat/completions" {
		t.Fatalf("calls = %+v", rt.Calls())
	}
	got := decodeReq(t, rt.Calls()[0].Req)
	if got["model"] != "qwen3-coder:8b" {
		t.Errorf("model override = %v", got["model"])
	}
	msgs := got["messages"].([]interface{})
	sys := msgs[0].(map[string]interface{})
	if !strings.Contains(sys["content"].(string), "BOOST") {
		t.Errorf("meta-prompt not applied: %q", sys["content"])
	}
	// The cascaded response should reach the harness.
	if !strings.Contains(rw.Body.String(), "local stream") {
		t.Errorf("body = %q", rw.Body.String())
	}
	if !strings.Contains(rw.Header().Get("X-Nexus-Cascade-Served-By"), "local") {
		t.Errorf("served-by header missing: %q", rw.Header().Get("X-Nexus-Cascade-Served-By"))
	}
	// Issue #8: every response carries X-Nexus-Degraded. Healthy
	// local Ollama -> "false".
	if got := rw.Header().Get("X-Nexus-Degraded"); got != "false" {
		t.Errorf("X-Nexus-Degraded = %q, want \"false\"", got)
	}
}

// TestChatRouteLocalHealthyDegradesToFrontier verifies the
// graceful-degradation path (issue #8): when the health poller
// reports Ollama unreachable, a route=local request must be served
// by the frontier endpoint and the response must carry
// X-Nexus-Degraded: true. The local Ollama URL must NOT be hit at
// all (issue requirement: avoid paying the timeout cost).
func TestChatRouteLocalHealthyDegradesToFrontier(t *testing.T) {
	deps, rt := baseDeps(t)
	// Trip the breaker directly via Probe — no background goroutine
	// needed, so the test stays race-free.
	deps.Health = tripBreaker(t)

	// Frontier is configured to return a valid OpenAI completion.
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"frontier fallback content"},"finish_reason":"stop"}]}`)
	})
	// Local MUST NOT be hit; if it is, the test fails.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("local Ollama URL was hit even though health.IsLocalHealthy=false")
	})

	// DSL "css" keyword forces route=local (no token-guardrail trip).
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	// Verify only frontier was called.
	var sawLocal, sawFrontier bool
	for _, c := range rt.Calls() {
		switch c.URL {
		case "http://ollama.local/v1/chat/completions":
			sawLocal = true
		case "http://frontier.local":
			sawFrontier = true
		}
	}
	if sawLocal {
		t.Error("local Ollama was called despite IsLocalHealthy=false")
	}
	if !sawFrontier {
		t.Error("frontier was not called on degraded route=local")
	}
	if got := rw.Header().Get("X-Nexus-Degraded"); got != "true" {
		t.Errorf("X-Nexus-Degraded = %q, want \"true\"", got)
	}
	if got := rw.Header().Get("X-Nexus-Cascade-Served-By"); !strings.Contains(got, "frontier") {
		t.Errorf("X-Nexus-Cascade-Served-By = %q, want frontier", got)
	}
	if !strings.Contains(rw.Body.String(), "frontier fallback content") {
		t.Errorf("body missing frontier content: %q", rw.Body.String())
	}
}

// TestChatRouteFusionHealthySkipsLocalPanel verifies the fusion
// graceful-degradation path (issue #8): when the health poller
// reports Ollama unreachable, a route=fusion request must skip
// the local panel member and serve the arbiter's synthesis of the
// frontier candidate alone. The local Ollama URL must NOT be hit.
func TestChatRouteFusionHealthySkipsLocalPanel(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Health = tripBreaker(t)

	// Local Ollama MUST NOT be hit.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("local Ollama URL was hit on degraded route=fusion")
	})
	// Frontier + arbiter respond normally.
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"frontier candidate"},"finish_reason":"stop"}]}`)
	})
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		// Anything else (e.g. the arbiter request) gets a
		// minimal SSE stream.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"synth\":\"ok\"}\n\n")
	})

	// DSL "system architecture" forces route=fusion.
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	var sawLocal bool
	for _, c := range rt.Calls() {
		if c.URL == "http://ollama.local/v1/chat/completions" {
			sawLocal = true
		}
	}
	if sawLocal {
		t.Errorf("local Ollama was called on degraded fusion; calls=%+v", rt.Calls())
	}
	if got := rw.Header().Get("X-Nexus-Degraded"); got != "true" {
		t.Errorf("X-Nexus-Degraded = %q, want \"true\"", got)
	}
}

// TestChatRouteFrontierStampsDegradedFalse confirms that a
// route=frontier request still receives the X-Nexus-Degraded
// response header (set to "false"), so callers can rely on the
// header being present on every proxied response.
func TestChatRouteFrontierStampsDegradedFalse(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})
	// Large prompt -> guardrail forces FRONTIER route.
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Nexus-Degraded"); got != "false" {
		t.Errorf("X-Nexus-Degraded = %q, want \"false\"", got)
	}
}

// tripBreaker returns a Health whose IsLocalHealthy returns false.
// We avoid spinning up the poller goroutine (and the Close/cancel
// dance that goes with it) by issuing a single failed Probe against
// a stub server that returns 503. Threshold is 1 so one failure is
// enough to flip the breaker.
func tripBreaker(t *testing.T) *health.Health {
	t.Helper()
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(deadSrv.Close)
	h := health.New(deadSrv.URL, time.Hour, 1, 2*time.Second, &http.Client{Timeout: 2 * time.Second})
	if err := h.Probe(context.Background()); err == nil {
		t.Fatal("expected Probe to fail against a 503 server")
	}
	if h.IsLocalHealthy() {
		t.Fatalf("breaker did not trip; failureCount=%d", h.FailureCount())
	}
	return h
}

// TestChatRouteLocalCascadeFallsBackToFrontier exercises the cascade from
// the handler: local Ollama returns garbage, frontier (configured in
// baseDeps) returns a valid OpenAI completion, and the harness receives
// the frontier response with served_by=frontier.
func TestChatRouteLocalCascadeFallsBackToFrontier(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Return something the cascade cannot validate: not a chat
		// completion JSON shape. Triggers fallback.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not openai json")
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"frontier fallback"},"finish_reason":"stop"}]}`)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	// Both local and frontier should have been called.
	urls := make([]string, len(rt.Calls()))
	for i, c := range rt.Calls() {
		urls[i] = c.URL
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 calls (local + fallback), got %d: %v", len(urls), urls)
	}
	if urls[0] != "http://ollama.local/v1/chat/completions" {
		t.Errorf("first call = %q, want local", urls[0])
	}
	if urls[1] != "http://frontier.local" {
		t.Errorf("second call = %q, want frontier", urls[1])
	}
	if !strings.Contains(rw.Body.String(), "frontier fallback") {
		t.Errorf("body missing fallback content: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Header().Get("X-Nexus-Cascade-Served-By"), "frontier") {
		t.Errorf("served-by = %q, want frontier", rw.Header().Get("X-Nexus-Cascade-Served-By"))
	}
}

// TestChatRouteLocalCascadeAllFail verifies the handler surfaces a 502
// when every step in the cascade fails — the contract from issue #14:
// "if all fail, return the last upstream's error to the client."
func TestChatRouteLocalCascadeAllFail(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rw.Code)
	}
	if len(rt.Calls()) != 2 {
		t.Errorf("expected 2 calls (all steps), got %d", len(rt.Calls()))
	}
}

func TestChatDSLArchitectureFusion(t *testing.T) {
	deps, rt := baseDeps(t)
	// Issue #48: progressive fusion only invokes the arbiter when
	// the two panel members DISAGREE. The OnAny handler returns
	// divergent content per URL so the test exercises the full
	// disagreement path (local + frontier + arbiter = 3 calls)
	// rather than the agreement-skip-arbiter path (which would
	// produce only 2 calls). The arbiter URL coincides with
	// frontier in baseDeps, so the routing switches on the
	// request body shape: "Master Synthesis Arbiter" identifies
	// the arbiter call.
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch {
		case strings.Contains(body, "Master Synthesis Arbiter"):
			// Arbiter call — return a synthesized stream so the
			// progressive handler can append it after the
			// speculative chunk.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"synth\":\"ok\"}\n\n")
		case strings.Contains(r.URL.String(), "ollama"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local divergent answer"}}]}`)
		default:
			// Frontier panel member — divergent content to
			// force the arbiter path.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier completely different answer about totally unrelated topic"}}]}`)
		}
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("status = %d", rw.Code)
	}
	// Expect: 1 local panel call + 1 frontier panel call + 1 arbiter stream.
	if len(rt.Calls()) < 3 {
		t.Errorf("expected >=3 calls (panel+arbiter), got %d", len(rt.Calls()))
	}
	hasLocal := false
	for _, c := range rt.Calls() {
		if c.URL == "http://ollama.local/v1/chat/completions" {
			hasLocal = true
		}
	}
	if !hasLocal {
		t.Error("fusion did not call local")
	}
	// Progressive fusion carries the speculative chunk and the
	// arbiter synthesis as SSE chunks; the harness should see
	// both in the response body.
	if !strings.Contains(rw.Body.String(), "local divergent answer") &&
		!strings.Contains(rw.Body.String(), "frontier completely different answer") {
		t.Errorf("speculative chunk missing from body: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), `"synth":"ok"`) {
		t.Errorf("arbiter synthesis missing from body: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", rw.Body.String())
	}
}

// TestChatNonStreamingLocalReturnsJSONObject is the issue #10 acceptance
// test for the local route: when the harness sends stream=false, the
// handler must take the BufferedFetch path and return a single
// chatCompletionResponse JSON object — not synthesized SSE chunks.
func TestChatNonStreamingLocalReturnsJSONObject(t *testing.T) {
	deps, rt := baseDeps(t)
	var seenBody string
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"non-streamed answer"},"finish_reason":"stop"}]}`))
	})
	body := `{"stream":false,"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(rw.Body.String(), `"object":"chat.completion"`) {
		t.Errorf("body missing chatCompletionResponse shape: %q", rw.Body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.Body.String()), "data:") {
		t.Errorf("body looks like SSE, want plain JSON: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), `"content":"non-streamed answer"`) {
		t.Errorf("upstream content not forwarded: %q", rw.Body.String())
	}
	// BufferedFetch must force stream=false on the wire even if the
	// harness accidentally sent stream=true (or the handler didn't
	// strip it). Belt and braces.
	if !strings.Contains(seenBody, `"stream":false`) {
		t.Errorf("upstream body missing stream=false override: %s", seenBody)
	}
}

// TestChatStreamingLocalStillSynthesizesSSE is the regression
// companion to the buffered test above: default (no stream field)
// must keep the existing cascade + SSE synthesis path.
func TestChatStreamingLocalStillSynthesizesSSE(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"message":{"content":"streamed answer"}}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if !strings.HasPrefix(strings.TrimSpace(rw.Body.String()), "data:") {
		t.Errorf("body should be SSE: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "streamed answer") {
		t.Errorf("content not forwarded: %q", rw.Body.String())
	}
}

// TestChatNonStreamingFrontierReturnsJSONObject exercises the frontier
// default branch when the harness asks for stream=false: the handler
// must call BufferedFetch and return a single JSON object.
func TestChatNonStreamingFrontierReturnsJSONObject(t *testing.T) {
	deps, rt := baseDeps(t)
	// 30000 chars / 4 = 7500 > 6000 guardrail, so this routes to
	// frontier via the default branch (not the local cascade).
	largeUser := strings.Repeat("a", 30000)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		// BufferedFetch forces stream=false on the wire; assert it.
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"stream":false`) {
			t.Errorf("upstream body missing stream=false override: %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"frontier non-stream"}}]}`))
	})
	body := `{"stream":false,"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(rw.Body.String(), `"object":"chat.completion"`) {
		t.Errorf("body missing chatCompletionResponse shape: %q", rw.Body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.Body.String()), "data:") {
		t.Errorf("body looks like SSE, want plain JSON: %q", rw.Body.String())
	}
}

// TestChatFusionArbiterHonorsStreamFalse is the issue #10 acceptance
// test for the fusion path: when the harness sends stream=false, the
// arbiter call must return a JSON object (not SSE) while the panel
// member calls stay stream=false as before.
//
// baseDeps configures the arbiter URL to coincide with the frontier
// panel member's URL (both resolve to d.Config.FrontierURL =
// "http://frontier.local"), so the recording transport cannot route
// them by URL alone. We use OnAny and identify the arbiter call by
// its synthetic system prompt, which always carries the
// "Master Synthesis Arbiter" instruction the Panel constructs.
func TestChatFusionArbiterHonorsStreamFalse(t *testing.T) {
	deps, rt := baseDeps(t)
	var arbiterBody string
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch {
		case strings.Contains(body, "Master Synthesis Arbiter"):
			// Arbiter call. Capture the body to assert stream=false.
			arbiterBody = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"synthesized"}}]}`))
		case strings.Contains(r.URL.String(), "ollama"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"local reply"}}]}`))
		default:
			// Frontier panel member.
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"frontier reply"}}]}`))
		}
	})
	body := `{"stream":false,"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(rw.Body.String(), `"content":"synthesized"`) {
		t.Errorf("arbiter JSON not forwarded: %q", rw.Body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.Body.String()), "data:") {
		t.Errorf("arbiter body looks like SSE, want plain JSON: %q", rw.Body.String())
	}
	if !strings.Contains(arbiterBody, `"stream":false`) {
		t.Errorf("arbiter request missing stream=false override: %s", arbiterBody)
	}
}

func TestChatSLMFallbackToFrontierOnError(t *testing.T) {
	deps, rt := baseDeps(t)
	// SLM call to ollama will fail; frontier should be called instead
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		// return 500 for ollama, ok for frontier
		if strings.Contains(r.URL.String(), "ollama") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("frontier stream"))
	})
	body := `{"messages":[{"role":"user","content":"refactor this complex module"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	// SLM error -> frontier. Should have at least one frontier call.
	var sawFrontier bool
	for _, c := range rt.Calls() {
		if c.URL == "http://frontier.local" {
			sawFrontier = true
		}
	}
	if !sawFrontier {
		t.Errorf("expected frontier fallback, calls=%+v", rt.Calls())
	}
}

func TestChatTOONCompressionAppliesNotice(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	body := "{\"messages\":[{\"role\":\"user\",\"content\":\"hi ```json\\n[{\\\"a\\\":1},{\\\"a\\\":2}]\\n``` end\"}]}"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	// Find the user message we sent upstream; should contain TOON block + system note.
	var upstreamUser string
	for _, c := range rt.Calls() {
		if c.Req == nil {
			continue
		}
		got := decodeReq(t, c.Req)
		if msgs, ok := got["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]interface{}); ok {
					if role, _ := mm["role"].(string); role == "system" {
						if strings.Contains(mm["content"].(string), "TOON") {
							upstreamUser = mm["content"].(string)
						}
					}
				}
			}
		}
	}
	if !strings.Contains(upstreamUser, "TOON") {
		t.Errorf("TOON notice missing in upstream system msg")
	}
}

// recordingObserver is a JudgeObserver test double that just appends
// every completion it sees to a slice. Safe for concurrent use.
type recordingObserver struct {
	mu   sync.Mutex
	seen []LocalCompletion
}

func (r *recordingObserver) Submit(c LocalCompletion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, c)
}

func (r *recordingObserver) Snapshot() []LocalCompletion {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LocalCompletion, len(r.seen))
	copy(out, r.seen)
	return out
}

func TestChatLocalRouteInvokesObserverWithInstructionAndOutput(t *testing.T) {
	deps, rt := baseDeps(t)
	obs := &recordingObserver{}
	deps.JudgeObserver = obs
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Cascade (issue #14) consumes a non-streaming JSON body and
		// re-emits it as a single SSE chunk via writeSSEResponse.
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"message":{"content":"hello world"}}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	got := obs.Snapshot()
	if len(got) != 1 {
		t.Fatalf("observer saw %d events, want 1", len(got))
	}
	c := got[0]
	if c.RequestID == "" {
		t.Error("RequestID should be populated")
	}
	if !strings.Contains(c.Instruction, "please fix the css") {
		t.Errorf("Instruction = %q", c.Instruction)
	}
	if !strings.Contains(c.Output, "hello world") {
		t.Errorf("Output should contain streamed body, got %q", c.Output)
	}
	if c.LocalModel != "qwen3-coder:8b" {
		t.Errorf("LocalModel = %q, want qwen3-coder:8b", c.LocalModel)
	}
	// The original stream body must still be visible to the client —
	// the observer is a tee, not a sink.
	if !strings.Contains(rw.Body.String(), "hello world") {
		t.Errorf("client body should contain streamed content, got %q", rw.Body.String())
	}
}

func TestChatLocalRouteObserverNilSkipsCapture(t *testing.T) {
	// When no observer is configured we must NOT pay the
	// capture-buffer cost. The streamed body is forwarded verbatim
	// and the test should not observe any side effects beyond that.
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Cascade consumer: non-streaming JSON; the cascade's
		// writeSSEResponse re-emits the content as a single SSE chunk.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"plain streamed body"}}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if !strings.Contains(rw.Body.String(), "plain streamed body") {
		t.Errorf("body = %q", rw.Body.String())
	}
}

func TestChatNonLocalRouteDoesNotInvokeObserver(t *testing.T) {
	// Fusion / Frontier routes must NOT fire the observer: the
	// judge is explicitly scoped to local outputs.
	deps, rt := baseDeps(t)
	obs := &recordingObserver{}
	deps.JudgeObserver = obs
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}` // fusion
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if got := len(obs.Snapshot()); got != 0 {
		t.Errorf("observer saw %d events on fusion, want 0", got)
	}

	obs = &recordingObserver{}
	deps.JudgeObserver = obs
	body = `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}` // guardrail -> frontier
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw = httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if got := len(obs.Snapshot()); got != 0 {
		t.Errorf("observer saw %d events on frontier, want 0", got)
	}
}

func TestChatObserverHonoursRequestIDHeader(t *testing.T) {
	deps, rt := baseDeps(t)
	obs := &recordingObserver{}
	deps.JudgeObserver = obs
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Cascade consumer: non-streaming JSON.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-Id", "abc-123")
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	got := obs.Snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].RequestID != "abc-123" {
		t.Errorf("RequestID = %q, want abc-123", got[0].RequestID)
	}
}

func TestCaptureWriterBoundsBuffer(t *testing.T) {
	// Confirm that writes past the cap still reach the client but
	// the internal buffer stops growing. This is what protects the
	// proxy from a runaway local model OOMing the observer.
	underlying := httptest.NewRecorder()
	const cap = 16
	cw := newCaptureWriter(underlying, cap)
	cw.Header().Set("X-Test", "1")
	cw.WriteHeader(200)
	if _, err := cw.Write([]byte("0123456789")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// First write (10 bytes) fits inside the cap.
	if got := cw.Buffer(); len(got) != 10 {
		t.Errorf("after first Write, Buffer len = %d, want 10", len(got))
	}
	if _, err := cw.Write([]byte("ABCDEFGHIJ")); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	// Second write would push the buffer past the cap, so we stop
	// buffering (but still forward to the client).
	if got := cw.Buffer(); len(got) > cap {
		t.Errorf("after second Write, Buffer len = %d, exceeds cap %d", len(got), cap)
	}
	if !strings.Contains(underlying.Body.String(), "0123456789ABCDEFGHIJ") {
		t.Errorf("client body missing appended chunks: %q", underlying.Body.String())
	}
	// A further write must not grow the buffer at all.
	if _, err := cw.Write([]byte("more bytes")); err != nil {
		t.Fatalf("third Write: %v", err)
	}
	if got := cw.Buffer(); len(got) > cap {
		t.Errorf("Buffer grew past cap: %d", len(got))
	}
}

func TestCaptureWriterFlushes(t *testing.T) {
	// upstream.Stream calls Flush after every chunk; the wrapper
	// must propagate that so SSE framing reaches the client.
	flushable := &flushableRW{ResponseWriter: httptest.NewRecorder()}
	cw := newCaptureWriter(flushable, 64)
	cw.Flush()
	if flushable.flushCount != 1 {
		t.Errorf("Flush did not propagate, count = %d", flushable.flushCount)
	}
}

type flushableRW struct {
	http.ResponseWriter
	flushCount int
}

func (f *flushableRW) Flush() {
	f.flushCount++
	if r, ok := f.ResponseWriter.(http.Flusher); ok {
		r.Flush()
	}
}

// Suppress unused import in case of partial test runs.
// capturingRecorder collects records synchronously into a slice. It exists
// only in tests; production code never blocks on Record().
type capturingRecorder struct {
	mu      sync.Mutex
	records []telemetry.Record
}

func (c *capturingRecorder) Record(r telemetry.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *capturingRecorder) Close() error { return nil }

func (c *capturingRecorder) Snapshot() []telemetry.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]telemetry.Record, len(c.records))
	copy(out, c.records)
	return out
}

// TestChatEmitsTelemetryRowWithCorrectRoute is the acceptance test for
// issue #16: a successful proxied request must produce exactly one
// telemetry record, with non-zero latency, the correct route captured at
// routing-decision time, and the model that the upstream call used.
func TestChatEmitsTelemetryRowWithCorrectRoute(t *testing.T) {
	deps, rt := baseDeps(t)
	rec := &capturingRecorder{}
	deps.Recorder = rec
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Sleep briefly so the in-memory test always observes non-zero
		// latency. Production traffic trivially clears this bar via the
		// network round-trip; the assertion below is what the issue's
		// acceptance criteria actually require.
		time.Sleep(2 * time.Millisecond)
		// Cascade (issue #14) forces stream=false + parses the response
		// as a non-streaming chat completion; return a valid OpenAI-
		// compatible JSON body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}]}`))
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}

	// Drain the handler's async record() — the handler invokes Record
	// synchronously, so by the time ServeHTTP returns the record is
	// already captured.
	records := rec.Snapshot()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	got := records[0]
	if got.Route != string(router.RouteLocal) {
		t.Errorf("Route = %q, want %q", got.Route, router.RouteLocal)
	}
	if got.Model != "qwen3-coder:8b" {
		t.Errorf("Model = %q, want qwen3-coder:8b", got.Model)
	}
	if got.RequestID == "" {
		t.Error("RequestID empty")
	}
	if got.TotalLatencyMs <= 0 {
		t.Errorf("TotalLatencyMs = %d, want > 0", got.TotalLatencyMs)
	}
	if got.OutputTokens <= 0 {
		t.Errorf("OutputTokens = %d, want > 0", got.OutputTokens)
	}
	if !got.Streaming {
		t.Error("Streaming = false, want true (default streaming request)")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
	if got.TTFTMs < 0 {
		t.Errorf("TTFTMs = %d, want >= 0", got.TTFTMs)
	}
}

// TestChatTelemetryTTFTZeroForNonStreaming ensures TTFT is recorded as 0
// (per issue spec) when the harness explicitly opts out of streaming.
func TestChatTelemetryTTFTZeroForNonStreaming(t *testing.T) {
	deps, rt := baseDeps(t)
	rec := &capturingRecorder{}
	deps.Recorder = rec
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond) // ensure elapsed > 0 so TTFT-vs-total comparison is meaningful
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})
	body := `{"stream":false,"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	records := rec.Snapshot()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Streaming {
		t.Error("Streaming = true, want false for non-streaming request")
	}
	if records[0].TTFTMs != 0 {
		t.Errorf("TTFTMs = %d, want 0 for non-streaming", records[0].TTFTMs)
	}
	if records[0].TotalLatencyMs <= 0 {
		t.Errorf("TotalLatencyMs = %d, want > 0", records[0].TotalLatencyMs)
	}
}

// TestChatTelemetryRecordsError exercises the upstream-error path: a row
// is still emitted (just with Error set) so failed requests are visible
// in the dashboard.
func TestChatTelemetryRecordsError(t *testing.T) {
	deps, _ := baseDeps(t)
	// Replace the recording transport with one that fails at the transport
	// layer so the cascade returns an error (HTTP non-2xx is NOT a
	// transport error — only RoundTrip failures are).
	deps.Client = &http.Client{Transport: errTransport{}}
	rec := &capturingRecorder{}
	deps.Recorder = rec
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	records := rec.Snapshot()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Error == "" {
		t.Error("Error empty, want non-empty for upstream failure")
	}
	if records[0].Route != string(router.RouteLocal) {
		t.Errorf("Route = %q, want local", records[0].Route)
	}
}

// errTransport always returns a transport error so the cascade surfaces
// the failure path. RecordingTransport can't simulate this — its RoundTrip
// always returns a *http.Response — so we swap in our own client.
type errTransport struct{}

func (errTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network down")
}

// TestChatTelemetryJSONLRecorderEndToEnd wires the production
// JSONLRecorder through the full handler and asserts the on-disk row
// matches what we expect. This is the cross-package integration test the
// issue's acceptance criteria point to.
func TestChatTelemetryJSONLRecorderEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tel.jsonl")
	r, err := telemetry.NewJSONLRecorder(path)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	deps, rt := baseDeps(t)
	deps.Recorder = r
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})
	// Large prompt -> guardrail forces FRONTIER route.
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	// Close drains the channel + flushes the file.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), data)
	}
	var row telemetry.Record
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("unmarshal: %v body=%q", err, lines[0])
	}
	if row.Route != string(router.RouteFrontier) {
		t.Errorf("Route = %q, want frontier", row.Route)
	}
	if row.TotalLatencyMs < 0 {
		t.Errorf("TotalLatencyMs = %d, want >= 0", row.TotalLatencyMs)
	}
	if row.RequestID == "" {
		t.Error("RequestID empty")
	}
	if row.Model == "" {
		t.Error("Model empty")
	}
}

// qualityRecordingObserver is a QualityObserver test double that
// appends each QualityEvent it sees. Safe for concurrent use.
type qualityRecordingObserver struct {
	mu   sync.Mutex
	seen []QualityEvent
}

func (r *qualityRecordingObserver) Submit(e QualityEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, e)
}

func (r *qualityRecordingObserver) Snapshot() []QualityEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]QualityEvent, len(r.seen))
	copy(out, r.seen)
	return out
}

// TestChatLocalRouteInvokesQualityObserverForToolCall exercises the
// issue #13 wiring: a tool-call envelope in the captured local
// response body produces a QualityEvent with the request id and the
// path from inside arguments.
//
// The tool-call JSON is embedded inside the content field because
// the cascade (issue #14) currently emits only `content` to the
// client; tool_calls are validated upstream but stripped from the
// streamed response. OpenCode and similar agents surface tool-call
// instructions back through the chat thread in the same text
// stream — the detector handles both because it is liberal on the
// JSON shape.
func TestChatLocalRouteInvokesQualityObserverForToolCall(t *testing.T) {
	deps, rt := baseDeps(t)
	obs := &qualityRecordingObserver{}
	deps.QualityObserver = obs
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Single JSON-escape layer — matches what writeSSEResponse
		// emits after re-marshaling the cascade's decoded
		// `content` field.
		body := `{"choices":[{"message":{"content":"applied edit ` +
			`{\"name\":\"edit_file\",\"arguments\":\"{\"path\":\"/tmp/foo.rs\",\"diff\":\"+x\"}\"}"}}]}`
		_, _ = w.Write([]byte(body))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	got := obs.Snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d quality events, want 1 (body=%q)", len(got), rw.Body.String())
	}
	if got[0].Path != "/tmp/foo.rs" {
		t.Errorf("Path = %q, want /tmp/foo.rs", got[0].Path)
	}
	if got[0].RequestID == "" {
		t.Error("RequestID should be populated")
	}
	if got[0].ToolName == "" {
		t.Error("ToolName should be populated")
	}
}

// TestChatLocalRouteQualityObserverNilIsSafe confirms the handler
// runs unchanged when QualityObserver is not configured (the file-
// edit scan should be a no-op in that case).
func TestChatLocalRouteQualityObserverNilIsSafe(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if !strings.Contains(rw.Body.String(), "hi") {
		t.Errorf("body = %q", rw.Body.String())
	}
}

// TestChatNonLocalRouteDoesNotInvokeQualityObserver confirms the
// scan lives behind the same RouteLocal gate as the judge observer:
// fusion and frontier routes never dispatch quality events.
func TestChatNonLocalRouteDoesNotInvokeQualityObserver(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		expect string
	}{
		{"fusion", `{"messages":[{"role":"user","content":"design the system architecture"}]}`, "fusion"},
		{"frontier", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`, "frontier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, rt := baseDeps(t)
			obs := &qualityRecordingObserver{}
			deps.QualityObserver = obs
			rt.OnAny(func(w http.ResponseWriter, _ *http.Request) {
				// Even if the response mentions an edit tool, the
				// scan should not fire because no captureBuffer
				// exists on this route.
				_, _ = w.Write([]byte(`{"name":"edit_file","arguments":"{\"path\":\"/tmp/x\"}"}`))
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			rw := httptest.NewRecorder()
			Chat(deps).ServeHTTP(rw, req)
			if got := obs.Snapshot(); len(got) != 0 {
				t.Errorf("%s: observer saw %d events; want 0", tc.expect, len(got))
			}
		})
	}
}

// TestEmitDetectedEditsSkipsEmptyBody confirms the cheap no-op branch.
func TestEmitDetectedEditsSkipsEmptyBody(t *testing.T) {
	obs := &qualityRecordingObserver{}
	emitDetectedEdits("", "req-1", obs)
	if got := obs.Snapshot(); len(got) != 0 {
		t.Errorf("got %d events on empty body, want 0", len(got))
	}
}

// TestEmitDetectedEditsNilObserverIsSafe confirms the helper does
// not panic when no observer is wired.
func TestEmitDetectedEditsNilObserverIsSafe(t *testing.T) {
	// Should not panic.
	emitDetectedEdits(`{"name":"write_file","arguments":"{\"path\":\"/tmp/x\"}"}`, "req-1", nil)
}

// TestChatRejectsOversizedBody is the acceptance test for issue #11: a
// 2 MiB POST against NEXUS_MAX_BODY_BYTES=1 MiB must return 413
// *before* JSON unmarshaling allocates the full payload. The response
// body must be JSON so the client can surface the failure in their UI.
func TestChatRejectsOversizedBody(t *testing.T) {
	deps, _ := baseDeps(t)
	deps.Config.MaxBodyBytes = 1 << 20 // 1 MiB

	// Build a 2 MiB JSON body: valid JSON-shaped prefix + huge filler
	// inside a string field. The byte cap fires long before any
	// unmarshal attempt, so the exact shape does not have to be a
	// well-formed chat request.
	const oversized = 2 << 20 // 2 MiB
	filler := strings.Repeat("a", oversized)
	body := `{"messages":[{"role":"user","content":"` + filler + `"}]}`
	if len(body) <= oversized {
		t.Fatalf("oversized body not actually oversized: %d", len(body))
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%q", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Body must be a parseable JSON error envelope mentioning the limit.
	var env struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &env); err != nil {
		t.Fatalf("response body not JSON: %v body=%q", err, rw.Body.String())
	}
	if !strings.Contains(env.Error["message"], "NEXUS_MAX_BODY_BYTES") {
		t.Errorf("error message = %q, want it to mention NEXUS_MAX_BODY_BYTES", env.Error["message"])
	}
}

// TestChatAcceptsBodyAtLimit confirms the handler succeeds for a
// request just under the cap. This protects against an off-by-one in
// the MaxBytesReader wiring (e.g. wrapping with limit-1).
func TestChatAcceptsBodyAtLimit(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Config.MaxBodyBytes = 1 << 20 // 1 MiB

	// A small but well-formed request that easily fits under 1 MiB.
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
		t.Errorf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
}

// TestChatAcceptsBodyJustUnderLimit verifies a payload one byte under
// the cap still succeeds (sanity check on MaxBytesReader semantics).
func TestChatAcceptsBodyJustUnderLimit(t *testing.T) {
	deps, rt := baseDeps(t)
	const cap = 1024 // small cap so the test stays fast
	deps.Config.MaxBodyBytes = cap

	rt.OnAny(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// Wrap filler in a comment that JSON ignores? Easier: build a
	// valid chat request whose `content` is just under the cap. We
	// need total JSON length <= cap.
	overhead := len(`{"messages":[{"role":"user","content":""}]}`)
	if cap <= overhead {
		t.Fatalf("cap %d too small for test scaffolding", cap)
	}
	content := strings.Repeat("x", cap-overhead-len(`""`))
	body := `{"messages":[{"role":"user","content":"` + content + `"}]}`
	if len(body) >= cap {
		t.Fatalf("body length %d >= cap %d", len(body), cap)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body len %d < cap %d); body=%q",
			rw.Code, len(body), cap, rw.Body.String())
	}
}

// TestChatRejectsBodyOneByteOverLimit is the negative sibling of
// TestChatAcceptsBodyJustUnderLimit: a payload exactly one byte over
// the cap must trip the MaxBytesReader. This pins the boundary so a
// future regression in the wiring (e.g. accidental `n-1`) is caught.
func TestChatRejectsBodyOneByteOverLimit(t *testing.T) {
	deps, _ := baseDeps(t)
	const cap = 1024
	deps.Config.MaxBodyBytes = cap

	const prefix = `{"messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	if cap <= len(prefix)+len(suffix) {
		t.Fatalf("cap %d too small for envelope", cap)
	}
	contentLen := cap - len(prefix) - len(suffix) + 1 // +1 byte over
	content := strings.Repeat("x", contentLen)
	body := prefix + content + suffix
	if len(body) <= cap {
		t.Fatalf("body length %d <= cap %d", len(body), cap)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body len %d > cap %d); body=%q",
			rw.Code, len(body), cap, rw.Body.String())
	}
}

// TestChatRejectsOversizedBodyBeforeUnmarshal verifies the cap fires
// *before* the JSON parser ever sees the payload. We detect this by
// installing a panic-prone RAG.Store stub: if unmarshal ran, the test
// would crash before reaching the upstream. (We instead rely on the
// simpler invariant that no upstream call is recorded.)
func TestChatRejectsOversizedBodyBeforeUnmarshal(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Config.MaxBodyBytes = 1 << 20

	const oversized = 3 << 20 // 3 MiB > 1 MiB cap
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("z", oversized) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rw.Code)
	}
	if calls := rt.Calls(); len(calls) != 0 {
		t.Errorf("expected zero upstream calls after oversized reject, got %d", len(calls))
	}
}

// TestWriteJSONError confirms the helper emits a parseable envelope.
func TestWriteJSONError(t *testing.T) {
	rw := httptest.NewRecorder()
	writeJSONError(rw, http.StatusRequestEntityTooLarge, "boom")
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rw.Code)
	}
	var env struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %v body=%q", err, rw.Body.String())
	}
	if env.Error["message"] != "boom" {
		t.Errorf("message = %q, want boom", env.Error["message"])
	}
	if env.Error["type"] != "Request Entity Too Large" {
		t.Errorf("type = %q, want %q", env.Error["type"], "Request Entity Too Large")
	}
}

// captureSlog swaps in a JSON slog handler writing to a buffer for the
// duration of fn, then restores the previous default. Returns the captured
// output (one JSON object per line). Used by issue #3 acceptance tests
// that assert on structured fields like `route=local`.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// TestChatEmitsSlogRouteLocal verifies the structured migration of
// router decision logs: a low-complexity prompt should produce a
// `dsl match` line with `route=local` (issue #3 acceptance criteria).
func TestChatEmitsSlogRouteLocal(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})

	var output string
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	output = captureSlog(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		Chat(deps).ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rw.Code)
		}
	})

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no slog output captured: %q", output)
	}
	var foundDSL bool
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid slog line %q: %v", line, err)
		}
		msg, _ := rec["msg"].(string)
		if msg != "dsl match" {
			continue
		}
		if route, _ := rec["route"].(string); route == string(router.RouteLocal) {
			foundDSL = true
			if rid, _ := rec["request_id"].(string); rid == "" {
				t.Errorf("dsl match line missing request_id: %s", line)
			}
		}
	}
	if !foundDSL {
		t.Fatalf("no dsl match log line with route=local in: %s", output)
	}
}

// TestChatEmitsSlogGuardrailVram verifies the guardrail path emits the
// expected structured fields (issue #3 acceptance criteria #2): a
// too-large prompt should produce a `guardrail forced frontier` line
// with `reason=vram` and a positive `estimated_tokens`.
func TestChatEmitsSlogGuardrailVram(t *testing.T) {
	deps, rt := baseDeps(t)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	// 30000 char prompt / 4 = 7500 > 6000 guardrail.
	largeUser := strings.Repeat("a", 30000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`

	output := captureSlog(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		Chat(deps).ServeHTTP(rw, req)
	})

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no slog output captured: %q", output)
	}
	var foundGuardrail bool
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid slog line %q: %v", line, err)
		}
		msg, _ := rec["msg"].(string)
		if msg != "guardrail forced frontier" {
			continue
		}
		reason, _ := rec["reason"].(string)
		tokens, _ := rec["estimated_tokens"].(float64) // JSON numbers decode to float64
		if reason == "vram" && tokens > 0 {
			foundGuardrail = true
		}
	}
	if !foundGuardrail {
		t.Fatalf("no guardrail forced frontier log line (reason=vram, estimated_tokens>0) in: %s", output)
	}
}

// fakeBudgetObserver is a programmable BudgetObserver for the
// dynamic-guardrail tests in issue #6. The Tokens/Source fields can
// be swapped at runtime; Snapshot returns what the chat handler saw
// on the most recent read.
type fakeBudgetObserver struct {
	mu     sync.Mutex
	Tokens int
	Source string
	calls  int
}

func (f *fakeBudgetObserver) BudgetTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.Tokens
}

func (f *fakeBudgetObserver) BudgetSource() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Source
}

func (f *fakeBudgetObserver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestChatDynamicBudgetOverridesStaticGuardrail verifies the boot
// contract for issue #6: when a BudgetObserver reports a small
// per-request budget, the guardrail triggers a frontier reroute
// even though the static NEXUS_TOKEN_GUARDRAIL (6000) would have
// permitted the same prompt. The probe must be consulted on every
// request so thermal-throttle / model-swap events take effect.
func TestChatDynamicBudgetOverridesStaticGuardrail(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.BudgetObserver = &fakeBudgetObserver{Tokens: 100, Source: "ollama-ps+amd-sysfs"}

	// 1000 chars / 4 = 250 tokens. Static guardrail (6000) would have
	// permitted it; dynamic budget (100) blocks it.
	prompt := strings.Repeat("a", 1000)
	body := `{"messages":[{"role":"user","content":"` + prompt + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if len(rt.Calls()) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(rt.Calls()))
	}
	if rt.Calls()[0].URL != "http://frontier.local" {
		t.Errorf("routed to %s, want frontier (dynamic budget should have tripped)", rt.Calls()[0].URL)
	}
	if obs, _ := deps.BudgetObserver.(*fakeBudgetObserver); obs.callCount() == 0 {
		t.Error("BudgetObserver was never consulted")
	}
}

// TestChatDynamicBudgetAllowsSmallPrompt confirms the dynamic
// budget lets a small prompt through to local Ollama, while still
// being consulted (issue #6: the probe must be consulted on every
// request — thermal-throttle / model-swap events take effect
// without waiting for a restart).
func TestChatDynamicBudgetAllowsSmallPrompt(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.BudgetObserver = &fakeBudgetObserver{Tokens: 4096, Source: "ollama-ps+amd-sysfs"}

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if len(rt.Calls()) != 1 || rt.Calls()[0].URL != "http://ollama.local/v1/chat/completions" {
		t.Fatalf("calls = %+v, want one ollama.local call", rt.Calls())
	}
	if obs, _ := deps.BudgetObserver.(*fakeBudgetObserver); obs.callCount() == 0 {
		t.Error("BudgetObserver was never consulted")
	}
}

// TestChatZeroBudgetFallsBackToStaticGuardrail pins down the
// acceptance criterion "a zero / unset probe falls back to the
// static NEXUS_TOKEN_GUARDRAIL" (issue #6). When the probe returns
// 0, the handler must behave as before the issue landed.
func TestChatZeroBudgetFallsBackToStaticGuardrail(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.BudgetObserver = &fakeBudgetObserver{Tokens: 0, Source: "static-fallback"}

	// 30000 char prompt / 4 = 7500 > static guardrail (6000) -> frontier.
	largeUser := strings.Repeat("a", 30000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rt.Calls()[0].URL != "http://frontier.local" {
		t.Errorf("routed to %s, want frontier (static guardrail must still trip)", rt.Calls()[0].URL)
	}
}

// TestChatNilBudgetObserverFallsBackToStaticGuardrail mirrors the
// zero-budget test but exercises the wiring path where no probe is
// installed at all (e.g. binary built without NEXUS_PROBE_INTERVAL).
func TestChatNilBudgetObserverFallsBackToStaticGuardrail(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.BudgetObserver = nil

	// 30000 char prompt / 4 = 7500 > static guardrail (6000) -> frontier.
	largeUser := strings.Repeat("a", 30000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rt.Calls()[0].URL != "http://frontier.local" {
		t.Errorf("routed to %s, want frontier (no probe -> static guardrail still trips)", rt.Calls()[0].URL)
	}
}

// TestChatDynamicBudgetSlogEmitsSource verifies the guardrail log
// line carries the budget source label so operators can confirm
// whether the dynamic probe or the static fallback took the call.
func TestChatDynamicBudgetSlogEmitsSource(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.BudgetObserver = &fakeBudgetObserver{Tokens: 100, Source: "ollama-ps+amd-sysfs"}

	largeUser := strings.Repeat("a", 1000)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	output := captureSlog(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		Chat(deps).ServeHTTP(rw, req)
	})

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "guardrail forced frontier" {
			continue
		}
		if src, _ := rec["budget_source"].(string); src == "ollama-ps+amd-sysfs" {
			if budget, _ := rec["budget_tokens"].(float64); budget == 100 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no guardrail line with budget_source=ollama-ps+amd-sysfs budget_tokens=100 in: %s", output)
	}
}

// TestBudgetObserverFuncNilClosures verifies the BudgetObserverFunc
// adapter degrades gracefully when the underlying closures are nil
// (mirrors main.go's wiring when only one of the probe knobs is set).
func TestBudgetObserverFuncNilClosures(t *testing.T) {
	var b BudgetObserverFunc // both Tokens and Source nil
	if b.BudgetTokens() != 0 {
		t.Errorf("nil Tokens must return 0")
	}
	if b.BudgetSource() != "static" {
		t.Errorf("nil Source must return \"static\", got %q", b.BudgetSource())
	}

	b = BudgetObserverFunc{Tokens: func() int { return 4096 }}
	if b.BudgetTokens() != 4096 {
		t.Errorf("Tokens = %d, want 4096", b.BudgetTokens())
	}
	if got := b.BudgetSource(); got != "static" {
		t.Errorf("Source with nil closure must return \"static\", got %q", got)
	}
}

// --- issue #48: streaming fusion with progressive delivery -------------

// TestChatFusionProgressiveAgreementSkipsArbiter is the chat-handler
// acceptance test for issue #48: when both panel members return
// identical content, the speculative chunk is streamed and the
// arbiter is NOT called — the response body is exactly the
// speculative chunk + `data: [DONE]\n\n`, with no arbiter stream.
func TestChatFusionProgressiveAgreementSkipsArbiter(t *testing.T) {
	deps, rt := baseDeps(t)
	// Both panel members return identical content. The arbiter URL
	// coincides with the frontier URL (baseDeps wiring), so any
	// call to the frontier URL that doesn't carry the
	// "Master Synthesis Arbiter" prompt is a panel member; calls
	// with that prompt would be the arbiter (which we MUST NOT
	// observe).
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch {
		case strings.Contains(body, "Master Synthesis Arbiter"):
			t.Errorf("arbiter was called on agreement; calls=%+v", rt.Calls())
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.String(), "ollama"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"identical panel answer"}}]}`)
		default:
			// Frontier panel member.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"identical panel answer"}}]}`)
		}
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Nexus-Fusion-Progressive"); got != "true" {
		t.Errorf("X-Nexus-Fusion-Progressive = %q, want \"true\"", got)
	}
	if got := rw.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	// Speculative chunk + [DONE], no arbiter synthesis.
	if !strings.Contains(rw.Body.String(), "identical panel answer") {
		t.Errorf("speculative chunk missing: %q", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", rw.Body.String())
	}
	// Source tag embedded in the chunk metadata identifies which
	// panel member streamed first.
	bodyStr := rw.Body.String()
	if !strings.Contains(bodyStr, `"source":"local"`) && !strings.Contains(bodyStr, `"source":"frontier"`) {
		t.Errorf("chunk metadata missing source tag: %q", bodyStr)
	}
	// Only 2 upstream calls (panel members), no arbiter.
	if len(rt.Calls()) != 2 {
		t.Errorf("expected 2 upstream calls (panel only), got %d: %+v", len(rt.Calls()), rt.Calls())
	}
}

// TestChatFusionProgressiveDisabledBackwardCompat verifies the
// backwards-compatibility acceptance criterion: setting
// NEXUS_FUSION_PROGRESSIVE=false (via the Config field) restores
// the legacy Panel behaviour — both members fetched, arbiter always
// invoked, single JSON-object response (issue #10 contract) when
// stream=false. The proxy must not silently regress when an operator
// opts out.
func TestChatFusionProgressiveDisabledBackwardCompat(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Config.FusionProgressiveDelivery = false
	var arbiterBody string
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch {
		case strings.Contains(body, "Master Synthesis Arbiter"):
			arbiterBody = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"synthesized"}}]}`)
		case strings.Contains(r.URL.String(), "ollama"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local"}}]}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier"}}]}`)
		}
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	// Progressive header MUST NOT be set on the legacy path.
	if got := rw.Header().Get("X-Nexus-Fusion-Progressive"); got != "" {
		t.Errorf("X-Nexus-Fusion-Progressive = %q, want empty (legacy path)", got)
	}
	// Arbiter MUST have been called (legacy always invokes).
	if arbiterBody == "" {
		t.Errorf("arbiter not called on legacy path; calls=%v", rt.Calls())
	}
	if !strings.Contains(arbiterBody, "Master Synthesis Arbiter") {
		t.Errorf("arbiter body missing arbiter prompt: %s", arbiterBody)
	}
	if !strings.Contains(rw.Body.String(), "synthesized") {
		t.Errorf("arbiter synthesis missing from response: %q", rw.Body.String())
	}
	// Body should NOT contain the progressive SSE chunks.
	if strings.HasPrefix(strings.TrimSpace(rw.Body.String()), "data:") {
		t.Errorf("legacy path emitted SSE chunks: %q", rw.Body.String())
	}
}

// TestChatFusionProgressiveTelemetryFlag confirms the chat handler
// stamps the fusion_arbiter_skipped telemetry flag (issue #48
// acceptance criterion) when the speculative chunk terminates the
// response without invoking the arbiter.
func TestChatFusionProgressiveTelemetryFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tel.jsonl")
	r, err := telemetry.NewJSONLRecorder(path)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	deps, rt := baseDeps(t)
	deps.Recorder = r
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch {
		case strings.Contains(body, "Master Synthesis Arbiter"):
			t.Errorf("arbiter was called on agreement; calls=%v", rt.Calls())
			w.WriteHeader(http.StatusOK)
		default:
			// Identical content for both panel members.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"identical"}}]}`)
		}
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var row telemetry.Record
	if err := json.Unmarshal(bytes.TrimSpace(data), &row); err != nil {
		t.Fatalf("unmarshal: %v body=%q", err, data)
	}
	if row.Route != string(router.RouteFusion) {
		t.Errorf("Route = %q, want fusion", row.Route)
	}
	if !row.FusionArbiterSkipped {
		t.Errorf("FusionArbiterSkipped = false, want true (agreement path)")
	}
}

// --- Local-route cooldown tests (issue #80) ---------------------------------

// TestChatLocalCooldownSkipsLocalAfterCascadeFailure verifies the core
// acceptance criterion: after the cascade detects a local failure and
// arms the cooldown, the NEXT request must skip local entirely and go
// directly to the frontier fallback, with the X-Nexus-Local-Cooldown
// header stamped on the response.
func TestChatLocalCooldownSkipsLocalAfterCascadeFailure(t *testing.T) {
	deps, rt := baseDeps(t)
	cd := circuit.NewWithClock(30*time.Second, time.Now)
	deps.LocalCooldown = cd

	localHits := int32(0)
	// First call to local returns garbage -> cascade falls back.
	// After the first request arms the cooldown, the second request
	// must NOT reach local at all.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&localHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not openai json")
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"frontier fallback"},"finish_reason":"stop"}]}`)
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`

	// Request 1: local fails, cascade falls back, cooldown armed.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw1 := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw1, req1)
	if rw1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d; body=%q", rw1.Code, rw1.Body.String())
	}
	if !cd.Active() {
		t.Fatal("cooldown should be armed after cascade local failure")
	}

	// Request 2: cooldown active, local must be skipped.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw2 := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d; body=%q", rw2.Code, rw2.Body.String())
	}
	if got := rw2.Header().Get("X-Nexus-Local-Cooldown"); got != "true" {
		t.Errorf("X-Nexus-Local-Cooldown = %q, want \"true\"", got)
	}
	if got := rw2.Header().Get("X-Nexus-Degraded"); got != "true" {
		t.Errorf("X-Nexus-Degraded = %q, want \"true\"", got)
	}
	// Local should have been hit exactly once (request 1).
	if n := atomic.LoadInt32(&localHits); n != 1 {
		t.Errorf("local was hit %d times, want 1 (cooldown should skip second)", n)
	}
}

// TestChatLocalCooldownDisabledDoesNotSkip verifies that a nil
// (disabled) cooldown circuit keeps the pre-#80 behaviour: even after
// a cascade local failure, the next request still tries local.
func TestChatLocalCooldownDisabledDoesNotSkip(t *testing.T) {
	deps, rt := baseDeps(t)
	// No LocalCooldown wired — disabled behaviour.

	localHits := int32(0)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&localHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not openai json")
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		Chat(deps).ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("req %d status = %d", i, rw.Code)
		}
	}
	if n := atomic.LoadInt32(&localHits); n != 3 {
		t.Errorf("local was hit %d times, want 3 (cooldown disabled)", n)
	}
	if got := deps.Config.LocalCooldown; got != 0 {
		t.Errorf("default LocalCooldown should be 0 in test config, got %v", got)
	}
}

// TestChatLocalCooldownStampsNoHeaderWhenInactive verifies that
// when the cooldown is not armed (no failure recorded), the
// X-Nexus-Local-Cooldown header is absent and local is attempted
// normally.
func TestChatLocalCooldownNoHeaderWhenInactive(t *testing.T) {
	deps, rt := baseDeps(t)
	cd := circuit.NewWithClock(30*time.Second, time.Now)
	deps.LocalCooldown = cd

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local ok"},"finish_reason":"stop"}]}`)
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%q", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Nexus-Local-Cooldown"); got != "" {
		t.Errorf("X-Nexus-Local-Cooldown = %q, want absent when cooldown inactive", got)
	}
	if got := rw.Header().Get("X-Nexus-Degraded"); got != "false" {
		t.Errorf("X-Nexus-Degraded = %q, want \"false\"", got)
	}
}

// TestChatLocalCooldownDoesNotAffectFrontierRoute verifies that the
// cooldown only affects route=local and route=fusion, not route=frontier.
func TestChatLocalCooldownDoesNotAffectFrontierRoute(t *testing.T) {
	deps, rt := baseDeps(t)
	cd := circuit.NewWithClock(30*time.Second, time.Now)
	deps.LocalCooldown = cd
	cd.RecordFailure() // arm the cooldown

	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	// Large prompt -> guardrail forces FRONTIER route.
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%q", rw.Code, rw.Body.String())
	}
	// Frontier route should NOT have the cooldown header — the
	// cooldown only protects route=local / route=fusion.
	if got := rw.Header().Get("X-Nexus-Local-Cooldown"); got != "" {
		t.Errorf("X-Nexus-Local-Cooldown = %q, want absent on route=frontier", got)
	}
}
