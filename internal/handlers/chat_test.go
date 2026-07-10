package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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
		_, _ = w.Write([]byte("local stream"))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
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

// Suppress unused import in case of partial test runs.
var _ = log.Println
