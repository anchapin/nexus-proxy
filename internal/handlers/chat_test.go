package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"context"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
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
	// Panel will hit local + frontier + arbiter (3 calls)
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	body := `{"messages":[{"role":"user","content":"design the system architecture"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("status = %d", rw.Code)
	}
	// Expect: 1 local panel call + 1 frontier panel call + 1 arbiter stream
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
var (
	_ = log.Println
	_ = sync.Mutex{}
)
