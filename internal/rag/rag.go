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
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

// embedCacheEntry pairs a prompt embedding with its expiry time so TTL-based
// eviction works alongside the LRU order.
type embedCacheEntry struct {
	key    string    // stored so we can evict by key when the list evicts
	vec    []float64 // embedding vector
	expire time.Time // TTL boundary
}

// EmbedCache is a并发-safe LRU cache for prompt embeddings keyed on the
// exact prompt string. It wraps an Embedder and eliminates redundant
// /api/embeddings round-trips for duplicate prompts within a configurable
// TTL window (issue #227).
type EmbedCache struct {
	inner  Embedder
	max    int           // max entries; 0 disables the cache
	ttl    time.Duration // per-entry TTL; 0 means no expiry
	mu     sync.Mutex
	lru    *list.List               // front = most-recently-used
	index  map[string]*list.Element // prompt → linked-list node
	hits   int64
	misses int64
	// hitCount is an atomic counter incremented on every cache hit.
	// Handlers snapshot it before and after Retrieve to estimate
	// per-request cache hits without being disturbed by concurrent requests.
	hitCount int64
	// loading tracks in-flight inner.Embed calls so concurrent goroutines
	// for the same key share a single upstream request.
	loading map[string]chan loadResult
}

// loadResult is the result of an inner.Embed call, shared across waiters.
type loadResult struct {
	vec []float64
	err error
}

// NewEmbedCache returns a cache that wraps inner. max is the LRU capacity
// (entries are evicted oldest-first when full); ttl is the per-entry time-to-live
// (zero = no expiry). Both max and ttl must be > 0 for caching to be active;
// when either is zero the cache is a no-op pass-through to inner.
func NewEmbedCache(inner Embedder, max int, ttl time.Duration) *EmbedCache {
	return &EmbedCache{
		inner:   inner,
		max:     max,
		ttl:     ttl,
		lru:     list.New(),
		index:   make(map[string]*list.Element),
		loading: make(map[string]chan loadResult),
	}
}

// Embed returns the cached embedding for text if present and not expired,
// otherwise calls inner.Embed and caches the result. Thread-safe.
func (c *EmbedCache) Embed(ctx context.Context, text string) ([]float64, error) {
	if c.max <= 0 || c.ttl <= 0 {
		return c.inner.Embed(ctx, text)
	}

	key := text

	// Fast path: check for a live cache entry under the write lock.
	c.mu.Lock()
	if el, ok := c.index[key]; ok {
		ent := el.Value.(embedCacheEntry)
		if time.Now().Before(ent.expire) {
			c.lru.MoveToFront(el)
			c.hits++
			atomic.AddInt64(&c.hitCount, 1)
			vec := ent.vec
			c.mu.Unlock()
			out := make([]float64, len(vec))
			copy(out, vec)
			return out, nil
		}
		// Expired — remove from both structures.
		c.lru.Remove(el)
		delete(c.index, key)
	}

	// Check if another goroutine is already loading this key.
	// If so, release the lock and wait for that goroutine's result.
	if waitCh, ok := c.loading[key]; ok {
		c.mu.Unlock()
		result := <-waitCh
		if result.err != nil {
			return nil, result.err
		}
		// We waited for a concurrent load and got a hit.
		atomic.AddInt64(&c.hits, 1)
		atomic.AddInt64(&c.hitCount, 1)
		out := make([]float64, len(result.vec))
		copy(out, result.vec)
		return out, nil
	}

	// Mark this key as being loaded. Use a buffered channel so we can
	// signal completion without blocking the broadcasting goroutine.
	loadCh := make(chan loadResult, 1)
	c.loading[key] = loadCh
	c.mu.Unlock()

	// Call the underlying embedder while not holding the lock.
	vec, err := c.inner.Embed(ctx, text)

	// Broadcast result to any waiting goroutines and mark load complete.
	c.mu.Lock()
	delete(c.loading, key)
	if err != nil {
		c.misses++
		c.mu.Unlock()
		loadCh <- loadResult{err: err}
		close(loadCh)
		return nil, err
	}

	// Double-check: another goroutine may have inserted a fresh entry
	// while we were loading.
	if el, ok := c.index[key]; ok {
		ent := el.Value.(embedCacheEntry)
		if time.Now().Before(ent.expire) {
			c.lru.MoveToFront(el)
			c.hits++
		} else {
			c.lru.Remove(el)
			delete(c.index, key)
		}
	}

	// Insert unless a concurrent goroutine inserted while we loaded.
	if _, exists := c.index[key]; !exists {
		ent := embedCacheEntry{
			key:    key,
			vec:    vec,
			expire: time.Now().Add(c.ttl),
		}
		el := c.lru.PushFront(ent)
		c.index[key] = el
		c.misses++
		if c.lru.Len() > c.max {
			oldest := c.lru.Back()
			evictKey := oldest.Value.(embedCacheEntry).key
			delete(c.index, evictKey)
			c.lru.Remove(oldest)
		}
	}
	c.mu.Unlock()

	loadCh <- loadResult{vec: vec, err: nil}
	close(loadCh)

	out := make([]float64, len(vec))
	copy(out, vec)
	return out, nil
}

// CacheStats returns the cumulative hit and miss counts since the cache was
// created. Used for observability; not thread-safe with concurrent access.
func (c *EmbedCache) CacheStats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// HitCount returns the current total hit count atomically. Used by the
// chat handler to snapshot before/after a Retrieve call to estimate
// per-request cache hits without being disturbed by concurrent requests.
func (c *EmbedCache) HitCount() int64 {
	return atomic.LoadInt64(&c.hitCount)
}

// RAGStore is the read+seed API the chat handler depends on. PersistentStore
// (issue #46) embeds *Store and implements the same surface; the handler
// is unaffected because both types satisfy this interface.
type RAGStore interface {
	Retrieve(ctx context.Context, prompt string) (*FewShotExample, float64, error)
	Add(filename, content string, embedding []float64)
	Size() int
	Threshold() float64
}

// EmbedCacheStats is the observability surface for the prompt embedding cache.
// It is implemented by *EmbedCache and is also exposed by *Store (where it
// delegates to the wrapped embedder when it is an *EmbedCache).
type EmbedCacheStats interface {
	// CacheStats returns the cumulative hit and miss counts.
	CacheStats() (hits, misses int64)
}

// RAGCacheStatsProvider is implemented by RAGStore when the store wraps a
// caching embedder. The chat handler type-asserts d.RAG to this interface
// to snapshot the embed cache hit count before/after Retrieve and report
// per-request cache hits via MetricsEvent (issue #227).
type RAGCacheStatsProvider interface {
	EmbedHitCount() int64
}

// Store holds the indexed few-shot examples.
type Store struct {
	// mu guards examples for concurrent readers (Retrieve) and writers
	// (IndexDir / Add / PersistentStore.Upsert). Operations that don't
	// touch the slice (Size / Threshold) skip the lock.
	mu        sync.RWMutex
	examples  []FewShotExample
	embedder  Embedder
	threshold float64
}

// NewStore constructs an empty store. dir is the on-disk location of the
// snippets; threshold is the cosine similarity floor (0..1) for retrieval.
func NewStore(embedder Embedder, threshold float64) *Store {
	return &Store{embedder: embedder, threshold: threshold}
}

// Size returns the number of indexed examples. Acquires the RLock
// because the slice header can be mutated concurrently by the
// watcher (issue #46) and by IndexDir during boot.
func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.examples)
}

// Threshold returns the configured similarity floor. Safe to
// call concurrently because threshold is set once at
// construction and never mutated.
func (s *Store) Threshold() float64 { return s.threshold }

// isSymlink reports whether a DirEntry represents a symbolic link.
// os.ReadDir uses Lstat under the hood, so the ModeSymlink bit is
// set on the entry itself — we never follow the link.
func isSymlink(f os.DirEntry) bool {
	return f.Type()&os.ModeSymlink != 0
}

// resolveDir resolves symlinks in dir once so that every file-path
// check later can compare against a canonical prefix. This also
// prevents the parent-symlink variant (e.g. "examples/sub/../../etc/passwd").
func resolveDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("rag: resolve examples dir %q: %w", dir, err)
	}
	return resolved, nil
}

// verifyInsideDir checks that resolvedPath is rooted inside baseDir.
// This catches path-traversal attempts where a symlinked subdirectory
// escapes the intended examples directory.
func verifyInsideDir(baseDir, resolvedPath string) bool {
	return strings.HasPrefix(resolvedPath, baseDir+string(filepath.Separator)) || resolvedPath == baseDir
}

// CacheStats returns the cache hit/miss counts from the wrapped embedder
// when it is an *EmbedCache. Returns zeros when caching is disabled
// or the embedder is not a cache.
func (s *Store) CacheStats() (hits, misses int64) {
	if ec, ok := s.embedder.(*EmbedCache); ok {
		return ec.CacheStats()
	}
	return 0, 0
}

// EmbedHitCount returns the current hit count from the wrapped *EmbedCache,
// or 0 if the embedder is not a caching embedder. Safe to call concurrently.
func (s *Store) EmbedHitCount() int64 {
	if ec, ok := s.embedder.(*EmbedCache); ok {
		return ec.HitCount()
	}
	return 0
}

// IndexDir walks dir, embedding every regular file's contents. It is
// permissive: a missing directory is created (and indexing returns empty),
// per-file read or embed errors are logged and skipped. This matches the
// prototype's behaviour but the errors are now observable instead of silent.
//
// Security: symlinks are skipped (issue #107) to prevent confidentiality
// leaks via injected few-shot examples. The directory path is resolved
// once to canonicalize it, and every file's resolved path is verified
// to remain inside the resolved directory.
func (s *Store) IndexDir(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return fmt.Errorf("rag: create examples dir %q: %w", dir, mkErr)
		}
		slog.Info("rag created examples directory",
			slog.String("dir", dir),
			slog.String("hint", "drop golden code snippets here"),
		)
		return nil
	}

	safeDir, err := resolveDir(dir)
	if err != nil {
		return err
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("rag: read examples dir %q: %w", dir, err)
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if isSymlink(f) {
			slog.Warn("rag: skipping symlink in examples dir (issue #107)",
				slog.String("filename", f.Name()),
				slog.String("dir", dir),
			)
			continue
		}
		path := filepath.Join(dir, f.Name())
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			slog.Error("rag: cannot resolve path, skipping",
				slog.String("filename", f.Name()),
				slog.Any("err", err),
			)
			continue
		}
		if !verifyInsideDir(safeDir, resolved) {
			slog.Warn("rag: skipping file that escapes examples dir (issue #107)",
				slog.String("filename", f.Name()),
				slog.String("resolved", resolved),
				slog.String("base", safeDir),
			)
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			slog.Error("rag read file", slog.String("filename", f.Name()), slog.Any("err", err))
			continue
		}
		emb, err := s.embedder.Embed(ctx, string(content))
		if err != nil {
			slog.Error("rag embed file", slog.String("filename", f.Name()), slog.Any("err", err))
			continue
		}
		s.mu.Lock()
		s.examples = append(s.examples, FewShotExample{
			Filename:  f.Name(),
			Content:   string(content),
			Embedding: emb,
		})
		s.mu.Unlock()
		slog.Info("rag indexed", slog.String("filename", f.Name()))
	}
	return nil
}

// Retrieve returns the highest-scoring example whose cosine similarity to the
// prompt embedding meets the configured threshold, or nil if nothing clears
// the bar. An empty store or empty prompt always yields nil.
func (s *Store) Retrieve(ctx context.Context, prompt string) (*FewShotExample, float64, error) {
	s.mu.RLock()
	n := len(s.examples)
	s.mu.RUnlock()
	if n == 0 || prompt == "" {
		return nil, 0, nil
	}
	promptEmb, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.examples = append(s.examples, FewShotExample{
		Filename:  filename,
		Content:   content,
		Embedding: embedding,
	})
}

// replace swaps the entire examples slice atomically. Used by
// PersistentStore.Load to populate from SQLite on boot and by the
// watcher after a deletion; the caller passes the new slice, this
// helper handles locking.
func (s *Store) replace(examples []FewShotExample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.examples = examples
}

// upsertExample inserts or replaces the example keyed by filename
// in the in-memory slice. Caller is responsible for the DB write;
// this only updates the search corpus so Retrieve sees the change.
func (s *Store) upsertExample(ex FewShotExample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.examples {
		if s.examples[i].Filename == ex.Filename {
			s.examples[i] = ex
			return
		}
	}
	s.examples = append(s.examples, ex)
}

// removeExample drops a single example from the in-memory slice.
// Caller is responsible for the DB write.
func (s *Store) removeExample(filename string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.examples[:0]
	for _, ex := range s.examples {
		if ex.Filename != filename {
			out = append(out, ex)
		}
	}
	s.examples = out
}

// snapshot returns a defensive copy of the examples slice. Used by
// tests and by the file watcher to compare state without holding
// the lock across an Embed call.
func (s *Store) snapshot() []FewShotExample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FewShotExample, len(s.examples))
	copy(out, s.examples)
	return out
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

// OpenAIEmbedder calls the OpenAI /v1/embeddings endpoint. It is safe for
// concurrent use via a shared http.Client.
type OpenAIEmbedder struct {
	BaseURL  string // e.g. "https://api.openai.com/v1"
	Model    string // e.g. "text-embedding-3-small"
	APIKey   string
	Client   *http.Client
	Audience string // optional OAuth audience for cURL-compatible header
}

// NewOpenAIEmbedder returns an embedder wired to the OpenAI embeddings endpoint.
func NewOpenAIEmbedder(baseURL, model, apiKey string, client *http.Client) *OpenAIEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAIEmbedder{BaseURL: baseURL, Model: model, APIKey: apiKey, Client: client}
}

// Embed fetches the embedding vector for text via the OpenAI /v1/embeddings API.
func (o *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": o.Model,
		"input": text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.BaseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	if o.Audience != "" {
		req.Header.Set("ocp-apim-subscription-key", o.Audience)
	}
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
		return nil, fmt.Errorf("openai embed %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("openai embed: decode: %w", err)
	}
	if len(raw.Data) == 0 || len(raw.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("openai embed: empty embedding for model %s", o.Model)
	}
	return raw.Data[0].Embedding, nil
}

// CohereEmbedder calls the Cohere /v1/embed endpoint. It is safe for
// concurrent use via a shared http.Client.
type CohereEmbedder struct {
	BaseURL string // e.g. "https://api.cohere.ai/v1"
	Model   string // e.g. "embed-english-v3.0"
	APIKey  string
	Client  *http.Client
}

// NewCohereEmbedder returns an embedder wired to the Cohere embeddings endpoint.
func NewCohereEmbedder(baseURL, model, apiKey string, client *http.Client) *CohereEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	return &CohereEmbedder{BaseURL: baseURL, Model: model, APIKey: apiKey, Client: client}
}

// Embed fetches the embedding vector for text via the Cohere /v1/embed API.
func (c *CohereEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": c.Model,
		"texts": []string{text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cohere embed %s: status %d: %s", c.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("cohere embed: decode: %w", err)
	}
	if len(raw.Embeddings) == 0 || len(raw.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("cohere embed: empty embedding for model %s", c.Model)
	}
	return raw.Embeddings[0], nil
}

// EmbedderType is the discriminator for the embedder factory.
type EmbedderType string

const (
	EmbedderTypeOllama EmbedderType = "ollama"
	EmbedderTypeOpenAI EmbedderType = "openai"
	EmbedderTypeCohere EmbedderType = "cohere"
)

// NewEmbedder constructs an Embedder from the given config fields.
// The returned Embedder must be wrapped with NewCachedEmbedder by the
// caller when the cache is desired (as in main.go).
func NewEmbedder(embedderType EmbedderType, baseURL, model, apiKey string, client *http.Client) (Embedder, error) {
	switch embedderType {
	case EmbedderTypeOpenAI:
		if apiKey == "" {
			return nil, fmt.Errorf("rag: NEXUS_EMBEDDER_TYPE=openai requires NEXUS_FRONTIER_API_KEY to be set")
		}
		return NewOpenAIEmbedder(baseURL, model, apiKey, client), nil
	case EmbedderTypeCohere:
		if apiKey == "" {
			return nil, fmt.Errorf("rag: NEXUS_EMBEDDER_TYPE=cohere requires NEXUS_COHERE_API_KEY to be set")
		}
		return NewCohereEmbedder(baseURL, model, apiKey, client), nil
	case EmbedderTypeOllama:
		fallthrough
	default:
		return NewOllamaEmbedder(baseURL, model, client), nil
	}
}
