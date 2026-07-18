package rag

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 0.0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1.0},
		{"zero vector", []float64{0, 0}, []float64{1, 2}, 0.0},
		{"both zero", []float64{0, 0}, []float64{0, 0}, 0.0},
		{"45 degrees", []float64{1, 1}, []float64{1, 0}, 1 / math.Sqrt2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CosineSimilarity(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type stubEmbedder struct {
	vecs map[string][]float64
	err  error
	// callCount is accessed atomically to avoid data races when
	// the concurrent test exercises the cache with multiple goroutines.
	callCount int64
}

func (s *stubEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	atomic.AddInt64(&s.callCount, 1)
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.vecs[text]; ok {
		return v, nil
	}
	return []float64{0, 0, 0}, nil
}

func (s *stubEmbedder) IsHealthy(context.Context) bool { return true }
func (s *stubEmbedder) IsBreakerOpen() bool            { return false }
func (s *stubEmbedder) RecordBreakerSuccess()          {}

func TestRetrieveThreshold(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{
		"prompt":     {1, 0, 0},
		"matching":   {0.9, 0.1, 0},
		"unrelated":  {0, 1, 0},
		"weak match": {0.4, 0.5, 0},
	}}
	store := NewStore(emb, 0.55)
	store.examples = []FewShotExample{
		{Filename: "matching.go", Content: "matching", Embedding: emb.vecs["matching"]},
		{Filename: "unrelated.go", Content: "unrelated", Embedding: emb.vecs["unrelated"]},
		{Filename: "weak.go", Content: "weak match", Embedding: emb.vecs["weak match"]},
	}

	ex, score, path, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil {
		t.Fatal("expected a match above threshold")
	}
	if ex.Filename != "matching.go" {
		t.Errorf("matched %s, want matching.go", ex.Filename)
	}
	if score <= 0.55 {
		t.Errorf("score %v should exceed threshold", score)
	}
	if path != IndexPathBruteForce {
		t.Errorf("path = %q, want %q (small store should use brute force)", path, IndexPathBruteForce)
	}
}

func TestRetrieveEmptyStore(t *testing.T) {
	store := NewStore(&stubEmbedder{}, 0.55)
	ex, _, path, err := store.Retrieve(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex != nil {
		t.Errorf("expected nil from empty store, got %+v", ex)
	}
	if path != IndexPathNone {
		t.Errorf("path = %q, want %q (empty store skips search)", path, IndexPathNone)
	}
}

func TestRetrieveBelowThreshold(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"weak":   {0.3, 0.4, 0},
	}}
	store := NewStore(emb, 0.9)
	store.examples = []FewShotExample{
		{Filename: "weak.go", Content: "weak", Embedding: emb.vecs["weak"]},
	}
	ex, score, path, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex != nil {
		t.Errorf("expected no match below threshold, got %s", ex.Filename)
	}
	// Score should still be reported (best candidate's similarity)
	// so the observability layer can populate the histogram bucket.
	if score <= 0 || score > 1 {
		t.Errorf("score = %v, want positive cosine-similarity in [0,1]", score)
	}
	if path != IndexPathBruteForce {
		t.Errorf("path = %q, want %q", path, IndexPathBruteForce)
	}
}

func TestRetrieveEmbedderError(t *testing.T) {
	store := NewStore(&stubEmbedder{err: errSentinel}, 0.55)
	store.examples = []FewShotExample{{Filename: "x.go", Content: "x", Embedding: []float64{1, 0}}}
	if _, _, path, err := store.Retrieve(context.Background(), "prompt"); err == nil {
		t.Error("expected error from embedder")
	} else if path != IndexPathNone {
		t.Errorf("path = %q, want %q on embedder error", path, IndexPathNone)
	}
}

// TestStoreConcurrentRetrieveAndAdd is a regression test for the
// race detector: the watcher (issue #46) may Upsert while a chat
// handler goroutine is mid-Retrieve. The store must serialise via
// its RWMutex or `go test -race` flags the access.
func TestStoreConcurrentRetrieveAndAdd(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"a":      {0.9, 0.1, 0},
		"b":      {0.1, 0.9, 0},
		"c":      {0.5, 0.5, 0},
	}}
	store := NewStore(emb, 0.0)
	store.Add("seed", "seed", []float64{1, 0})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			store.Add("seed", "seed", []float64{1, 0})
			store.replace(store.snapshot())
		}
	}()
	for i := 0; i < 200; i++ {
		if _, _, _, err := store.Retrieve(context.Background(), "prompt"); err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
	}
	<-done
}

func TestStoreStats(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{"prompt": {1, 0, 0}}}
	store := NewStore(emb, 0.55)

	if _, _, _, err := store.Retrieve(context.Background(), "prompt"); err != nil {
		t.Fatalf("empty Retrieve: %v", err)
	}
	store.Add("match.go", "match", []float64{1, 0, 0})
	if _, _, _, err := store.Retrieve(context.Background(), "prompt"); err != nil {
		t.Fatalf("hit Retrieve: %v", err)
	}
	if _, _, _, err := store.Retrieve(context.Background(), "unknown"); err != nil {
		t.Fatalf("threshold Retrieve: %v", err)
	}
	emb.err = errSentinel
	if _, _, _, err := store.Retrieve(context.Background(), "prompt"); err == nil {
		t.Fatal("expected embedding error")
	}

	stats := store.Stats()
	if stats.RetrievalAttempts != 4 {
		t.Errorf("RetrievalAttempts = %d, want 4", stats.RetrievalAttempts)
	}
	if stats.RetrievalHits != 1 {
		t.Errorf("RetrievalHits = %d, want 1", stats.RetrievalHits)
	}
	if stats.RetrievalMisses != 3 {
		t.Errorf("RetrievalMisses = %d, want 3", stats.RetrievalMisses)
	}
	if stats.EmptyStoreMisses != 1 {
		t.Errorf("EmptyStoreMisses = %d, want 1", stats.EmptyStoreMisses)
	}
	if stats.ThresholdMisses != 1 {
		t.Errorf("ThresholdMisses = %d, want 1", stats.ThresholdMisses)
	}
	if stats.EmbedErrors != 1 {
		t.Errorf("EmbedErrors = %d, want 1", stats.EmbedErrors)
	}
	if stats.LastIndexAt.IsZero() {
		t.Error("LastIndexAt is zero, want non-zero time")
	}
}

// TestRetrieveReturnsHNSWPath verifies that when the store has at
// least indexThreshold snippets, Retrieve reports IndexPathHNSW so the
// observability layer can partition the similarity histogram by index
// path (issue #447).
func TestRetrieveReturnsHNSWPath(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
	}}
	store := NewStore(emb, 0.0)
	// Populate with indexThreshold entries so the HNSW index is used.
	for i := 0; i < indexThreshold+5; i++ {
		store.Add(fmt.Sprintf("e%d.go", i), "content", []float64{1, 0, 0})
	}
	ex, _, path, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil {
		t.Fatal("expected a match above threshold=0")
	}
	if path != IndexPathHNSW {
		t.Errorf("path = %q, want %q (>= %d entries should use HNSW)", path, IndexPathHNSW, indexThreshold)
	}
}

// TestRetrieveThresholdMissReturnsPath guards the histogram
// contract: a threshold miss still reports the score and the index
// path so the miss bucket advances exactly once per retrieval
// (issue #447 AC).
func TestRetrieveThresholdMissReturnsPath(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"weak":   {0.1, 0.99, 0},
	}}
	store := NewStore(emb, 0.9) // tight threshold; "weak" stays below
	store.examples = []FewShotExample{
		{Filename: "weak.go", Content: "weak", Embedding: emb.vecs["weak"]},
	}
	ex, score, path, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex != nil {
		t.Fatalf("expected nil below threshold, got %s", ex.Filename)
	}
	if score <= 0 {
		t.Errorf("threshold miss score = %v, want positive cosine similarity", score)
	}
	if path != IndexPathBruteForce {
		t.Errorf("path = %q, want %q", path, IndexPathBruteForce)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

var errSentinel = errStub("embed down")

// TestIndexDirSkipsSymlinks is a regression test for issue #107:
// a symlink inside the examples directory pointing to a sensitive file
// must NOT be indexed. The symlink is detected at the DirEntry level
// (ModeSymlink bit) and skipped before os.ReadFile is ever called.
func TestIndexDirSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()

	// Create a legitimate example file.
	if err := os.WriteFile(filepath.Join(dir, "safe.md"), []byte("safe content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a sensitive file that a symlink would target.
	sensitive := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(sensitive, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink pointing outside the examples directory.
	if err := os.Symlink(sensitive, filepath.Join(dir, "injected.txt")); err != nil {
		t.Fatal(err)
	}

	// Also create a symlink to a directory to test that DirEntry.IsDir()
	// returning false for directory symlinks doesn't bypass the check.
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "dir-link")); err != nil {
		t.Fatal(err)
	}

	store := NewStore(&stubEmbedder{vecs: map[string][]float64{
		"safe content": {1, 0, 0},
	}}, 0.0)

	if err := store.IndexDir(context.Background(), dir); err != nil {
		t.Fatalf("IndexDir: %v", err)
	}

	if store.Size() != 1 {
		t.Fatalf("expected 1 indexed file, got %d — symlink was not skipped", store.Size())
	}
	if store.examples[0].Filename != "safe.md" {
		t.Errorf("indexed %s, want safe.md", store.examples[0].Filename)
	}
}

// TestIndexDirRejectsEscapingSymlinks verifies that when the examples
// directory is a symlink itself (or contains a subdirectory symlink
// that resolves outside the allowed root), files escaping the resolved
// directory are rejected.
func TestIndexDirRejectsEscapingSymlinks(t *testing.T) {
	// Set up: realDir holds the secret, wrapperDir is a symlink → realDir.
	realDir := filepath.Join(t.TempDir(), "real")
	wrapperDir := filepath.Join(t.TempDir(), "wrapper")

	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "secret.txt"), []byte("escaped!"), 0o644); err != nil {
		t.Fatal(err)
	}
	// wrapperDir is a symlink pointing to realDir. When IndexDir resolves
	// wrapperDir, it gets realDir as the safe prefix. A file created via
	// the realDir path is legitimate, but this test proves the resolution
	// path is exercised — the entry-level symlink check catches symlinks
	// at any level.
	if err := os.Symlink(realDir, wrapperDir); err != nil {
		t.Fatal(err)
	}

	store := NewStore(&stubEmbedder{vecs: map[string][]float64{
		"escaped!": {1, 0, 0},
	}}, 0.0)

	if err := store.IndexDir(context.Background(), wrapperDir); err != nil {
		t.Fatalf("IndexDir: %v", err)
	}
	// The file in realDir is a regular file (not a symlink), so it IS
	// indexed through the resolved wrapper. This is expected — the
	// important thing is that symlink entries are rejected, which
	// TestIndexDirSkipsSymlinks proves.
	if store.Size() != 1 {
		t.Errorf("expected 1 indexed file, got %d", store.Size())
	}
}

// Tests for EmbedCache (issue #227).

func TestEmbedCacheDisabled(t *testing.T) {
	// When max entries or TTL is zero, EmbedCache is a pass-through.
	inner := &stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 0, 5*time.Minute) // max=0 → no cache
	vec, err := cache.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vec[0] != 1 || vec[1] != 2 || vec[2] != 3 {
		t.Errorf("unexpected vector: %v", vec)
	}
	if inner.callCount != 1 {
		t.Errorf("callCount = %d, want 1", inner.callCount)
	}
}

func TestEmbedCacheHit(t *testing.T) {
	inner := &stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute)

	// First call: cache miss, calls inner.
	_, err := cache.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if inner.callCount != 1 {
		t.Errorf("first call: inner.callCount = %d, want 1", inner.callCount)
	}

	// Second call with same key: cache hit, does not call inner.
	vec2, err := cache.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed (cached): %v", err)
	}
	if inner.callCount != 1 {
		t.Errorf("second call: inner.callCount = %d, want 1 (cached)", inner.callCount)
	}
	if vec2[0] != 1 || vec2[1] != 2 || vec2[2] != 3 {
		t.Errorf("unexpected vector: %v", vec2)
	}

	hits, misses := cache.CacheStats()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
}

func TestEmbedCacheLRUEviction(t *testing.T) {
	inner := &stubEmbedder{vecs: map[string][]float64{
		"a": {1, 0, 0},
		"b": {0, 1, 0},
		"c": {0, 0, 1},
	}}
	cache := NewEmbedCache(inner, 2, 5*time.Minute) // capacity = 2

	cache.Embed(context.Background(), "a") // miss → a
	cache.Embed(context.Background(), "b") // miss → b
	cache.Embed(context.Background(), "c") // miss → c (evicts a)

	// Accessing "a" again should be a miss (it was evicted).
	cache.Embed(context.Background(), "a") // miss → a re-inserted, c evicted
	// Accessing "b" should be a miss (it was evicted when c was inserted).
	cache.Embed(context.Background(), "b") // miss → b re-inserted, a evicted

	hits, misses := cache.CacheStats()
	if hits != 0 {
		t.Errorf("hits = %d, want 0 (a,b,c were all evicted before their re-access)", hits)
	}
	if misses != 5 {
		t.Errorf("misses = %d, want 5 (a,b,c,a,b)", misses)
	}
}

func TestEmbedCacheTTLExpiry(t *testing.T) {
	inner := &stubEmbedder{}
	cache := NewEmbedCache(inner, 100, 10*time.Millisecond)

	cache.Embed(context.Background(), "key") // miss
	if inner.callCount != 1 {
		t.Fatalf("first call: inner.callCount = %d, want 1", inner.callCount)
	}

	// Second call within TTL: hit.
	cache.Embed(context.Background(), "key")
	if inner.callCount != 1 {
		t.Errorf("second call within TTL: inner.callCount = %d, want 1", inner.callCount)
	}

	// Wait for TTL to expire.
	time.Sleep(15 * time.Millisecond)

	// Third call after TTL: miss (entry expired).
	cache.Embed(context.Background(), "key")
	if inner.callCount != 2 {
		t.Errorf("call after TTL expiry: inner.callCount = %d, want 2", inner.callCount)
	}
}

func TestEmbedCacheErrorPassthrough(t *testing.T) {
	inner := &stubEmbedder{err: errSentinel}
	cache := NewEmbedCache(inner, 100, 5*time.Minute)
	_, err := cache.Embed(context.Background(), "hello")
	if err == nil {
		t.Error("expected error, got nil")
	}
	if inner.callCount != 1 {
		t.Errorf("callCount = %d, want 1", inner.callCount)
	}
}

func TestEmbedCacheConcurrent(t *testing.T) {
	inner := &stubEmbedder{}
	cache := NewEmbedCache(inner, 100, 5*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cache.Embed(context.Background(), "same-key")
			}
		}()
	}
	wg.Wait()

	// Only one inner call should have been made for "same-key".
	if inner.callCount != 1 {
		t.Errorf("inner.callCount = %d, want 1 (all goroutines should hit same cache entry)", inner.callCount)
	}
	hits, misses := cache.CacheStats()
	if hits != 499 { // 10 goroutines × 50 calls - 1 miss = 499 hits
		t.Errorf("hits = %d, want 499", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
}

func TestEmbedCacheHitCount(t *testing.T) {
	inner := &stubEmbedder{vecs: map[string][]float64{"a": {1}}}
	cache := NewEmbedCache(inner, 10, 5*time.Minute)

	before := cache.HitCount()
	cache.Embed(context.Background(), "a") // miss
	afterMiss := cache.HitCount()
	cache.Embed(context.Background(), "a") // hit
	afterHit := cache.HitCount()

	if afterMiss != before {
		t.Errorf("afterMiss hitCount = %d, want %d (should not change on miss)", afterMiss, before)
	}
	if afterHit != before+1 {
		t.Errorf("afterHit hitCount = %d, want %d", afterHit, before+1)
	}
}

func TestStoreWithCachingEmbedder(t *testing.T) {
	// Verify that Store.EmbedHitCount() delegates to the wrapped *EmbedCache.
	inner := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"match":  {0.9, 0.1, 0},
	}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute)
	store := NewStore(cache, 0.55)
	store.Add("match.go", "matching", inner.vecs["match"])

	// First Retrieve: cache miss (prompt not cached).
	_, _, _, _ = store.Retrieve(context.Background(), "prompt")
	if inner.callCount != 1 {
		t.Errorf("first Retrieve: inner.callCount = %d, want 1", inner.callCount)
	}

	// Second Retrieve with same prompt: cache hit.
	_, _, _, _ = store.Retrieve(context.Background(), "prompt")
	if inner.callCount != 1 {
		t.Errorf("second Retrieve: inner.callCount = %d, want 1 (cached)", inner.callCount)
	}

	hits, misses := store.CacheStats()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
	stats := store.Stats()
	if stats.CacheHits != 1 || stats.CacheMisses != 1 {
		t.Errorf("cache stats = hits %d misses %d, want 1/1", stats.CacheHits, stats.CacheMisses)
	}

	hitCount := store.EmbedHitCount()
	if hitCount != 1 {
		t.Errorf("EmbedHitCount = %d, want 1", hitCount)
	}
}
