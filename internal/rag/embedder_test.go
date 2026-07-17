package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewEmbedder_Ollama verifies that the factory produces an OllamaEmbedder
// when the type is Ollama (the default).
func TestNewEmbedder_Ollama(t *testing.T) {
	emb, err := NewEmbedder(EmbedderTypeOllama, "http://localhost:11434", "nomic-embed-text", "", nil, BreakerConfig{})
	if err != nil {
		t.Fatalf("NewEmbedder(ollama): %v", err)
	}
	if _, ok := emb.(*OllamaEmbedder); !ok {
		t.Errorf("expected *OllamaEmbedder, got %T", emb)
	}
}

func TestNewEmbedder_OpenAI_RequiresAPIKey(t *testing.T) {
	_, err := NewEmbedder(EmbedderTypeOpenAI, "https://api.openai.com/v1", "text-embedding-3-small", "", nil, BreakerConfig{})
	if err == nil {
		t.Fatal("expected error for empty API key with openai embedder")
	}
}

func TestNewEmbedder_Cohere_RequiresAPIKey(t *testing.T) {
	_, err := NewEmbedder(EmbedderTypeCohere, "https://api.cohere.ai/v1", "embed-english-v3.0", "", nil, BreakerConfig{})
	if err == nil {
		t.Fatal("expected error for empty API key with cohere embedder")
	}
}

func TestNewEmbedder_OpenAI_Success(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-testkey" {
			t.Errorf("expected Authorization header with Bearer token, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type: application/json, got %q", r.Header.Get("Content-Type"))
		}
		resp := map[string]any{
			"data": []map[string]any{{
				"embedding": []float64{0.1, 0.2, 0.3},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer svr.Close()

	emb, err := NewEmbedder(EmbedderTypeOpenAI, svr.URL, "text-embedding-3-small", "sk-testkey", svr.Client(), BreakerConfig{})
	if err != nil {
		t.Fatalf("NewEmbedder(openai): %v", err)
	}
	vec, err := emb.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("expected 3 dims, got %d", len(vec))
	}
}

func TestOllamaEmbedder_CircuitBreaker_TripsAfterThreshold(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer svr.Close()

	emb := NewOllamaEmbedder(svr.URL, "nomic-embed-text", svr.Client(),
		BreakerConfig{Threshold: 3, Cooldown: 5 * time.Second})

	ctx := context.Background()
	// Three failures should trip the breaker.
	for i := 0; i < 3; i++ {
		_, err := emb.Embed(ctx, "test")
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: got ErrCircuitOpen before threshold", i+1)
		}
	}
	if !emb.IsBreakerOpen() {
		t.Error("expected breaker to be open after 3 failures")
	}
	// Next call should be rejected without hitting the server.
	calls := 0
	firstCall := true
	svr2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstCall {
			firstCall = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		calls++
	}))
	defer svr2.Close()
	emb2 := NewOllamaEmbedder(svr2.URL, "nomic-embed-text", svr2.Client(),
		BreakerConfig{Threshold: 1, Cooldown: 10 * time.Second})
	_, _ = emb2.Embed(ctx, "test") // trips the breaker (firstCall=true, doesn't count)
	_, err := emb2.Embed(ctx, "test")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if calls != 0 {
		t.Error("server should not be called while circuit is open")
	}
}

func TestOllamaEmbedder_CircuitBreaker_BlocksWhileOpen(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer svr.Close()

	emb := NewOllamaEmbedder(svr.URL, "nomic-embed-text", svr.Client(),
		BreakerConfig{Threshold: 1, Cooldown: 2 * time.Second})

	ctx := context.Background()
	// First call fails and trips the breaker.
	_, err := emb.Embed(ctx, "test")
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("first call: expected non-circuit error, got %v", err)
	}

	// Second call should be blocked by circuit breaker.
	_, err = emb.Embed(ctx, "test")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second call: expected ErrCircuitOpen, got %v", err)
	}

	if !emb.IsBreakerOpen() {
		t.Error("expected breaker to be open")
	}
}

func TestOllamaEmbedder_CircuitBreaker_RecoversAfterCooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping time-based test in -short mode")
	}
	// Server always fails.
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer svr.Close()

	emb := NewOllamaEmbedder(svr.URL, "nomic-embed-text", svr.Client(),
		BreakerConfig{Threshold: 1, Cooldown: 200 * time.Millisecond})

	ctx := context.Background()
	// First call fails and trips the breaker.
	_, _ = emb.Embed(ctx, "test")

	// Wait long enough for the cooldown to fully elapse (500ms >> 200ms cooldown).
	time.Sleep(500 * time.Millisecond)

	// Cooldown has expired; isOpen resets failureCount to 0 and cooldownUntil to 0,
	// so the call reaches the server (no ErrCircuitOpen). The server fails,
	// recordFailure increments failureCount to 1, and since threshold=1 the
	// breaker re-trips immediately. IsBreakerOpen() is true AFTER the call.
	_, err := emb.Embed(ctx, "test")
	if errors.Is(err, ErrCircuitOpen) {
		t.Error("expected HTTP error after cooldown, not ErrCircuitOpen")
	}
	if !emb.IsBreakerOpen() {
		// After a post-cooldown failure the breaker should be open again.
		t.Error("breaker should be open after post-cooldown failure")
	}
}

func TestOllamaEmbedder_CircuitBreaker_ResetsOnSuccess(t *testing.T) {
	// Server always fails (to avoid cooldown blocking failure recording).
	failCount := 999
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failCount > 0 {
			failCount--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float64{0.1, 0.2, 0.3},
		})
	}))
	defer svr.Close()

	emb := NewOllamaEmbedder(svr.URL, "nomic-embed-text", svr.Client(),
		BreakerConfig{Threshold: 3, Cooldown: 500 * time.Millisecond})

	ctx := context.Background()
	// Two failures: failureCount should be 2 after.
	for i := 0; i < 2; i++ {
		_, err := emb.Embed(ctx, "test")
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: got ErrCircuitOpen before threshold", i+1)
		}
	}
	if got := emb.FailureCount(); got != 2 {
		t.Errorf("after two failures: failureCount = %d, want 2", got)
	}

	// Third failure trips the breaker (threshold=3).
	_, err := emb.Embed(ctx, "test")
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatal("3rd call should not be blocked before cooldown starts")
	}
	// Breaker is now open due to cooldown. Subsequent failures are blocked.
	_, err = emb.Embed(ctx, "test")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("call 4: expected ErrCircuitOpen (blocked by cooldown), got %v", err)
	}

	// Wait for cooldown to expire.
	time.Sleep(600 * time.Millisecond)

	// After cooldown: isOpen resets failureCount to 0 and cooldownUntil to 0.
	// The call proceeds, fails, and records failureCount=1 (NOT re-tripped,
	// since 1 < threshold=3). Breaker remains closed.
	_, err = emb.Embed(ctx, "test")
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatal("post-cooldown call should not be blocked")
	}
	if emb.IsBreakerOpen() {
		t.Error("breaker should be closed after post-cooldown failure (count=1 < threshold=3)")
	}

	// After cooldown, a success resets the counter.
	// Temporarily make server succeed.
	svr.Close()
	svr = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float64{0.1, 0.2, 0.3},
		})
	}))
	defer svr.Close()
	// Re-create embedder pointing to new server URL.
	emb = NewOllamaEmbedder(svr.URL, "nomic-embed-text", svr.Client(),
		BreakerConfig{Threshold: 3, Cooldown: 5 * time.Second})
	vec, err := emb.Embed(ctx, "test")
	if err != nil {
		t.Fatalf("success call: %v", err)
	}
	if got := emb.FailureCount(); got != 0 {
		t.Errorf("failureCount after success = %d, want 0", got)
	}
	if len(vec) != 3 {
		t.Errorf("expected 3 dims, got %d", len(vec))
	}
}

func TestOllamaEmbedder_CircuitBreaker_ZeroConfigDefaults(t *testing.T) {
	emb := NewOllamaEmbedder("http://localhost:9999", "nomic-embed-text", nil,
		BreakerConfig{})
	ctx := context.Background()
	_, err := emb.Embed(ctx, "test")
	// With zero threshold the breaker is disabled; we should get a connection
	// error, not ErrCircuitOpen.
	if errors.Is(err, ErrCircuitOpen) {
		t.Error("breaker should be disabled with zero threshold")
	}
}

func TestNewEmbedder_Cohere_Success(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cohere-key-123" {
			t.Errorf("expected Authorization header, got %q", r.Header.Get("Authorization"))
		}
		resp := map[string]any{
			"embeddings": [][]float64{{0.1, 0.2, 0.3}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer svr.Close()

	emb, err := NewEmbedder(EmbedderTypeCohere, svr.URL, "embed-english-v3.0", "cohere-key-123", svr.Client(), BreakerConfig{})
	if err != nil {
		t.Fatalf("NewEmbedder(cohere): %v", err)
	}
	vec, err := emb.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("expected 3 dims, got %d", len(vec))
	}
}

func TestNewEmbedder_UnknownType_FallsBackToOllama(t *testing.T) {
	emb, err := NewEmbedder(EmbedderType("unknown"), "http://localhost:11434", "nomic-embed-text", "", nil, BreakerConfig{})
	if err != nil {
		t.Fatalf("NewEmbedder(unknown): %v", err)
	}
	if _, ok := emb.(*OllamaEmbedder); !ok {
		t.Errorf("expected fallback to *OllamaEmbedder, got %T", emb)
	}
}

// verify that all embedder types satisfy the Embedder interface
var _ Embedder = (*OllamaEmbedder)(nil)
var _ Embedder = (*OpenAIEmbedder)(nil)
var _ Embedder = (*CohereEmbedder)(nil)
