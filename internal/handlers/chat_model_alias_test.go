package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/providers"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// TestChatModelAliasResolvesToProvider verifies that a request with a
// model matching a configured alias is dispatched to the target
// provider with the upstream model name rewritten in the body.
func TestChatModelAliasResolvesToProvider(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Providers = providers.NewProviderRegistry()
	deps.Providers.Register(providers.ProviderConfig{
		NameVal:    "anthropic",
		BaseURLVal: "http://anthropic.local",
		ModelVal:   "claude-3-5-sonnet",
		APIKeyVal:  "sk-ant-test",
	})
	deps.Config.ModelAliases = map[string]string{
		"gpt-4": "anthropic/claude-3-5-sonnet",
	}

	// The request sends model "gpt-4"; the alias should rewrite it
	// and dispatch to the anthropic provider.
	var capturedBody string
	rt.On("POST", "http://anthropic.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`))
	})

	body := `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	// Verify the request went to the anthropic provider, not the default frontier.
	calls := rt.Calls()
	foundAnthropic := false
	for _, c := range calls {
		if c.URL == "http://anthropic.local/v1/chat/completions" {
			foundAnthropic = true
		}
	}
	if !foundAnthropic {
		t.Errorf("no call to anthropic provider; calls: %v", callURLs(calls))
	}
	// Verify the body model was rewritten from "gpt-4" to "claude-3-5-sonnet".
	if !strings.Contains(capturedBody, `"model":"claude-3-5-sonnet"`) {
		t.Errorf("upstream body does not contain rewritten model: %s", capturedBody)
	}
	if strings.Contains(capturedBody, `"model":"gpt-4"`) {
		t.Errorf("upstream body still contains original alias model 'gpt-4': %s", capturedBody)
	}
}

// TestChatModelAliasUnaliasedPassesThrough verifies backward
// compatibility: a model that does not match any alias passes through
// unchanged.
func TestChatModelAliasUnaliasedPassesThrough(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Config.ModelAliases = map[string]string{
		"gpt-4": "anthropic/claude-3-5-sonnet",
	}
	// No providers registry; the alias should not affect routing.

	// Large prompt forces frontier route.
	largeUser := strings.Repeat("a", 48500)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyStr := string(b)
		// The model should be whatever the client sent (not rewritten).
		if !strings.Contains(bodyStr, `"model":"custom-model"`) {
			t.Errorf("expected model 'custom-model' in body, got: %s", bodyStr)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`))
	})

	body := `{"model":"custom-model","stream":false,"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
}

// TestChatModelAliasStrictRejectsUnknown verifies that strict mode
// returns HTTP 400 for a model that matches no alias and no exact
// provider model.
func TestChatModelAliasStrictRejectsUnknown(t *testing.T) {
	deps, _ := baseDeps(t)
	deps.Providers = providers.NewProviderRegistry()
	deps.Providers.Register(providers.ProviderConfig{
		NameVal:    "openai",
		BaseURLVal: "http://openai.local",
		ModelVal:   "gpt-4o",
	})
	deps.Config.ModelAliases = map[string]string{
		"gpt-4": "openai/gpt-4o",
	}
	deps.Config.ModelAliasesStrict = true

	body := `{"model":"unknown-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rw.Code, rw.Body.String())
	}
}

// TestChatModelAliasStrictAllowsExactProviderModel verifies that
// strict mode passes through a model that exactly matches a
// configured provider model (no alias needed).
func TestChatModelAliasStrictAllowsExactProviderModel(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Providers = providers.NewProviderRegistry()
	deps.Providers.Register(providers.ProviderConfig{
		NameVal:    "openai",
		BaseURLVal: "http://frontier.local",
		ModelVal:   "gpt-4o",
	})
	deps.Config.ModelAliasesStrict = true

	// Large prompt forces frontier route.
	largeUser := strings.Repeat("a", 48500)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`))
	})

	// "gpt-4o" matches a provider model exactly, so strict mode allows it.
	body := `{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
}

// TestChatModelAliasBypassesRouting verifies that when an alias is
// resolved, the request is routed to frontier (the target provider)
// even when the prompt would normally trigger a local route.
func TestChatModelAliasBypassesRouting(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Providers = providers.NewProviderRegistry()
	deps.Providers.Register(providers.ProviderConfig{
		NameVal:    "anthropic",
		BaseURLVal: "http://anthropic.local",
		ModelVal:   "claude-3-5-sonnet",
		APIKeyVal:  "sk-ant-test",
	})
	deps.Config.ModelAliases = map[string]string{
		"gpt-4": "anthropic/claude-3-5-sonnet",
	}

	// "format this css" normally triggers route=local. With an alias,
	// it should bypass to frontier (anthropic provider).
	rt.On("POST", "http://anthropic.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`))
	})
	// Register ollama to verify it's NOT called.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ollama should not be called when alias is resolved")
	})

	body := `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"format this css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	calls := rt.Calls()
	for _, c := range calls {
		if strings.Contains(c.URL, "ollama.local") {
			t.Errorf("ollama was called despite alias resolution: %s", c.URL)
		}
	}
}

func callURLs(calls []upstream.RecordedCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.URL
	}
	return out
}
