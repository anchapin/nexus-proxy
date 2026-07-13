package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewEmbedder_Ollama(t *testing.T) {
	emb, err := NewEmbedder(EmbedderTypeOllama, "http://localhost:11434", "nomic-embed-text", "", nil)
	if err != nil {
		t.Fatalf("NewEmbedder(ollama): %v", err)
	}
	if _, ok := emb.(*OllamaEmbedder); !ok {
		t.Errorf("expected *OllamaEmbedder, got %T", emb)
	}
}

func TestNewEmbedder_OpenAI_RequiresAPIKey(t *testing.T) {
	_, err := NewEmbedder(EmbedderTypeOpenAI, "https://api.openai.com/v1", "text-embedding-3-small", "", nil)
	if err == nil {
		t.Fatal("expected error for empty API key with openai embedder")
	}
}

func TestNewEmbedder_Cohere_RequiresAPIKey(t *testing.T) {
	_, err := NewEmbedder(EmbedderTypeCohere, "https://api.cohere.ai/v1", "embed-english-v3.0", "", nil)
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

	emb, err := NewEmbedder(EmbedderTypeOpenAI, svr.URL, "text-embedding-3-small", "sk-testkey", svr.Client())
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

	emb, err := NewEmbedder(EmbedderTypeCohere, svr.URL, "embed-english-v3.0", "cohere-key-123", svr.Client())
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
	emb, err := NewEmbedder(EmbedderType("unknown"), "http://localhost:11434", "nomic-embed-text", "", nil)
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
