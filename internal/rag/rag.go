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
	"strconv"
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
	Filename  string // base filename only (no path)
	Dir       string // directory from which this example was indexed
	Content   string
	Embedding []float64
}

// Embedder turns text into a vector. Implementations must be safe for
// concurrent use; the store calls them concurrently during indexing.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float64, error)
	IsHealthy(ctx context.Context) bool
	// IsBreakerOpen is implemented by OllamaEmbedder to expose circuit
	// breaker state. It returns false for embedders that do not have a
	// circuit breaker (issue #304).
	IsBreakerOpen() bool
	// RecordBreakerSuccess resets the OllamaEmbedder failure counter on a
	// successful embedding. No-op for embedders without a breaker.
	RecordBreakerSuccess()
	// SetTripCallback sets a function to be called synchronously when the
	// circuit breaker trips (issue #971). No-op for embedders without a breaker.
	SetTripCallback(kind string, cb func(kind string))
}

// embedCacheEntry pairs a prompt embedding with its expiry time so TTL-based
// eviction works alongside the LRU order.
type embedCacheEntry struct {
	key    string    // stored so we can evict by key when the list evicts
	vec    []float64 // embedding vector
	expire time.Time // TTL boundary
}

// loadingSlot holds the shared channel for an in-flight inner.Embed call
// and a done channel that signals all waiters have given up (timeout or
// ctx cancel). When the done channel is closed, the loading goroutine
// stops trying to broadcast its result and cleans up.
type loadingSlot struct {
	ch   chan loadResult
	done chan struct{}
}

// loadResult is the result of an inner.Embed call, shared across waiters.
type loadResult struct {
	vec []float64
	err error
}

// EmbedCache is a并发-safe LRU cache for prompt embeddings keyed on the
// exact prompt string. It wraps an Embedder and eliminates redundant
// /api/embeddings round-trips for duplicate prompts within a configurable
// TTL window (issue #227).
type EmbedCache struct {
	inner       Embedder
	max         int           // max entries; 0 disables the cache
	ttl         time.Duration // per-entry TTL; 0 means no expiry
	waitTimeout time.Duration // max time a waiter waits for a concurrent load; issue #800
	mu          sync.Mutex
	lru         *list.List               // front = most-recently-used
	index       map[string]*list.Element // prompt → linked-list node
	hits        int64
	misses      int64
	// hitCount is an atomic counter incremented on every cache hit.
	// Handlers snapshot it before and after Retrieve to estimate
	// per-request cache hits without being disturbed by concurrent requests.
	hitCount int64
	// loading tracks in-flight inner.Embed calls so concurrent goroutines
	// for the same key share a single upstream request.
	loading map[string]*loadingSlot
}

// NewEmbedCache returns a cache that wraps inner. max is the LRU capacity
// (entries are evicted oldest-first when full); ttl is the per-entry time-to-live
// (zero = no expiry); waitTimeout is how long a waiter goroutine waits for
// the in-flight inner.Embed to complete before falling through to a direct call.
// Both max and ttl must be > 0 for caching to be active;
// when either is zero the cache is a no-op pass-through to inner.
func NewEmbedCache(inner Embedder, max int, ttl time.Duration, waitTimeout time.Duration) *EmbedCache {
	return &EmbedCache{
		inner:       inner,
		max:         max,
		ttl:         ttl,
		waitTimeout: waitTimeout,
		lru:         list.New(),
		index:       make(map[string]*list.Element),
		loading:     make(map[string]*loadingSlot),
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
	if slot, ok := c.loading[key]; ok {
		c.mu.Unlock()
		// Wait for the result with optional timeout. If waitTimeout is 0,
		// we wait indefinitely (pre-issue-#800 behaviour).
		var result loadResult
		if c.waitTimeout > 0 {
			timer := time.NewTimer(c.waitTimeout)
			select {
			case result = <-slot.ch:
				timer.Stop()
			case <-timer.C:
				// Timeout: delete the loading slot so future waiters don't reuse it,
				// then fall through to call inner directly. We do NOT close done —
				// the inner goroutine may still be running and its result will be
				// broadcast to any other waiters on slot.ch (issue #1043).
				c.mu.Lock()
				delete(c.loading, key)
				c.mu.Unlock()
				return c.inner.Embed(ctx, text)
			case <-ctx.Done():
				timer.Stop()
				c.mu.Lock()
				delete(c.loading, key)
				c.mu.Unlock()
				return nil, ctx.Err()
			}
		} else {
			select {
			case result = <-slot.ch:
			case <-ctx.Done():
				c.mu.Lock()
				delete(c.loading, key)
				c.mu.Unlock()
				return nil, ctx.Err()
			}
		}
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

	// Mark this key as being loaded. Create a slot with a buffered
	// result channel and an unbuffered done channel.
	slot := &loadingSlot{
		ch:   make(chan loadResult, 1),
		done: make(chan struct{}),
	}
	c.loading[key] = slot
	c.mu.Unlock()

	// Call the underlying embedder while not holding the lock.
	vec, err := c.inner.Embed(ctx, text)

	// Broadcast result to any waiting goroutines and mark load complete.
	c.mu.Lock()
	delete(c.loading, key)
	if err != nil {
		atomic.AddInt64(&c.misses, 1)
		c.mu.Unlock()
		select {
		case slot.ch <- loadResult{err: err}:
		case <-slot.done:
			// All waiters gave up; result is orphaned in the buffered
			// channel — that's fine, the sender (us) will exit and
			// the GC will clean up the slot.
		}
		close(slot.ch)
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

	select {
	case slot.ch <- loadResult{vec: vec, err: nil}:
	case <-slot.done:
		// All waiters gave up; result is orphaned in the buffered
		// channel — that's fine.
	}
	close(slot.ch)

	out := make([]float64, len(vec))
	copy(out, vec)
	return out, nil
}

// EmbedBatch returns cached embeddings for texts that are already in the cache,
// then delegates to the inner EmbedBatch for any missing texts and caches those
// results. The returned slice is in the same order as the input texts.
func (c *EmbedCache) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	if c.max <= 0 || c.ttl <= 0 {
		return c.inner.EmbedBatch(ctx, texts)
	}

	// Partition texts into cached and uncached.
	c.mu.Lock()
	var uncachedIdx []int
	result := make([][]float64, len(texts))
	now := time.Now()
	for i, text := range texts {
		if el, ok := c.index[text]; ok {
			ent := el.Value.(embedCacheEntry)
			if now.Before(ent.expire) {
				c.lru.MoveToFront(el)
				vec := ent.vec
				// Defensive copy: caller may modify result[i], which must not corrupt the cache entry.
				result[i] = append(make([]float64, 0, len(vec)), vec...)
				continue
			}
			c.lru.Remove(el)
			delete(c.index, text)
		}
		uncachedIdx = append(uncachedIdx, i)
	}
	c.mu.Unlock()

	if len(uncachedIdx) == 0 {
		atomic.AddInt64(&c.hits, int64(len(texts)))
		atomic.AddInt64(&c.hitCount, int64(len(texts)))
		return result, nil
	}

	// Extract uncached texts.
	uncachedTexts := make([]string, len(uncachedIdx))
	for i, idx := range uncachedIdx {
		uncachedTexts[i] = texts[idx]
	}

	// Call the inner embedder in batch.
	uncachedVecs, err := c.inner.EmbedBatch(ctx, uncachedTexts)
	if err != nil {
		atomic.AddInt64(&c.misses, int64(len(uncachedIdx)))
		return nil, err
	}

	// Populate uncached results and insert into cache.
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, idx := range uncachedIdx {
		vec := uncachedVecs[i]
		result[idx] = vec
		ent := embedCacheEntry{
			key:    texts[idx],
			vec:    vec,
			expire: now.Add(c.ttl),
		}
		el := c.lru.PushFront(ent)
		c.index[texts[idx]] = el
		if c.lru.Len() > c.max {
			oldest := c.lru.Back()
			if oldest != nil {
				evictKey := oldest.Value.(embedCacheEntry).key
				delete(c.index, evictKey)
				c.lru.Remove(oldest)
			}
		}
	}
	atomic.AddInt64(&c.misses, int64(len(uncachedIdx)))

	return result, nil
}

// CacheStats returns the cumulative hit and miss counts since the cache was
// created. Used for observability; safe to call concurrently with
// Embed, EmbedBatch, and Retrieve.
func (c *EmbedCache) CacheStats() (hits, misses int64) {
	return atomic.LoadInt64(&c.hits), atomic.LoadInt64(&c.misses)
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

// SetTripCallback sets a function to be called synchronously when the
// wrapped embedder's circuit breaker trips (issue #971). No-op when the
// inner embedder does not implement the method.
func (c *EmbedCache) SetTripCallback(kind string, cb func(kind string)) {
	if e, ok := c.inner.(interface {
		SetTripCallback(string, func(kind string))
	}); ok {
		e.SetTripCallback(kind, cb)
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
	// ThresholdFor returns the effective threshold for the given directory
	// (issue #671). When no per-directory override is set, returns the
	// global threshold.
	ThresholdFor(dir string) float64
	// IndexMode reports which retrieval path the next Retrieve call
	// will take: "none", "brute_force", or "hnsw" (issue #446).
	IndexMode() string
	// IsBreakerOpen returns true when the RAG embedder's circuit breaker
	// has tripped and is blocking requests (issue #304).
	IsBreakerOpen() bool
	// RecordBreakerSuccess notifies the embedder's circuit breaker of a
	// successful retrieval so the failure counter is reset (issue #304).
	RecordBreakerSuccess()
	// LastSuccessfulKind returns the circuit kind of the last successful
	// embedder (e.g. "ollama", "openai", "cohere"), or "" if no embedder
	// has succeeded yet. Used for RAG circuit breaker observability (issue #886).
	LastSuccessfulKind() string
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

// InjectionSkipRecorder is optionally implemented by RAGStore backends
// (both *Store and *PersistentStore) so the chat handler can bump the
// counter when a RAG injection is aborted by the NEXUS_MAX_BODY_BYTES
// size guard (issue #594). The handler type-asserts d.RAG to this
// interface; a nil-safe no-op happens when the backend does not
// implement it.
type InjectionSkipRecorder interface {
	IncInjectionSkippedSizeLimit()
}

type StoreStats struct {
	LastIndexAt               time.Time
	RetrievalAttempts         uint64
	RetrievalHits             uint64
	RetrievalMisses           uint64
	EmptyStoreMisses          uint64
	ThresholdMisses           uint64
	EmbedErrors               uint64
	CacheHits                 uint64
	CacheMisses               uint64
	InjectionSkippedSizeLimit uint64
	IndexGeneration           int64
}

// Store holds the indexed few-shot examples.
type Store struct {
	mu                 sync.RWMutex
	examples           []FewShotExample
	embedder           Embedder
	threshold          float64
	thresholdOverrides map[string]float64 // dir -> threshold; unspecified dirs use global threshold
	index              *HNSWIndex
	indexConfig        HNSWConfig
	batchSize          int // number of files to embed per batch; 0 disables batching

	lastIndexAt               int64
	retrievalAttempts         uint64
	retrievalHits             uint64
	retrievalMisses           uint64
	emptyStoreMisses          uint64
	thresholdMisses           uint64
	embedErrors               uint64
	injectionSkippedSizeLimit uint64
	generation                int64
	lastSuccessfulKind        string // circuit kind of last successful embedder (issue #886)
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithBatchSize sets the number of files embedded per batch in IndexDir.
// A value of 0 disables batching (each file is embedded individually).
func WithBatchSize(n int) StoreOption {
	return func(s *Store) { s.batchSize = n }
}

// indexThreshold is the minimum store size before the HNSW index is used.
// Below this threshold, brute-force scan is fast enough and avoids the
// index build cost. Issue #420 measured ~1ms for 50 snippets brute-force
// vs ~0.1ms HNSW — the crossover point is around 50-100 snippets.
const indexThreshold = 50

// ParseThresholdOverrides parses NEXUS_RAG_THRESHOLD_<DIR> env vars and
// returns a map of directory name -> threshold. The global NEXUS_RAG_THRESHOLD
// applies to all directories without an explicit override.
//
// Directory names are compared case-insensitively. For example,
// NEXUS_RAG_THRESHOLD_GO_TESTS=0.7 sets threshold 0.7 for the "go_tests"
// directory. This enables per-domain similarity tuning (issue #671).
func ParseThresholdOverrides() map[string]float64 {
	overrides := make(map[string]float64)
	prefix := "NEXUS_RAG_THRESHOLD_"
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, prefix) {
			continue
		}
		idx := strings.Index(env, "=")
		if idx < 0 {
			continue
		}
		rawKey := env[len(prefix):idx]
		if rawKey == "" {
			continue
		}
		key := prefix + strings.ToValidUTF8(rawKey, "")
		val := os.Getenv(key)
		if val == "" {
			continue
		}
		threshold, err := strconv.ParseFloat(val, 64)
		if err != nil || threshold < 0 || threshold > 1 {
			slog.Warn("rag: ignoring invalid threshold override",
				slog.String("dir", rawKey),
				slog.String("value", val),
				slog.Any("err", err),
			)
			continue
		}
		overrides[strings.ToLower(strings.ToValidUTF8(rawKey, ""))] = threshold
	}
	return overrides
}

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

// IndexMode describes which retrieval path Store.Retrieve will use for
// the current example count (issue #446). The /status endpoint reports
// this value so operators can confirm whether the HNSW index is in use.
const (
	// IndexModeNone is reported when the store is empty — Retrieve
	// short-circuits before any search happens.
	IndexModeNone = "none"
	// IndexModeBruteForce is reported when the store has fewer than
	// indexThreshold examples or when the HNSW index has been
	// invalidated by an upsert (issue #420).
	IndexModeBruteForce = "brute_force"
	// IndexModeHNSW is reported when the store is large enough and
	// the HNSW index is fully populated — Retrieve then uses the
	// approximate-neighbour code path.
	IndexModeHNSW = "hnsw"
)

// IndexMode returns the retrieval path the next Retrieve call will
// take: "none" (empty store), "brute_force" (small store or
// invalidated index), or "hnsw" (approximate index active). Race-safe
// — the same lock pattern Retrieve uses to decide the path (issue #446).
func (s *Store) IndexMode() string {
	s.mu.RLock()
	n := len(s.examples)
	indexed := s.index != nil && s.index.Size() >= n
	s.mu.RUnlock()
	switch {
	case n == 0:
		return IndexModeNone
	case n < indexThreshold || !indexed:
		return IndexModeBruteForce
	default:
		return IndexModeHNSW
	}
}

// NewStore constructs an empty store. dir is the on-disk location of the
// snippets; threshold is the cosine similarity floor (0..1) for retrieval.
func NewStore(embedder Embedder, threshold float64, opts ...StoreOption) *Store {
	cfg := DefaultHNSWConfig()
	s := &Store{
		embedder:    embedder,
		threshold:   threshold,
		index:       NewHNSWIndex(cfg),
		indexConfig: cfg,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

// ThresholdFor returns the effective similarity threshold for the given
// directory. It returns the global threshold when no per-directory override
// is configured. The comparison is case-insensitive.
//
// NEXUS_RAG_THRESHOLD is global (issue #671): it applies to all indexed
// files unless a per-directory override is set via
// NEXUS_RAG_THRESHOLD_<DIR>=<value>. For example,
// NEXUS_RAG_THRESHOLD_GO_TESTS=0.7 sets threshold 0.7 for files indexed
// from the "go_tests" directory.
func (s *Store) ThresholdFor(dir string) float64 {
	if s.thresholdOverrides == nil {
		return s.threshold
	}
	// Normalize directory name to match parsing convention
	dirKey := strings.ToLower(filepath.Base(dir))
	if override, ok := s.thresholdOverrides[dirKey]; ok {
		return override
	}
	return s.threshold
}

func (s *Store) Stats() StoreStats {
	attempts := atomic.LoadUint64(&s.retrievalAttempts)
	hits := atomic.LoadUint64(&s.retrievalHits)
	misses := atomic.LoadUint64(&s.retrievalMisses)
	cacheHits, cacheMisses := s.CacheStats()
	stats := StoreStats{
		RetrievalAttempts:         attempts,
		RetrievalHits:             hits,
		RetrievalMisses:           misses,
		EmptyStoreMisses:          atomic.LoadUint64(&s.emptyStoreMisses),
		ThresholdMisses:           atomic.LoadUint64(&s.thresholdMisses),
		EmbedErrors:               atomic.LoadUint64(&s.embedErrors),
		InjectionSkippedSizeLimit: atomic.LoadUint64(&s.injectionSkippedSizeLimit),
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
	stats.IndexGeneration = atomic.LoadInt64(&s.generation)
	return stats
}

func (s *Store) markIndexed(at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	atomic.StoreInt64(&s.lastIndexAt, at.UnixNano())
}

// IncInjectionSkippedSizeLimit bumps the counter for RAG injections that
// were aborted by the NEXUS_MAX_BODY_BYTES size guard (issue #594). Safe
// for concurrent use. Promoted to *PersistentStore via embedding.
func (s *Store) IncInjectionSkippedSizeLimit() {
	atomic.AddUint64(&s.injectionSkippedSizeLimit, 1)
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

	type fileInfo struct {
		name    string
		content string
	}
	var validFiles []fileInfo

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
		validFiles = append(validFiles, fileInfo{name: f.Name(), content: string(content)})
	}

	if s.batchSize > 0 && len(validFiles) > 0 {
		for i := 0; i < len(validFiles); i += s.batchSize {
			end := i + s.batchSize
			if end > len(validFiles) {
				end = len(validFiles)
			}
			batch := validFiles[i:end]
			texts := make([]string, len(batch))
			for j, fi := range batch {
				texts[j] = fi.content
			}
			embs, err := s.embedder.EmbedBatch(ctx, texts)
			if err != nil {
				// Partial batch: entries were appended to s.examples but
				// upsertExample was never called, so the HNSW index is stale.
				// Invalidate it so Retrieve falls back to brute-force.
				s.mu.Lock()
				s.index = nil
				s.mu.Unlock()
				slog.Warn("rag embed batch failed, HNSW index invalidated",
					slog.Any("err", err),
					slog.Int("batchStart", i),
					slog.Int("batchLen", len(batch)),
				)
				continue
			}
			s.mu.Lock()
			for j, fi := range batch {
				s.examples = append(s.examples, FewShotExample{
					Filename:  fi.name,
					Dir:       safeDir,
					Content:   fi.content,
					Embedding: embs[j],
				})
				s.markIndexed(time.Now().UTC())
				slog.Info("rag indexed", slog.String("filename", fi.name))
			}
			s.mu.Unlock()
		}
	} else {
		for _, fi := range validFiles {
			emb, err := s.embedder.Embed(ctx, fi.content)
			if err != nil {
				slog.Error("rag embed file", slog.String("filename", fi.name), slog.Any("err", err))
				continue
			}
			s.mu.Lock()
			s.examples = append(s.examples, FewShotExample{
				Filename:  fi.name,
				Dir:       safeDir,
				Content:   fi.content,
				Embedding: emb,
			})
			s.mu.Unlock()
			s.markIndexed(time.Now().UTC())
			slog.Info("rag indexed", slog.String("filename", fi.name))
		}
	}

	// If EmbedBatch failed mid-way, s.index was invalidated but later batches
	// still appended to s.examples. Rebuild synchronously now so IndexDir
	// returns with a consistent state instead of leaving the index incomplete
	// until the next Retrieve call (issue #976).
	s.maybeRebuildIndex()

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

	s.maybeRebuildIndex()

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
		if best != nil && bestScore > s.ThresholdFor(best.Dir) {
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
	if best != nil && bestScore > s.ThresholdFor(best.Dir) {
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
	// Track the kind of the successful embedder for observability (issue #886).
	// Use a type switch to identify the embedder kind since we don't want
	// to add Kind() to the Embedder interface (would break all test doubles).
	s.lastSuccessfulKind = embedderKind(s.embedder)
}

// SetTripCallback sets a function to be called synchronously when the
// underlying embedder's circuit breaker trips (issue #971). No-op when the
// embedder does not implement the method.
func (s *Store) SetTripCallback(kind string, cb func(kind string)) {
	if e, ok := s.embedder.(interface {
		SetTripCallback(string, func(kind string))
	}); ok {
		e.SetTripCallback(kind, cb)
	}
}

// embedderKind returns the circuit kind string for the given embedder.
// Returns "" for unknown embedder types.
func embedderKind(e Embedder) string {
	switch te := e.(type) {
	case *OllamaEmbedder:
		return "ollama"
	case *OpenAIEmbedder:
		return "openai"
	case *CohereEmbedder:
		return "cohere"
	case *EmbedCache:
		return embedderKind(te.inner)
	default:
		return ""
	}
}

// LastSuccessfulKind returns the circuit kind of the last successful embedder
// (e.g. "ollama", "openai", "cohere"), or "" if no embedder has succeeded yet.
// Used for RAG circuit breaker observability (issue #886).
func (s *Store) LastSuccessfulKind() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSuccessfulKind
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

// restoreIndex sets the HNSW index from a serialized blob produced
// by HNSWIndex.Serialize, replacing any in-progress rebuild. This
// lets Load skip the O(n) reconstruction for large corpora (issue #939).
// Must be called while holding the store lock.
func (s *Store) restoreIndex(blob []byte) error {
	idx, err := DeserializeHNSWIndex(blob, s.indexConfig)
	if err != nil {
		return fmt.Errorf("rag: restore hnsw index: %w", err)
	}
	s.index = idx
	return nil
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
	// Invalidate the index — Retrieve will rebuild lazily on next call.
	if s.index != nil {
		s.index = nil
	}
	atomic.AddInt64(&s.generation, 1)
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

// SerializeIndex serializes the current HNSW index to a blob. If the index
// is nil but the store is large enough to warrant an index, it rebuilds first.
// Must be called while holding the store lock.
func (s *Store) SerializeIndex() ([]byte, error) {
	if s.index == nil && len(s.examples) >= indexThreshold {
		s.rebuildIndex()
	}
	if s.index == nil {
		return nil, nil
	}
	return s.index.Serialize()
}

// maybeRebuildIndex checks whether the HNSW index needs to be rebuilt
// after an upsert/delete invalidated it, and rebuilds it synchronously
// if the store is large enough to warrant indexing. This is the "lazy
// rebuild on next Retrieve" that the upsertExample comment promises but
// Retrieve never fulfilled (issue #829).
//
// The caller must not hold any lock. This function acquires and releases
// the write lock internally.
func (s *Store) maybeRebuildIndex() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index != nil {
		return
	}
	n := len(s.examples)
	if n < indexThreshold {
		return
	}
	s.rebuildIndex()
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
	if s.index != nil {
		s.index = nil
	}
	atomic.AddInt64(&s.generation, 1)
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

// EmbedBatch fetches embedding vectors for multiple texts in a single request.
// If the circuit breaker is open it returns ErrCircuitOpen without calling Ollama.
func (o *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	if o.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindOllama)
	}
	payload, _ := json.Marshal(map[string]any{"model": o.Model, "prompts": texts})
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
		return nil, fmt.Errorf("ollama embed batch: response body exceeds %d-byte size limit", defaultMaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed batch %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed batch: decode: %w", err)
	}
	if len(raw.Embeddings) == 0 {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed batch: empty embeddings for model %s", o.Model)
	}
	if len(raw.Embeddings) != len(texts) {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("ollama embed batch: response has %d embeddings, want %d", len(raw.Embeddings), len(texts))
	}
	for i, emb := range raw.Embeddings {
		if len(emb) == 0 {
			o.breaker.RecordFailure()
			return nil, fmt.Errorf("ollama embed batch: empty embedding at index %d for model %s", i, o.Model)
		}
	}
	for i, emb := range raw.Embeddings {
		if i == 0 {
			continue
		}
		if len(emb) != len(raw.Embeddings[0]) {
			o.breaker.RecordFailure()
			return nil, fmt.Errorf("ollama embed batch: embedding at index %d has dimension %d, want %d", i, len(emb), len(raw.Embeddings[0]))
		}
	}
	o.breaker.RecordSuccess()
	return raw.Embeddings, nil
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

// EmbedBatch fetches embedding vectors for multiple texts via the OpenAI
// /v1/embeddings API. If the circuit breaker is open, it returns ErrCircuitOpen
// without calling OpenAI.
func (o *OpenAIEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	if o.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindOpenAI)
	}
	payload, _ := json.Marshal(map[string]any{
		"model": o.Model,
		"input": texts,
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
		return nil, fmt.Errorf("openai embed batch %s: status %d: %s", o.Model, resp.StatusCode, body)
	}
	var raw struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("openai embed batch: decode: %w", err)
	}
	if len(raw.Data) == 0 {
		o.breaker.RecordFailure()
		return nil, fmt.Errorf("openai embed batch: empty data for model %s", o.Model)
	}
	for i, d := range raw.Data {
		if len(d.Embedding) == 0 {
			o.breaker.RecordFailure()
			return nil, fmt.Errorf("openai embed batch: empty embedding at index %d for model %s", i, o.Model)
		}
	}
	for i, d := range raw.Data {
		if i == 0 {
			continue
		}
		if len(d.Embedding) != len(raw.Data[0].Embedding) {
			o.breaker.RecordFailure()
			return nil, fmt.Errorf("openai embed batch: embedding at index %d has dimension %d, want %d", i, len(d.Embedding), len(raw.Data[0].Embedding))
		}
	}
	o.breaker.RecordSuccess()
	result := make([][]float64, len(raw.Data))
	for i, d := range raw.Data {
		result[i] = d.Embedding
	}
	return result, nil
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

// SetTripCallback sets a function to be called synchronously when the
// circuit breaker trips (issue #971).
func (o *OpenAIEmbedder) SetTripCallback(kind string, cb func(kind string)) {
	o.breaker.SetTripCallback(kind, cb)
}

// Kind returns the circuit kind string for this embedder ("openai").
// Used for RAG circuit breaker observability (issue #886).
func (o *OpenAIEmbedder) Kind() string {
	return "openai"
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

// EmbedBatch fetches embedding vectors for multiple texts via the Cohere
// /v1/embed API. If the circuit breaker is open, it returns ErrCircuitOpen
// without calling Cohere.
func (c *CohereEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	if c.breaker.IsOpen() {
		return nil, newCircuitError(circuitKindCohere)
	}
	payload, _ := json.Marshal(map[string]any{
		"model": c.Model,
		"texts": texts,
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
		return nil, fmt.Errorf("cohere embed batch: response body exceeds %d-byte size limit", defaultMaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed batch %s: status %d: %s", c.Model, resp.StatusCode, body)
	}
	var raw struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed batch: decode: %w", err)
	}
	if len(raw.Embeddings) == 0 {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("cohere embed batch: empty embeddings for model %s", c.Model)
	}
	for i, emb := range raw.Embeddings {
		if len(emb) == 0 {
			c.breaker.RecordFailure()
			return nil, fmt.Errorf("cohere embed batch: empty embedding at index %d for model %s", i, c.Model)
		}
	}
	for i, emb := range raw.Embeddings {
		if i == 0 {
			continue
		}
		if len(emb) != len(raw.Embeddings[0]) {
			c.breaker.RecordFailure()
			return nil, fmt.Errorf("cohere embed batch: embedding at index %d has dimension %d, want %d", i, len(emb), len(raw.Embeddings[0]))
		}
	}
	c.breaker.RecordSuccess()
	return raw.Embeddings, nil
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

// SetTripCallback sets a function to be called synchronously when the
// circuit breaker trips (issue #971).
func (c *CohereEmbedder) SetTripCallback(kind string, cb func(kind string)) {
	c.breaker.SetTripCallback(kind, cb)
}

// Kind returns the circuit kind string for this embedder ("cohere").
// Used for RAG circuit breaker observability (issue #886).
func (c *CohereEmbedder) Kind() string {
	return "cohere"
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

// SetTripCallback sets a function to be called synchronously when the
// circuit breaker trips (issue #971).
func (o *OllamaEmbedder) SetTripCallback(kind string, cb func(kind string)) {
	o.breaker.SetTripCallback(kind, cb)
}

// Kind returns the circuit kind string for this embedder ("ollama").
// Used for RAG circuit breaker observability (issue #886).
func (o *OllamaEmbedder) Kind() string {
	return "ollama"
}
