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
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/ioutils"

	"time"
)

// defaultMaxResponseBytes is the default cap on upstream response bodies.
// It is used by ReadAllLimited to prevent memory exhaustion (issue #365).
const defaultMaxResponseBytes = 64 << 20 // 64 MiB

// ErrCircuitOpen is returned by any embedder's Embed method when the circuit
// breaker has tripped and the cooldown window has not yet elapsed.
// Callers should treat this as a transient failure and retry after the
// cooldown expires.
var ErrCircuitOpen = fmt.Errorf("rag: embedder circuit breaker open")

// embedderCircuitKind identifies which embedder's circuit breaker is open.
// It is used by the chat handler to call the correct observability method.
type embedderCircuitKind string

const (
	circuitKindOllama embedderCircuitKind = "ollama"
	circuitKindOpenAI embedderCircuitKind = "openai"
	circuitKindCohere embedderCircuitKind = "cohere"
)

// circuitError wraps ErrCircuitOpen with the embedder kind so the chat
// handler can call the correct observability method when a circuit trips.
type circuitError struct {
	kind embedderCircuitKind
}

func (e *circuitError) Error() string        { return ErrCircuitOpen.Error() }
func (e *circuitError) Is(target error) bool { return target == ErrCircuitOpen }

// newCircuitError returns an error that is equal to ErrCircuitOpen (so
// errors.Is works) but carries the embedder kind for observability.
func newCircuitError(kind embedderCircuitKind) error {
	return &circuitError{kind: kind}
}

// CircuitKind extracts the embedder circuit kind from an error returned
// by an embedder's Embed method. Returns "" if the error is not an embedder
// circuit error.
func CircuitKind(err error) string {
	if ce, ok := err.(*circuitError); ok {
		return string(ce.kind)
	}
	return ""
}

// BreakerConfig configures the circuit breaker on OllamaEmbedder.
// A zero Threshold disables the breaker.
type BreakerConfig struct {
	Threshold int           // consecutive failures that trip the breaker; 0 = disabled
	Cooldown  time.Duration // how long the breaker stays open after tripping
}

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
	IsHealthy(ctx context.Context) bool
	// IsBreakerOpen is implemented by OllamaEmbedder to expose circuit
	// breaker state. It returns false for embedders that do not have a
	// circuit breaker (issue #304).
	IsBreakerOpen() bool
	// RecordBreakerSuccess resets the OllamaEmbedder failure counter on a
	// successful embedding. No-op for embedders without a breaker.
	RecordBreakerSuccess()
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
			atomic.AddInt64(&c.hits, 1)
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
		atomic.AddInt64(&c.misses, 1)
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
			atomic.AddInt64(&c.hits, 1)
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
		atomic.AddInt64(&c.misses, 1)
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

func (c *EmbedCache) IsHealthy(ctx context.Context) bool {
	return c.inner.IsHealthy(ctx)
}

// IsBreakerOpen delegates to the wrapped embedder's circuit breaker state
// (issue #304). Returns false when the inner embedder does not implement
// the method.
func (c *EmbedCache) IsBreakerOpen() bool {
	if e, ok := c.inner.(interface{ IsBreakerOpen() bool }); ok {
		return e.IsBreakerOpen()
	}
	return false
}

// RecordBreakerSuccess delegates to the wrapped embedder's circuit breaker
// success handler (issue #304). No-op when the inner embedder does not
// implement the method.
func (c *EmbedCache) RecordBreakerSuccess() {
	if e, ok := c.inner.(interface{ RecordBreakerSuccess() }); ok {
		e.RecordBreakerSuccess()
	}
}

// RAGStore is the read+seed API the chat handler depends on. PersistentStore
// (issue #46) embeds *Store and implements the same surface; the handler
// is unaffected because both types satisfy this interface.
//
// Retrieve's fourth return value reports the index path the search
// actually used (issue #447); see IndexPath for the label vocabulary.
// Callers that don't need the path can ignore it with `_, _, _, err :=`
// or the blank-identifier shorthand.
type RAGStore interface {
	Retrieve(ctx context.Context, prompt string) (*FewShotExample, float64, IndexPath, error)
	Add(filename, content string, embedding []float64)
	Size() int
	Threshold() float64
	// IsBreakerOpen returns true when the RAG embedder's circuit breaker
	// has tripped and is blocking requests (issue #304).
	IsBreakerOpen() bool
	// RecordBreakerSuccess notifies the embedder's circuit breaker of a
	// successful retrieval so the failure counter is reset (issue #304).
	RecordBreakerSuccess()
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

type StoreStats struct {
	LastIndexAt       time.Time
	RetrievalAttempts uint64
	RetrievalHits     uint64
	RetrievalMisses   uint64
	EmptyStoreMisses  uint64
	ThresholdMisses   uint64
	EmbedErrors       uint64
	CacheHits         uint64
	CacheMisses       uint64
}

// Store holds the indexed few-shot examples.
type Store struct {
	mu          sync.RWMutex
	examples    []FewShotExample
	embedder    Embedder
	threshold   float64
	index       *HNSWIndex
	indexConfig HNSWConfig

	lastIndexAt       int64
	retrievalAttempts uint64
	retrievalHits     uint64
	retrievalMisses   uint64
	emptyStoreMisses  uint64
	thresholdMisses   uint64
	embedErrors       uint64
}

// indexThreshold is the minimum store size before the HNSW index is used.
// Below this threshold, brute-force scan is fast enough and avoids the
// index build cost. Issue #420 measured ~1ms for 50 snippets brute-force
// vs ~0.1ms HNSW — the crossover point is around 50-100 snippets.
const indexThreshold = 50

// IndexPath identifies which retrieval algorithm Retrieve used to find
// the best matching example (issue #447). The value is one of HNSWIndexPath
// or BruteForceIndexPath when Retrieve actually performed a search; it is
// empty (IndexPathNone) for the fast-exit cases (empty store, empty
// prompt, embedder error) where no candidate was ever scored.
//
// Callers surface the value through RAGEvent.IndexPath so the
// observability layer can partition the similarity histogram by index
// path — operators tune the HNSW crossover and diagnose embedding-model
// drift by comparing the two distributions.
type IndexPath string

const (
	// IndexPathNone means no retrieval was attempted (empty store,
	// empty prompt, or embedder error). No similarity score is
	// available for this retrieval.
	IndexPathNone IndexPath = ""

	// IndexPathHNSW means Retrieve used the HNSW approximate
	// nearest-neighbor index (issue #420). Buckets labelled
	// {path="hnsw"} in nexus_rag_similarity_histogram carry
	// these observations.
	IndexPathHNSW IndexPath = "hnsw"

	// IndexPathBruteForce means Retrieve fell back to the linear
	// cosine scan (small store, or HNSW unavailable). Buckets
	// labelled {path="brute_force"} in
	// nexus_rag_similarity_histogram carry these observations.
	IndexPathBruteForce IndexPath = "brute_force"
)

// NewStore constructs an empty store. dir is the on-disk location of the
// snippets; threshold is the cosine similarity floor (0..1) for retrieval.
func NewStore(embedder Embedder, threshold float64) *Store {
	cfg := DefaultHNSWConfig()
	return &Store{
		embedder:    embedder,
		threshold:   threshold,
		index:       NewHNSWIndex(cfg),
		indexConfig: cfg,
	}
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

func (s *Store) Stats() StoreStats {
	attempts := atomic.LoadUint64(&s.retrievalAttempts)
	hits := atomic.LoadUint64(&s.retrievalHits)
	misses := atomic.LoadUint64(&s.retrievalMisses)
	cacheHits, cacheMisses := s.CacheStats()
	stats := StoreStats{
		RetrievalAttempts: attempts,
		RetrievalHits:     hits,
		RetrievalMisses:   misses,
		EmptyStoreMisses:  atomic.LoadUint64(&s.emptyStoreMisses),
		ThresholdMisses:   atomic.LoadUint64(&s.thresholdMisses),
		EmbedErrors:       atomic.LoadUint64(&s.embedErrors),
	}
	if cacheHits > 0 {
		stats.CacheHits = uint64(cacheHits)
	}
	if cacheMisses > 0 {
		stats.CacheMisses = uint64(cacheMisses)
	}
	if timestamp := atomic.LoadInt64(&s.lastIndexAt); timestamp > 0 {
		stats.LastIndexAt = time.Unix(0, timestamp).UTC()
	}
	return stats
}

func (s *Store) markIndexed(at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	atomic.StoreInt64(&s.lastIndexAt, at.UnixNano())
}

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
		s.markIndexed(time.Now().UTC())
		slog.Info("rag indexed", slog.String("filename", f.Name()))
	}
	return nil
}

// Retrieve returns the highest-scoring example whose cosine similarity to the
// prompt embedding meets the configured threshold, or nil if nothing clears
// the bar. An empty store or empty prompt always yields nil.
//
// The fourth return value is the IndexPath actually used for the search
// (issue #447): IndexPathHNSW or IndexPathBruteForce when a search
// happened, IndexPathNone for the fast-exit cases. Callers forward this
// to the RAG observer so the similarity histogram is partitioned by
// index path.
//
// When the store has at least indexThreshold snippets, Retrieve uses the
// HNSW approximate nearest-neighbor index for O(log n) search, then
// re-ranks the top candidates with exact cosine similarity. This delivers
// Recall@5 ≥ 0.95 vs brute-force while cutting p99 latency from ~50ms to
// ~10ms for 10,000 snippets (issue #420).
func (s *Store) Retrieve(ctx context.Context, prompt string) (*FewShotExample, float64, IndexPath, error) {
	atomic.AddUint64(&s.retrievalAttempts, 1)
	s.mu.RLock()
	n := len(s.examples)
	s.mu.RUnlock()
	if n == 0 {
		atomic.AddUint64(&s.retrievalMisses, 1)
		atomic.AddUint64(&s.emptyStoreMisses, 1)
		return nil, 0, IndexPathNone, nil
	}
	if prompt == "" {
		atomic.AddUint64(&s.retrievalMisses, 1)
		atomic.AddUint64(&s.thresholdMisses, 1)
		return nil, 0, IndexPathNone, nil
	}
	promptEmb, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		atomic.AddUint64(&s.retrievalMisses, 1)
		atomic.AddUint64(&s.embedErrors, 1)
		return nil, 0, IndexPathNone, err
	}

	s.mu.RLock()
	useIndex := n >= indexThreshold && s.index != nil && s.index.Size() >= n
	examples := s.examples
	var idx *HNSWIndex
	if useIndex {
		idx = s.index
	}
	s.mu.RUnlock()

	if useIndex && idx != nil {
		// HNSW path: search index for top candidates, then re-rank with exact cosine.
		// efSearch=50 gives good recall@5; we search for top 10 and re-rank.
		candidateIDs := idx.Search(promptEmb, 10)
		s.mu.RLock()
		defer s.mu.RUnlock()
		var best *FewShotExample
		var bestScore float64 = -1
		for _, id := range candidateIDs {
			if id < 0 || id >= len(examples) {
				continue
			}
			score := CosineSimilarity(promptEmb, examples[id].Embedding)
			if score > bestScore {
				bestScore = score
				best = &examples[id]
			}
		}
		if best != nil && bestScore > s.threshold {
			atomic.AddUint64(&s.retrievalHits, 1)
			return best, bestScore, IndexPathHNSW, nil
		}
		atomic.AddUint64(&s.retrievalMisses, 1)
		atomic.AddUint64(&s.thresholdMisses, 1)
		return nil, bestScore, IndexPathHNSW, nil
	}

	// Brute-force path: O(n) scan for small stores or when index unavailable.
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
		atomic.AddUint64(&s.retrievalHits, 1)
		return best, bestScore, IndexPathBruteForce, nil
	}
	atomic.AddUint64(&s.retrievalMisses, 1)
	atomic.AddUint64(&s.thresholdMisses, 1)
	return nil, bestScore, IndexPathBruteForce, nil
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
	id := len(s.examples)
	s.examples = append(s.examples, FewShotExample{
		Filename:  filename,
		Content:   content,
		Embedding: embedding,
	})
	if s.index != nil {
		s.index.Add(id, embedding)
	}
	s.mu.Unlock()
	s.markIndexed(time.Now().UTC())
}

// IsBreakerOpen delegates to the underlying embedder's circuit breaker
// state (issue #304). Returns false when the embedder does not implement
// the method.
func (s *Store) IsBreakerOpen() bool {
	if e, ok := s.embedder.(interface{ IsBreakerOpen() bool }); ok {
		return e.IsBreakerOpen()
	}
	return false
}

// RecordBreakerSuccess delegates to the underlying embedder's circuit
// breaker success handler (issue #304). No-op when the embedder does not
// implement the method.
func (s *Store) RecordBreakerSuccess() {
	if e, ok := s.embedder.(interface{ RecordBreakerSuccess() }); ok {
		e.RecordBreakerSuccess()
	}
}

// replace swaps the entire examples slice atomically. Used by
// PersistentStore.Load to populate from SQLite on boot and by the
// watcher after a deletion; the caller passes the new slice, this
// helper handles locking. The HNSW index is rebuilt after the swap.
func (s *Store) replace(examples []FewShotExample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.examples = examples
	// Rebuild the HNSW index from the new examples slice.
	s.rebuildIndex()
}

// upsertExample inserts or replaces the example keyed by filename
// in the in-memory slice. Caller is responsible for the DB write;
// this only updates the search corpus so Retrieve sees the change.
// Since HNSW doesn't support efficient in-place updates, the index
// is invalidated so the next Retrieve will rebuild it.
func (s *Store) upsertExample(ex FewShotExample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existingIdx := -1
	for i := range s.examples {
		if s.examples[i].Filename == ex.Filename {
			existingIdx = i
			break
		}
	}
	if existingIdx >= 0 {
		s.examples[existingIdx] = ex
	} else {
		s.examples = append(s.examples, ex)
	}
	// Invalidate the index — we rebuild lazily on next Retrieve if needed.
	// Use Size() == 0 as the "invalidated" sentinel since the actual
	// index size is always >= 0 for a built index.
	if s.index != nil {
		// Mark as stale by setting a flag. We use the index's size field
		// trick: set size to a special value that won't match real examples.
		// Actually simpler: just nil out index and let Retrieve rebuild.
		s.index = nil
	}
}

// rebuildIndex reconstructs the HNSW index from the current examples slice.
// Must be called while holding the store lock.
func (s *Store) rebuildIndex() {
	if len(s.examples) < indexThreshold {
		s.index = nil
		return
	}
	s.index = NewHNSWIndex(s.indexConfig)
	for i, ex := range s.examples {
		s.index.Add(i, ex.Embedding)
	}
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
	BaseURL       string // e.g. "http://localhost:11434"
	Model         string // e.g. "nomic-embed-text"
	Client        *http.Client
	BreakerConfig // optional; zero-value means breaker disabled
	breaker       health.Breaker
}

// NewOllamaEmbedder returns an embedder wired to the given Ollama instance.
// Passing a zero BreakerConfig (Threshold==0) disables the circuit breaker.
func NewOllamaEmbedder(baseURL, model string, client *http.Client, cb BreakerConfig) *OllamaEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	e := &OllamaEmbedder{
		BaseURL:       baseURL,
		Model:         model,
		Client:        client,
		BreakerConfig: cb,
		breaker: health.Breaker{
			Threshold: cb.Threshold,
			Cooldown:  cb.Cooldown,
		},
	}
	if cb.Threshold > 0 {
		health.RegisterBreaker("ollama", &e.breaker)
	}
	return e
}

// IsHealthy checks whether the Ollama embedder is reachable by issuing a
// short embed request with the given timeout. It returns true if the request
// succeeds within the timeout, false otherwise. A nil context uses the default
// background timeout of 2 seconds.
func (o *OllamaEmbedder) IsHealthy(ctx context.Context) bool {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	_, err := o.Embed(ctx, "health check")
	return err == nil
}

// Embed fetches the embedding vector for text. If the circuit breaker is
// open it returns ErrCircuitOpen without calling Ollama.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	if o.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindOllama)
	}
	payload, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.BaseURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		o.breaker.RecordFailure()
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutils.ReadAllLimited(resp.Body, defaultMaxResponseBytes)
	if err != nil {
		o.breaker.RecordFailure()
		return nil, err
	}
	if len(body) >= defaultMaxResponseBytes {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed: response body exceeds %d-byte size limit", defaultMaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed: decode: %w", err)
	}
	if len(raw.Embedding) == 0 {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed: empty embedding for model %s", o.Model)
	}
	o.breaker.RecordSuccess()
	return raw.Embedding, nil
}

// OpenAIEmbedder calls the OpenAI /v1/embeddings endpoint. It is safe for
// concurrent use via a shared http.Client.
type OpenAIEmbedder struct {
	BaseURL       string // e.g. "https://api.openai.com/v1"
	Model         string // e.g. "text-embedding-3-small"
	APIKey        string
	Client        *http.Client
	Audience      string // optional OAuth audience for cURL-compatible header
	BreakerConfig        // optional; zero-value means breaker disabled
	breaker       health.Breaker
}

// NewOpenAIEmbedder returns an embedder wired to the OpenAI embeddings endpoint.
// Passing a zero BreakerConfig (Threshold==0) disables the circuit breaker.
func NewOpenAIEmbedder(baseURL, model, apiKey string, client *http.Client, cb BreakerConfig) *OpenAIEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	e := &OpenAIEmbedder{
		BaseURL:       baseURL,
		Model:         model,
		APIKey:        apiKey,
		Client:        client,
		Audience:      "",
		BreakerConfig: cb,
		breaker: health.Breaker{
			Threshold: cb.Threshold,
			Cooldown:  cb.Cooldown,
		},
	}
	if cb.Threshold > 0 {
		health.RegisterBreaker("openai", &e.breaker)
	}
	return e
}

// Embed fetches the embedding vector for text via the OpenAI /v1/embeddings API.
// If the circuit breaker is open, it returns ErrCircuitOpen without calling OpenAI.
func (o *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	if o.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindOpenAI)
	}
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
		o.breaker.RecordFailure()
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutils.ReadAllLimited(resp.Body, defaultMaxResponseBytes)
	if err != nil {
		o.breaker.RecordFailure()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("openai embed %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("openai embed: decode: %w", err)
	}
	if len(raw.Data) == 0 || len(raw.Data[0].Embedding) == 0 {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("openai embed: empty embedding for model %s", o.Model)
	}
	o.breaker.RecordSuccess()
	return raw.Data[0].Embedding, nil
}

func (o *OpenAIEmbedder) IsHealthy(ctx context.Context) bool {
	if o.breaker.IsOpen() {
		return false
	}
	_, err := o.Embed(ctx, "health")
	return err == nil
}

// IsBreakerOpen reports whether the circuit is currently open.
func (o *OpenAIEmbedder) IsBreakerOpen() bool {
	return o.breaker.IsOpen()
}

// RecordBreakerSuccess resets the circuit breaker failure counter.
func (o *OpenAIEmbedder) RecordBreakerSuccess() {
	o.breaker.RecordSuccess()
}

// CohereEmbedder calls the Cohere /v1/embed endpoint. It is safe for
// concurrent use via a shared http.Client.
type CohereEmbedder struct {
	BaseURL       string // e.g. "https://api.cohere.ai/v1"
	Model         string // e.g. "embed-english-v3.0"
	APIKey        string
	Client        *http.Client
	BreakerConfig // optional; zero-value means breaker disabled
	breaker       health.Breaker
}

// NewCohereEmbedder returns an embedder wired to the Cohere embeddings endpoint.
// Passing a zero BreakerConfig (Threshold==0) disables the circuit breaker.
func NewCohereEmbedder(baseURL, model, apiKey string, client *http.Client, cb BreakerConfig) *CohereEmbedder {
	if client == nil {
		client = http.DefaultClient
	}
	e := &CohereEmbedder{
		BaseURL:       baseURL,
		Model:         model,
		APIKey:        apiKey,
		Client:        client,
		BreakerConfig: cb,
		breaker: health.Breaker{
			Threshold: cb.Threshold,
			Cooldown:  cb.Cooldown,
		},
	}
	if cb.Threshold > 0 {
		health.RegisterBreaker("cohere", &e.breaker)
	}
	return e
}

// Embed fetches the embedding vector for text via the Cohere /v1/embed API.
// If the circuit breaker is open, it returns ErrCircuitOpen without calling Cohere.
func (c *CohereEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	if c.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindCohere)
	}
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
		c.breaker.RecordFailure()
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutils.ReadAllLimited(resp.Body, defaultMaxResponseBytes)
	if err != nil {
		c.breaker.RecordFailure()
		return nil, err
	}
	if len(body) >= defaultMaxResponseBytes {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed: response body exceeds %d-byte size limit", defaultMaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed %s: status %d: %s", c.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed: decode: %w", err)
	}
	if len(raw.Embeddings) == 0 || len(raw.Embeddings[0]) == 0 {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed: empty embedding for model %s", c.Model)
	}
	c.breaker.RecordSuccess()
	return raw.Embeddings[0], nil
}

func (c *CohereEmbedder) IsHealthy(ctx context.Context) bool {
	if c.breaker.IsOpen() {
		return false
	}
	_, err := c.Embed(ctx, "health")
	return err == nil
}

// IsBreakerOpen reports whether the circuit is currently open.
func (c *CohereEmbedder) IsBreakerOpen() bool {
	return c.breaker.IsOpen()
}

// RecordBreakerSuccess resets the circuit breaker failure counter.
func (c *CohereEmbedder) RecordBreakerSuccess() {
	c.breaker.RecordSuccess()
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
// The breakerConfig applies to all embedder types.
func NewEmbedder(embedderType EmbedderType, baseURL, model, apiKey string, client *http.Client, breakerConfig BreakerConfig) (Embedder, error) {
	switch embedderType {
	case EmbedderTypeOpenAI:
		if apiKey == "" {
			return nil, fmt.Errorf("rag: NEXUS_EMBEDDER_TYPE=openai requires NEXUS_FRONTIER_API_KEY to be set")
		}
		return NewOpenAIEmbedder(baseURL, model, apiKey, client, breakerConfig), nil
	case EmbedderTypeCohere:
		if apiKey == "" {
			return nil, fmt.Errorf("rag: NEXUS_EMBEDDER_TYPE=cohere requires NEXUS_COHERE_API_KEY to be set")
		}
		return NewCohereEmbedder(baseURL, model, apiKey, client, breakerConfig), nil
	case EmbedderTypeOllama:
		fallthrough
	default:
		return NewOllamaEmbedder(baseURL, model, client, breakerConfig), nil
	}
}

// FailureCount returns the current consecutive-failure counter. Exported
// for the Prometheus gauge provider in main.go.
func (o *OllamaEmbedder) FailureCount() int {
	return int(o.breaker.FailureCount())
}

// RecordBreakerSuccess resets the circuit breaker failure counter. Called
// by the chat handler when a RAG retrieval succeeds so the breaker does
// not remain open after transient failures (issue #304).
func (o *OllamaEmbedder) RecordBreakerSuccess() {
	o.breaker.RecordSuccess()
}

// IsBreakerOpen reports whether the circuit is currently in the open
// (cooldown) state. Exported for tests and operational dashboards.
func (o *OllamaEmbedder) IsBreakerOpen() bool {
	return o.breaker.IsOpen()
}
