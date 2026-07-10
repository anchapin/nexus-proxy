// Package rag implements the local few-shot retrieval layer.
//
// The store is an in-memory slice of FewShotExample values, each carrying
// its source content and a precomputed embedding vector. Retrieval is a
// brute-force cosine scan: the dataset is expected to be small (developer
// curated snippets), so the constant factor matters more than the algorithm.
//
// All HTTP and filesystem side effects are funnelled through Store so that
// callers can substitute a deterministic Embedder in tests.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
)

// FewShotExample is one indexed code snippet with its embedding.
type FewShotExample struct {
	Filename  string
	Content   string
	Embedding []float64
}

// Embedder turns text into a vector. Implementations must be safe for
// concurrent use; the store calls them concurrently during indexing.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// Store holds the indexed few-shot examples.
type Store struct {
	examples  []FewShotExample
	embedder  Embedder
	threshold float64
}

// NewStore constructs an empty store. dir is the on-disk location of the
// snippets; threshold is the cosine similarity floor (0..1) for retrieval.
func NewStore(embedder Embedder, threshold float64) *Store {
	return &Store{embedder: embedder, threshold: threshold}
}

// Size returns the number of indexed examples.
func (s *Store) Size() int { return len(s.examples) }

// Threshold returns the configured similarity floor.
func (s *Store) Threshold() float64 { return s.threshold }

// IndexDir walks dir, embedding every regular file's contents. It is
// permissive: a missing directory is created (and indexing returns empty),
// per-file read or embed errors are logged and skipped. This matches the
// prototype's behaviour but the errors are now observable instead of silent.
func (s *Store) IndexDir(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return fmt.Errorf("rag: create examples dir %q: %w", dir, mkErr)
		}
		log.Printf("[RAG INDEXER]: Created %s directory. Drop golden code snippets here!", dir)
		return nil
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("rag: read examples dir %q: %w", dir, err)
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		path := filepath.Join(dir, f.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[RAG ERROR]: read %s: %v", f.Name(), err)
			continue
		}
		emb, err := s.embedder.Embed(ctx, string(content))
		if err != nil {
			log.Printf("[RAG ERROR]: embed %s: %v", f.Name(), err)
			continue
		}
		s.examples = append(s.examples, FewShotExample{
			Filename:  f.Name(),
			Content:   string(content),
			Embedding: emb,
		})
		log.Printf("[RAG INDEXER]: Indexed %s successfully.", f.Name())
	}
	return nil
}

// Retrieve returns the highest-scoring example whose cosine similarity to the
// prompt embedding meets the configured threshold, or nil if nothing clears
// the bar. An empty store or empty prompt always yields nil.
func (s *Store) Retrieve(ctx context.Context, prompt string) (*FewShotExample, float64, error) {
	if len(s.examples) == 0 || prompt == "" {
		return nil, 0, nil
	}
	promptEmb, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, 0, err
	}

	var best *FewShotExample
	var bestScore float64 = -1
	for i := range s.examples {
		score := CosineSimilarity(promptEmb, s.examples[i].Embedding)
		if score > bestScore {
			bestScore = score
			best = &s.examples[i]
		}
	}
	if best != nil && bestScore > s.threshold {
		return best, bestScore, nil
	}
	return nil, bestScore, nil
}

// CosineSimilarity returns the cosine of the angle between a and b. A zero
// vector on either side yields 0 (rather than NaN) so callers can sort
// scores without a special case.
func CosineSimilarity(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// FormatInjection returns the standard "[PROXY RETRIEVAL CONTEXT]" block
// appended to a user message when a high-similarity example is found.
func FormatInjection(ex *FewShotExample) string {
	return fmt.Sprintf(
		"\n\n[PROXY RETRIEVAL CONTEXT]: Here is a highly relevant, validated few-shot example from the local codebase (%s):\n```\n%s\n```\nAnalyze its architecture and apply its patterns if relevant to this task.",
		ex.Filename, ex.Content,
	)
}

// Add is a test/seed helper to insert a precomputed example directly into
// the store. Production code uses IndexDir; Add exists so callers (and
// tests) can populate the store without going through the embedding API.
func (s *Store) Add(filename, content string, embedding []float64) {
	s.examples = append(s.examples, FewShotExample{
		Filename:  filename,
		Content:   content,
		Embedding: embedding,
	})
}

// OllamaEmbedder calls the Ollama /api/embeddings endpoint. It is safe for
// concurrent use via a shared http.Client.
type OllamaEmbedder struct {
	BaseURL string // e.g. "http://localhost:11434"
	Model   string // e.g. "nomic-embed-text"
	Client  *http.Client
}

// NewOllamaEmbedder returns an embedder wired to the given Ollama instance.
func NewOllamaEmbedder(baseURL, model string, client *http.Client) *OllamaEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	return &OllamaEmbedder{BaseURL: baseURL, Model: model, Client: client}
}

// Embed fetches the embedding vector for text.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	payload, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.BaseURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("ollama embed: decode: %w", err)
	}
	if len(raw.Embedding) == 0 {
		return nil, fmt.Errorf("ollama embed: empty embedding for model %s", o.Model)
	}
	return raw.Embedding, nil
}

