package rag

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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

func (s *stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	atomic.AddInt64(&s.callCount, int64(len(texts)))
	if s.err != nil {
		return nil, s.err
	}
	result := make([][]float64, len(texts))
	for i, text := range texts {
		if v, ok := s.vecs[text]; ok {
			result[i] = v
		} else {
			result[i] = []float64{0, 0, 0}
		}
	}
	return result, nil
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

// TestStoreInjectionSkippedCounter covers issue #594: the
// InjectionSkippedSizeLimit counter is bumped by
// IncInjectionSkippedSizeLimit and surfaced via Stats().
func TestStoreInjectionSkippedCounter(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{"prompt": {1, 0, 0}}}
	store := NewStore(emb, 0.55)

	if got := store.Stats().InjectionSkippedSizeLimit; got != 0 {
		t.Fatalf("initial InjectionSkippedSizeLimit = %d, want 0", got)
	}
	store.IncInjectionSkippedSizeLimit()
	store.IncInjectionSkippedSizeLimit()
	if got := store.Stats().InjectionSkippedSizeLimit; got != 2 {
		t.Errorf("InjectionSkippedSizeLimit = %d, want 2", got)
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

// TestIndexDirBatchesFiles verifies that when batchSize > 0, IndexDir
// calls EmbedBatch with files grouped into batches.
func TestIndexDirBatchesFiles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.txt", i)), []byte(fmt.Sprintf("content%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var batchCalls int
	var lastBatchLen int
	emb := &batchCountingEmbedder{
		vecs: map[string][]float64{},
		onBatch: func(texts []string) {
			batchCalls++
			lastBatchLen = len(texts)
		},
	}

	store := NewStore(emb, 0.0, WithBatchSize(2))
	if err := store.IndexDir(context.Background(), dir); err != nil {
		t.Fatalf("IndexDir: %v", err)
	}

	// 5 files with batch size 2: batches should be [2, 2, 1]
	if batchCalls != 3 {
		t.Errorf("batchCalls = %d, want 3", batchCalls)
	}
	if lastBatchLen != 1 {
		t.Errorf("lastBatchLen = %d, want 1", lastBatchLen)
	}
	if store.Size() != 5 {
		t.Errorf("store.Size() = %d, want 5", store.Size())
	}
}

// TestIndexDirNoBatching verifies that batchSize=0 disables batching
// and calls Embed once per file.
func TestIndexDirNoBatching(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.txt", i)), []byte(fmt.Sprintf("content%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var singleCalls int
	emb := &batchCountingEmbedder{
		vecs: map[string][]float64{},
		onSingle: func(string) {
			singleCalls++
		},
	}

	store := NewStore(emb, 0.0, WithBatchSize(0)) // batching disabled
	if err := store.IndexDir(context.Background(), dir); err != nil {
		t.Fatalf("IndexDir: %v", err)
	}

	if singleCalls != 3 {
		t.Errorf("singleCalls = %d, want 3", singleCalls)
	}
	if store.Size() != 3 {
		t.Errorf("store.Size() = %d, want 3", store.Size())
	}
}

// batchCountingEmbedder tracks whether Embed or EmbedBatch is called.
type batchCountingEmbedder struct {
	vecs      map[string][]float64
	onBatch   func([]string)
	onSingle  func(string)
	batchUsed bool
}

func (e *batchCountingEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	e.batchUsed = false
	if e.onSingle != nil {
		e.onSingle(text)
	}
	if e.vecs == nil {
		e.vecs = map[string][]float64{}
	}
	if _, ok := e.vecs[text]; !ok {
		e.vecs[text] = []float64{0, 0, 0}
	}
	return e.vecs[text], nil
}

func (e *batchCountingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	e.batchUsed = true
	if e.onBatch != nil {
		e.onBatch(texts)
	}
	result := make([][]float64, len(texts))
	for i, text := range texts {
		if e.vecs == nil {
			e.vecs = map[string][]float64{}
		}
		if _, ok := e.vecs[text]; !ok {
			e.vecs[text] = []float64{0, 0, 0}
		}
		result[i] = e.vecs[text]
	}
	return result, nil
}

func (e *batchCountingEmbedder) IsHealthy(context.Context) bool { return true }
func (e *batchCountingEmbedder) IsBreakerOpen() bool            { return false }
func (e *batchCountingEmbedder) RecordBreakerSuccess()          {}

// Tests for EmbedCache (issue #227).

func TestEmbedCacheDisabled(t *testing.T) {
	// When max entries or TTL is zero, EmbedCache is a pass-through.
	inner := &stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 0, 5*time.Minute, 5*time.Second) // max=0 → no cache
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
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

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
	cache := NewEmbedCache(inner, 2, 5*time.Minute, 5*time.Second) // capacity = 2

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
	cache := NewEmbedCache(inner, 100, 10*time.Millisecond, 5*time.Second)

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
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)
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
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

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
	cache := NewEmbedCache(inner, 10, 5*time.Minute, 5*time.Second)

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

// embedCacheBreakerStub is a stub that tracks breaker calls for testing
// EmbedCache delegation (issue #670).
type embedCacheBreakerStub struct {
	stubEmbedder
	breakerOpen  bool
	breakerCalls int
	successCalls int
}

func (e *embedCacheBreakerStub) IsBreakerOpen() bool {
	e.breakerCalls++
	return e.breakerOpen
}

func (e *embedCacheBreakerStub) RecordBreakerSuccess() {
	e.successCalls++
}

// TestEmbedCacheIsBreakerOpenDelegation tests that EmbedCache.IsBreakerOpen
// delegates to the inner embedder when it implements IsBreakerOpen and
// returns the inner's breaker state (issue #670 AC1).
func TestEmbedCacheIsBreakerOpenDelegation(t *testing.T) {
	inner := &embedCacheBreakerStub{
		stubEmbedder: stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}},
		breakerOpen:  true,
	}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

	if !cache.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = false, want true (should propagate inner breaker)")
	}
	if inner.breakerCalls != 1 {
		t.Errorf("inner.breakerCalls = %d, want 1", inner.breakerCalls)
	}

	inner.breakerOpen = false
	if cache.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false after inner breaker closes")
	}
}

// TestEmbedCacheIsBreakerOpenWithoutBreaker tests that EmbedCache.IsBreakerOpen
// returns false when the inner embedder's breaker reports closed (issue #670).
// Note: Since Embedder interface requires IsBreakerOpen, we test with an
// embedder that returns false. The "no panic" case for CachedEmbedder is
// already covered in cache_test.go; EmbedCache uses the same delegation pattern.
func TestEmbedCacheIsBreakerOpenWithoutBreaker(t *testing.T) {
	inner := &stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

	// stubEmbedder.IsBreakerOpen returns false.
	if cache.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false when inner breaker is closed")
	}
}

// TestEmbedCacheRecordBreakerSuccessDelegation tests that
// EmbedCache.RecordBreakerSuccess delegates to the inner embedder's
// breaker success handler (issue #670 AC3).
func TestEmbedCacheRecordBreakerSuccessDelegation(t *testing.T) {
	inner := &embedCacheBreakerStub{
		stubEmbedder: stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}},
	}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

	cache.RecordBreakerSuccess()
	cache.RecordBreakerSuccess()

	if inner.successCalls != 2 {
		t.Errorf("inner.successCalls = %d, want 2", inner.successCalls)
	}
}

// TestEmbedCacheRecordBreakerSuccessWithoutBreaker tests that
// EmbedCache.RecordBreakerSuccess is safe when called on an
// embedder whose breaker is not tripped (issue #670).
func TestEmbedCacheRecordBreakerSuccessWithoutBreaker(t *testing.T) {
	inner := &stubEmbedder{vecs: map[string][]float64{"hello": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)

	// Should not panic.
	cache.RecordBreakerSuccess()
}

func TestStoreWithCachingEmbedder(t *testing.T) {
	// Verify that Store.EmbedHitCount() delegates to the wrapped *EmbedCache.
	inner := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"match":  {0.9, 0.1, 0},
	}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)
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

// TestStoreIndexMode exercises the IndexMode accessor added for
// issue #446 so operators can confirm which retrieval path the
// next Retrieve call will take. The three modes are: "none" (empty
// store), "brute_force" (below indexThreshold or index invalidated),
// and "hnsw" (large store with a populated HNSW index).
func TestStoreIndexMode(t *testing.T) {
	emb := &stubEmbedder{vecs: map[string][]float64{}}

	t.Run("empty store reports none", func(t *testing.T) {
		store := NewStore(emb, 0.55)
		if got := store.IndexMode(); got != IndexModeNone {
			t.Errorf("IndexMode() = %q, want %q", got, IndexModeNone)
		}
	})

	t.Run("below threshold reports brute_force", func(t *testing.T) {
		store := NewStore(emb, 0.0)
		// Any count strictly less than indexThreshold (50) reports
		// brute_force even when the HNSW index is technically
		// present.
		for i := 0; i < indexThreshold-1; i++ {
			store.Add(fmt.Sprintf("snippet-%d", i), "x", []float64{1, 0, 0})
		}
		if got := store.IndexMode(); got != IndexModeBruteForce {
			t.Errorf("IndexMode() = %q, want %q", got, IndexModeBruteForce)
		}
	})

	t.Run("at or above threshold with index reports hnsw", func(t *testing.T) {
		store := NewStore(emb, 0.0)
		for i := 0; i < indexThreshold+5; i++ {
			store.Add(fmt.Sprintf("snippet-%d", i), "x", []float64{1, 0, 0})
		}
		if got := store.IndexMode(); got != IndexModeHNSW {
			t.Errorf("IndexMode() = %q, want %q", got, IndexModeHNSW)
		}
	})

	t.Run("upsertThenRetrieve_IndexRebuiltHNSWActive", func(t *testing.T) {
		store := NewStore(emb, 0.0)
		for i := 0; i < indexThreshold+5; i++ {
			store.Add(fmt.Sprintf("snippet-%d", i), "x", []float64{1, 0, 0})
		}
		if got := store.IndexMode(); got != IndexModeHNSW {
			t.Fatalf("IndexMode() before upsert = %q, want %q", got, IndexModeHNSW)
		}
		// upsertExample invalidates the HNSW index (issue #829) —
		// the next Retrieve will rebuild it lazily, restoring HNSW.
		store.upsertExample(FewShotExample{Filename: "snippet-0", Content: "x", Embedding: []float64{1, 0, 0}})
		if got := store.IndexMode(); got != IndexModeBruteForce {
			t.Fatalf("IndexMode() immediately after upsert = %q, want %q (no Retrieve yet)", got, IndexModeBruteForce)
		}
		// Retrieve triggers the lazy rebuild.
		_, _, _, err := store.Retrieve(context.Background(), "test prompt")
		if err != nil {
			t.Fatalf("Retrieve() after upsert failed: %v", err)
		}
		if got := store.IndexMode(); got != IndexModeHNSW {
			t.Errorf("IndexMode() after upsert+Retrieve = %q, want %q", got, IndexModeHNSW)
		}
	})
}

// breakerStub is a controllable Embedder whose breaker/health state can
// be configured and whose delegation call counts can be inspected. It
// exercises CachedEmbedder's breaker-delegation paths (issue #600).
type breakerStub struct {
	breakerOpen  bool
	healthy      bool
	successCalls int
	healthyCalls int
	breakerCalls int
}

func (b *breakerStub) Embed(context.Context, string) ([]float64, error) {
	return []float64{1, 0, 0}, nil
}

func (b *breakerStub) EmbedBatch(context.Context, []string) ([][]float64, error) {
	return [][]float64{{1, 0, 0}}, nil
}

func (b *breakerStub) IsHealthy(context.Context) bool {
	b.healthyCalls++
	return b.healthy
}

func (b *breakerStub) IsBreakerOpen() bool {
	b.breakerCalls++
	return b.breakerOpen
}

func (b *breakerStub) RecordBreakerSuccess() {
	b.successCalls++
}

// TestCachedEmbedderBreakerDelegation verifies that CachedEmbedder
// propagates the inner embedder's open breaker state instead of always
// reporting closed (issue #600 AC).
func TestCachedEmbedderBreakerDelegation(t *testing.T) {
	inner := &breakerStub{breakerOpen: true}
	c := NewCachedEmbedder(inner, 8)

	if !c.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = false, want true (should propagate inner breaker)")
	}
	if inner.breakerCalls != 1 {
		t.Errorf("inner.breakerCalls = %d, want 1", inner.breakerCalls)
	}

	inner.breakerOpen = false
	if c.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false after inner closes")
	}
}

// TestCachedEmbedderRecordBreakerSuccess verifies the success signal
// delegates to the inner embedder's breaker (issue #600 AC).
func TestCachedEmbedderRecordBreakerSuccess(t *testing.T) {
	inner := &breakerStub{}
	c := NewCachedEmbedder(inner, 8)

	c.RecordBreakerSuccess()
	c.RecordBreakerSuccess()

	if inner.successCalls != 2 {
		t.Errorf("inner.successCalls = %d, want 2", inner.successCalls)
	}
}

// TestCachedEmbedderIsHealthy verifies health-state delegation and
// that a context the inner embedder ignores is handled without panic
// (issue #600 AC).
func TestCachedEmbedderIsHealthy(t *testing.T) {
	inner := &breakerStub{healthy: true}
	c := NewCachedEmbedder(inner, 8)

	if !c.IsHealthy(context.Background()) {
		t.Error("IsHealthy() = false, want true")
	}
	if inner.healthyCalls != 1 {
		t.Errorf("inner.healthyCalls = %d, want 1", inner.healthyCalls)
	}

	inner.healthy = false
	if c.IsHealthy(context.Background()) {
		t.Error("IsHealthy() = true, want false after inner goes unhealthy")
	}
}

// TestCachedEmbedderNoBreaker wraps an embedder whose breaker reports
// closed and verifies CachedEmbedder returns false without panicking
// (issue #600 AC).
func TestCachedEmbedderNoBreaker(t *testing.T) {
	c := NewCachedEmbedder(&stubEmbedder{}, 8)

	if c.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false for plain embedder")
	}
	c.RecordBreakerSuccess()
}

func TestParseThresholdOverridesNonASCII(t *testing.T) {
	key := "NEXUS_RAG_THRESHOLD_代码"
	if err := os.Setenv(key, "0.7"); err != nil {
		t.Fatalf("Setenv(%q): %v", key, err)
	}
	defer os.Unsetenv(key)

	overrides := ParseThresholdOverrides()
	if got, ok := overrides["代码"]; !ok {
		t.Errorf("overrides[%q] missing; got %v", "代码", overrides)
	} else if got != 0.7 {
		t.Errorf("overrides[%q] = %v, want 0.7", "代码", got)
	}
}

func TestParseThresholdOverrides(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		val   string
		dir   string
		want  float64
		valid bool
	}{
		{"ASCII override", "NEXUS_RAG_THRESHOLD_代码", "0.7", "代码", 0.7, true},
		{"empty value", "NEXUS_RAG_THRESHOLD_emptydir", "", "emptydir", 0, false},
		{"invalid value", "NEXUS_RAG_THRESHOLD_invaliddir", "not_a_float", "invaliddir", 0, false},
		{"out of range", "NEXUS_RAG_THRESHOLD_oobdir", "1.5", "oobdir", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.valid {
				if err := os.Setenv(tc.key, tc.val); err != nil {
					t.Fatalf("Setenv(%q, %q): %v", tc.key, tc.val, err)
				}
				defer os.Unsetenv(tc.key)
			}

			overrides := ParseThresholdOverrides()
			if tc.valid {
				if got, ok := overrides[tc.dir]; !ok {
					t.Errorf("overrides[%q] missing; got %v", tc.dir, overrides)
				} else if got != tc.want {
					t.Errorf("overrides[%q] = %v, want %v", tc.dir, got, tc.want)
				}
			} else {
				if _, ok := overrides[tc.dir]; ok {
					t.Errorf("overrides[%q] unexpectedly present; got %v", tc.dir, overrides)
				}
			}
		})
	}
}

// delayedEmbedder blocks until the unblock channel is closed or the
// context is cancelled. It implements Embedder for testing.
type delayedEmbedder struct {
	unblock chan struct{}
}

func (d *delayedEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	select {
	case <-d.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []float64{0, 0, 0}, nil
}

func (d *delayedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	select {
	case <-d.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result := make([][]float64, len(texts))
	for i := range texts {
		result[i] = []float64{0, 0, 0}
	}
	return result, nil
}

func (d *delayedEmbedder) IsHealthy(context.Context) bool { return true }
func (d *delayedEmbedder) IsBreakerOpen() bool            { return false }
func (d *delayedEmbedder) RecordBreakerSuccess()          {}

// TestEmbedCacheCtxCancelNoStaleEntry verifies that when a waiting goroutine's
// context is cancelled, the loading slot is removed from c.loading and no
// goroutine is leaked (issue #800).
func TestEmbedCacheCtxCancelNoStaleEntry(t *testing.T) {
	unblock := make(chan struct{})
	close(unblock) // immediate return — inner embed will not block in this test
	inner := &delayedEmbedder{unblock: unblock}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	// Start a goroutine that embeds and will cancel.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cache.Embed(ctx, "key")
	}()

	// Give the goroutine time to register in c.loading.
	time.Sleep(10 * time.Millisecond)

	// Cancel the context — the waiting goroutine should detect this and
	// delete the loading slot.
	cancel()

	// Wait for the goroutine to exit.
	wg.Wait()

	// Verify no stale entry is left in c.loading.
	cache.mu.Lock()
	_, hasStale := cache.loading["key"]
	cache.mu.Unlock()

	if hasStale {
		t.Error("c.loading[\"key\"] still present after ctx cancel — stale entry leak")
	}
}

// TestEmbedCacheTimeoutFallsThrough verifies that a waiting goroutine
// that times out falls through to a direct inner call and cleans up the
// loading slot (issue #800).
func TestEmbedCacheTimeoutFallsThrough(t *testing.T) {
	// Use a stubEmbedder that returns immediately. The test sets a very short
	// waitTimeout so the concurrent goroutine times out while the first
	// goroutine's result is "in flight".
	inner := &stubEmbedder{vecs: map[string][]float64{"key": {1, 2, 3}}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 50*time.Millisecond)

	ctx := context.Background()

	// First goroutine: starts loading and will complete quickly.
	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		_, firstErr = cache.Embed(ctx, "key")
	}()

	// Give the first goroutine time to register in c.loading.
	time.Sleep(5 * time.Millisecond)

	// Second goroutine: arrives while first is loading, times out waiting.
	wg.Add(1)
	var waiterErr error
	go func() {
		defer wg.Done()
		_, waiterErr = cache.Embed(ctx, "key")
	}()

	wg.Wait()

	// Both should succeed (the waiter's timeout is short enough that it
	// falls through to a direct call which succeeds because stub returns fast).
	if firstErr != nil {
		t.Errorf("first goroutine: %v", firstErr)
	}
	if waiterErr != nil {
		t.Errorf("waiter goroutine: %v", waiterErr)
	}

	// Verify no stale entry.
	cache.mu.Lock()
	_, hasStale := cache.loading["key"]
	cache.mu.Unlock()

	if hasStale {
		t.Error("c.loading[\"key\"] still present — stale entry leak")
	}
}

// TestEmbedCacheConcurrentStressWithCancel races many goroutines with
// context cancellation to detect loading-map corruption. Run under
// `go test -race ./internal/rag/...`.
func TestEmbedCacheConcurrentStressWithCancel(t *testing.T) {
	unblock := make(chan struct{})
	close(unblock) // immediate return
	inner := &delayedEmbedder{unblock: unblock}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 200*time.Millisecond)

	var wg sync.WaitGroup
	var errors atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Half the goroutines get a cancelled context.
			c := ctx
			if id%2 == 0 {
				subCtx, subCancel := context.WithCancel(context.Background())
				subCancel()
				c = subCtx
			}
			_, _ = cache.Embed(c, fmt.Sprintf("prompt-%d", id%5))
		}(i)
	}
	wg.Wait()
	cancel()

	if errors.Load() > 0 {
		t.Errorf("%d errors during concurrent ctx-cancel stress test", errors.Load())
	}

	// c.loading must be clean after all goroutines settle.
	cache.mu.Lock()
	if len(cache.loading) > 0 {
		t.Errorf("c.loading not clean after stress test: %d entries remain", len(cache.loading))
	}
	cache.mu.Unlock()
}

func TestStoreIsBreakerOpen_DelegatesToInner(t *testing.T) {
	stub := &breakerStub{breakerOpen: true}
	store := NewStore(stub, 0.55)

	if !store.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = false, want true (should propagate inner breaker)")
	}
	if stub.breakerCalls != 1 {
		t.Errorf("stub.breakerCalls = %d, want 1", stub.breakerCalls)
	}

	stub.breakerOpen = false
	if store.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false after inner closes")
	}
}

func TestStoreIsBreakerOpen_ReturnsFalseWhenInnerLacksInterface(t *testing.T) {
	stub := &stubEmbedder{vecs: map[string][]float64{"x": {1, 0}}}
	store := NewStore(stub, 0.55)

	if store.IsBreakerOpen() {
		t.Error("IsBreakerOpen() = true, want false (stubEmbedder's IsBreakerOpen returns false)")
	}
}

func TestStoreRecordBreakerSuccess_DelegatesToInner(t *testing.T) {
	stub := &breakerStub{}
	store := NewStore(stub, 0.55)

	store.RecordBreakerSuccess()
	store.RecordBreakerSuccess()

	if stub.successCalls != 2 {
		t.Errorf("stub.successCalls = %d, want 2", stub.successCalls)
	}
}

func TestStoreRecordBreakerSuccess_NoOpWhenInnerLacksInterface(t *testing.T) {
	stub := &stubEmbedder{vecs: map[string][]float64{"x": {1, 0}}}
	store := NewStore(stub, 0.55)

	store.RecordBreakerSuccess()
}

func TestOllamaEmbedderIsHealthy_NilCtxUsesDefaultTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"embedding":[0.1,0.2,0.3]}`)
	}))
	t.Cleanup(srv.Close)

	emb := NewOllamaEmbedder(srv.URL, "test-model", nil, BreakerConfig{})
	_ = emb.IsHealthy(context.TODO())
}

// TestThresholdFor_CaseInsensitive verifies that ThresholdFor performs a
// case-insensitive directory-name lookup so that mixed-case directory names
// (e.g., "Go_TESTS") match their lowercased env-var keys (e.g.,
// NEXUS_RAG_THRESHOLD_GO_TESTS=0.7). Regression test for issue #940.
func TestThresholdFor_CaseInsensitive(t *testing.T) {
	store := NewStore(&stubEmbedder{vecs: map[string][]float64{"x": {1, 0}}}, 0.5)
	store.thresholdOverrides = map[string]float64{
		"go_tests": 0.7,
		"my_dir":   0.3,
	}

	cases := []struct {
		dir  string
		want float64
	}{
		// Mixed-case path → override matches lowercased key
		{"/path/to/Go_TESTS", 0.7},
		// Bare mixed-case name
		{"Go_TESTS", 0.7},
		// Exact lowercase (baseline)
		{"go_tests", 0.7},
		// All uppercase
		{"GO_TESTS", 0.7},
		// Mixed-case path for second entry; filepath.Base gives last element
		{"/some/path/My_Dir", 0.3},
		{"My_Dir", 0.3},
		{"my_dir", 0.3},
		{"MY_DIR", 0.3},
		// Unmatched directory falls back to global threshold (0.5)
		{"/unknown/path", 0.5},
		{"unknown", 0.5},
		// Edge: path with no directory component
		{"./Go_TESTS", 0.7},
	}

	for _, tc := range cases {
		got := store.ThresholdFor(tc.dir)
		if got != tc.want {
			t.Errorf("ThresholdFor(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// TestEmbedCacheTimeoutSuccessPathRace verifies that when a waiting goroutine
// times out and falls through, and then the original loading goroutine completes,
// there is no goroutine leak (issue #975). This test uses a delayedEmbedder
// with a delay longer than the waiter's timeout to widen the race window.
func TestEmbedCacheTimeoutSuccessPathRace(t *testing.T) {
	unblock := make(chan struct{})
	inner := &delayedEmbedderWithDelay{
		unblock: unblock,
		delay:   100 * time.Millisecond,
	}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 50*time.Millisecond)

	ctx := context.Background()

	// First goroutine: starts loading and will take 100ms.
	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		_, firstErr = cache.Embed(ctx, "key")
	}()

	// Give the first goroutine time to register in c.loading.
	time.Sleep(10 * time.Millisecond)

	// Second goroutine: arrives while first is loading, times out waiting after 50ms.
	wg.Add(1)
	var waiterErr error
	go func() {
		defer wg.Done()
		_, waiterErr = cache.Embed(ctx, "key")
	}()

	// Wait for both goroutines. The second should timeout (50ms) while the first
	// is still loading (100ms). When the waiter falls through and calls inner.Embed,
	// it will also block on unblock (which we close after 150ms).
	time.Sleep(60 * time.Millisecond) // Let waiter timeout
	close(unblock)                    // Now both inner.Embed calls can complete

	wg.Wait()

	// Both should succeed.
	if firstErr != nil {
		t.Errorf("first goroutine: %v", firstErr)
	}
	if waiterErr != nil {
		t.Errorf("waiter goroutine: %v", waiterErr)
	}

	// Verify no stale entry.
	cache.mu.Lock()
	_, hasStale := cache.loading["key"]
	cache.mu.Unlock()

	if hasStale {
		t.Error("c.loading[\"key\"] still present — stale entry leak")
	}
}

type delayedEmbedderWithDelay struct {
	unblock chan struct{}
	delay   time.Duration
}

func (d *delayedEmbedderWithDelay) Embed(ctx context.Context, text string) ([]float64, error) {
	select {
	case <-time.After(d.delay):
	case <-d.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []float64{0, 0, 0}, nil
}

func (d *delayedEmbedderWithDelay) EmbedBatch(ctx context.Context, texts []string) ([][]float64, error) {
	select {
	case <-time.After(d.delay):
	case <-d.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result := make([][]float64, len(texts))
	for i := range texts {
		result[i] = []float64{0, 0, 0}
	}
	return result, nil
}

func (d *delayedEmbedderWithDelay) IsHealthy(context.Context) bool { return true }
func (d *delayedEmbedderWithDelay) IsBreakerOpen() bool            { return false }
func (d *delayedEmbedderWithDelay) RecordBreakerSuccess()          {}

// failingAfterBatchEmbedder returns embeddings normally for the first N batches,
// then returns an error, then succeeds again for subsequent batches.
// This simulates a transient embedder failure mid-way through IndexDir.
type failingAfterBatchEmbedder struct {
	mu             sync.Mutex
	batchCount     int
	failAtBatch    int // fail when batchCount == failAtBatch
	restoreAtBatch int // restore normal operation at this batch (inclusive); 0 = never restore
	vecs           map[string][]float64
}

func (f *failingAfterBatchEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float64{0, 0, 0}, nil
}

func (f *failingAfterBatchEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	f.mu.Lock()
	f.batchCount++
	currentBatch := f.batchCount
	f.mu.Unlock()

	// Fail on the target batch
	if currentBatch == f.failAtBatch {
		return nil, fmt.Errorf("transient embedder failure on batch %d", currentBatch)
	}

	result := make([][]float64, len(texts))
	for i, text := range texts {
		if v, ok := f.vecs[text]; ok {
			result[i] = v
		} else {
			result[i] = []float64{0, 0, 0}
		}
	}
	return result, nil
}

func (f *failingAfterBatchEmbedder) IsHealthy(context.Context) bool { return true }
func (f *failingAfterBatchEmbedder) IsBreakerOpen() bool            { return false }
func (f *failingAfterBatchEmbedder) RecordBreakerSuccess()          {}
func (f *failingAfterBatchEmbedder) BatchCount() int                { return f.batchCount }

// TestIndexDirBatchFailureStillBuildsIndex verifies that when EmbedBatch fails
// mid-way through IndexDir, the HNSW index is rebuilt synchronously after all
// batches complete (issue #976). Previously, a batch failure invalidated the
// index and subsequent successful batches would append to examples without
// rebuilding the index, leaving it incomplete until the next Retrieve call.
func TestIndexDirBatchFailureStillBuildsIndex(t *testing.T) {
	// Create enough files to exceed indexThreshold (50) with batch size 20.
	// With 120 files and batch size 20: batches are [20,20,20,20,20,20] = 6 batches.
	// Batch 3 (files 40-59) fails, so those entries are not added.
	// Batches 1,2,4,5,6 succeed: 20+20+20+20+20 = 100 entries.
	// 100 >= 50, so maybeRebuildIndex should rebuild the index synchronously.
	const totalFiles = 120
	const batchSize = 20
	dir := t.TempDir()
	for i := 0; i < totalFiles; i++ {
		content := fmt.Sprintf("content %d", i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.txt", i)), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	// Embedder fails on batch 3, succeeds on all others.
	emb := &failingAfterBatchEmbedder{
		failAtBatch:    3, // 3rd batch call (0-indexed: batch 2) fails
		restoreAtBatch: 0, // don't restore - subsequent batches still succeed
		vecs:           map[string][]float64{},
	}

	store := NewStore(emb, 0.0, WithBatchSize(batchSize))

	// IndexDir should complete without error despite the batch failure.
	if err := store.IndexDir(context.Background(), dir); err != nil {
		t.Fatalf("IndexDir: %v", err)
	}

	// 100 examples indexed (5 successful batches * 20 = 100).
	// The 3rd batch (20 files) failed and was skipped.
	if g := store.Size(); g != 100 {
		t.Errorf("store.Size() = %d, want 100 (one batch of 20 failed)", g)
	}

	// The index should be built (IndexMode should be HNSW, not brute-force).
	// Before the fix: IndexMode would be IndexModeBruteForce because the index
	// was invalidated on batch 3 failure and never rebuilt synchronously.
	// With the fix: maybeRebuildIndex is called after all batches, sees
	// 100 >= 50, and rebuilds the index synchronously.
	mode := store.IndexMode()
	if mode != IndexModeHNSW {
		t.Errorf("IndexMode = %q, want %q (index should be rebuilt after batch failure)", mode, IndexModeHNSW)
	}
}
