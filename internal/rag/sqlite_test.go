package rag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestPersistentStore opens an in-memory PersistentStore with a
// deterministic stub embedder keyed by content. Cleanup closes the
// underlying DB.
func newTestPersistentStore(t *testing.T) *PersistentStore {
	t.Helper()
	ps, err := OpenPersistentStore(":memory:", &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return ps
}

func TestOpenPersistentStoreRejectsEmptyPath(t *testing.T) {
	if _, err := OpenPersistentStore("", &stubEmbedder{}, 0.55); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestPersistentStoreUpsertAndLoad(t *testing.T) {
	ps := newTestPersistentStore(t)
	ctx := context.Background()

	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "alpha.go",
		Content:   "alpha content",
		Embedding: []float64{1, 0, 0},
	}); err != nil {
		t.Fatalf("Upsert alpha: %v", err)
	}
	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "beta.go",
		Content:   "beta content",
		Embedding: []float64{0, 1, 0},
	}); err != nil {
		t.Fatalf("Upsert beta: %v", err)
	}
	if got := ps.Size(); got != 2 {
		t.Fatalf("Size after Upsert = %d, want 2", got)
	}

	// Persist to an on-disk DB and reopen it to verify the round-trip.
	onDisk := filepath.Join(t.TempDir(), "rag.db")
	disk, err := OpenPersistentStore(onDisk, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore(disk): %v", err)
	}
	if err := disk.Upsert(ctx, FewShotExample{
		Filename:  "alpha.go",
		Content:   "alpha content",
		Embedding: []float64{1, 0, 0},
	}); err != nil {
		t.Fatalf("disk Upsert alpha: %v", err)
	}
	if err := disk.Upsert(ctx, FewShotExample{
		Filename:  "beta.go",
		Content:   "beta content",
		Embedding: []float64{0, 1, 0},
	}); err != nil {
		t.Fatalf("disk Upsert beta: %v", err)
	}
	if err := disk.Close(); err != nil {
		t.Fatalf("disk Close: %v", err)
	}

	disk2, err := OpenPersistentStore(onDisk, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = disk2.Close() })

	n, err := disk2.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 2 {
		t.Fatalf("Load returned %d rows, want 2", n)
	}
	if got := disk2.Size(); got != 2 {
		t.Errorf("Size after Load = %d, want 2", got)
	}

	// Reopen with a counting embedder keyed by the prompt text. If
	// the disk cache is bypassed (i.e. someone re-embedded every
	// indexed row) we'd see N+1 Embed calls (one per indexed row
	// plus the prompt). With the cache intact we see exactly 1.
	counter := &indexedCallCounter{
		vecs: map[string][]float64{
			"alpha content": {1, 0, 0}, // matches the stored embedding
		},
	}
	failingStore, err := OpenPersistentStore(onDisk, counter, 0.55)
	if err != nil {
		t.Fatalf("reopen with counter: %v", err)
	}
	t.Cleanup(func() { _ = failingStore.Close() })
	if _, err := failingStore.Load(ctx); err != nil {
		t.Fatalf("Load with counter: %v", err)
	}
	ex, _, err := failingStore.Retrieve(ctx, "alpha content")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil || ex.Filename != "alpha.go" {
		t.Errorf("Retrieve returned %v, want alpha.go", ex)
	}
	if counter.totalCalls() != 1 {
		t.Errorf("embedder calls = %d, want 1 (only the prompt, none for indexed rows)", counter.totalCalls())
	}
}

func TestPersistentStoreUpsertReplacesExisting(t *testing.T) {
	// Use a deterministic embedder so the prompt vector matches
	// the stored vector — the default stubEmbedder returns the
	// zero vector for unknown prompts, which never crosses the
	// retrieval threshold.
	emb := &vectorEmbedder{
		vecs: map[string][]float64{
			"v2": {1, 0, 0},
		},
	}
	ps, err := OpenPersistentStore(":memory:", emb, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	ctx := context.Background()

	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "x.go",
		Content:   "v1",
		Embedding: []float64{0, 0, 0},
	}); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "x.go",
		Content:   "v2",
		Embedding: []float64{1, 0, 0},
	}); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}

	if got := ps.Size(); got != 1 {
		t.Errorf("Size after duplicate Upsert = %d, want 1 (replaced)", got)
	}
	ex, _, err := ps.Retrieve(ctx, "v2")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil || ex.Content != "v2" {
		t.Errorf("Retrieve returned %v, want v2", ex)
	}
}

func TestPersistentStoreRemove(t *testing.T) {
	ps := newTestPersistentStore(t)
	ctx := context.Background()

	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "a.go",
		Content:   "a",
		Embedding: []float64{1, 0},
	}); err != nil {
		t.Fatalf("Upsert a: %v", err)
	}
	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "b.go",
		Content:   "b",
		Embedding: []float64{0, 1},
	}); err != nil {
		t.Fatalf("Upsert b: %v", err)
	}
	if err := ps.Remove(ctx, "a.go"); err != nil {
		t.Fatalf("Remove a: %v", err)
	}
	if got := ps.Size(); got != 1 {
		t.Errorf("Size after Remove = %d, want 1", got)
	}

	// Idempotent: removing a missing row should not error.
	if err := ps.Remove(ctx, "missing.go"); err != nil {
		t.Errorf("Remove missing: %v", err)
	}
}

func TestPersistentStoreUpsertValidatesFilename(t *testing.T) {
	ps := newTestPersistentStore(t)
	if err := ps.Upsert(context.Background(), FewShotExample{
		Filename:  "",
		Embedding: []float64{1, 0},
	}); err == nil {
		t.Fatal("expected error for empty filename")
	}
}

func TestPersistentStoreLoadEmpty(t *testing.T) {
	ps := newTestPersistentStore(t)
	n, err := ps.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 0 {
		t.Errorf("Load empty = %d, want 0", n)
	}
	if ps.Size() != 0 {
		t.Errorf("Size after empty Load = %d, want 0", ps.Size())
	}
}

func TestPersistentStoreLoadOrIndexFreshDB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one.go"), []byte("one"), 0o644); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.go"), []byte("two"), 0o644); err != nil {
		t.Fatalf("write two: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "rag.db")
	ps, err := OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	n, err := ps.LoadOrIndex(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadOrIndex: %v", err)
	}
	if n != 2 {
		t.Errorf("LoadOrIndex = %d, want 2", n)
	}

	// Reopen with a failing embedder: if Load reads from disk,
	// Retrieve must succeed without calling Embed.
	ps2, err := OpenPersistentStore(dbPath, &countingErrEmbedder{err: errors.New("must not call embedder for indexed rows")}, 0.55)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = ps2.Close() })
	n2, err := ps2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if n2 != 2 {
		t.Errorf("Load after restart = %d, want 2", n2)
	}
}

func TestPersistentStoreLoadOrIndexSkipsEmbedWhenDBHasRows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.go"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "rag.db")
	ps, err := OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	// Prime the DB so LoadOrIndex sees an existing row and skips
	// IndexDir (which would call the embedder).
	if err := ps.Upsert(context.Background(), FewShotExample{
		Filename:  "seed.go",
		Content:   "seed",
		Embedding: []float64{1, 0, 0},
	}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// Reopen with an embedder that would fail if called.
	if err := ps.Close(); err != nil {
		t.Fatalf("close ps: %v", err)
	}
	failing := &countingErrEmbedder{err: errors.New("embedder must not be called")}
	ps2, err := OpenPersistentStore(dbPath, failing, 0.55)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = ps2.Close() })

	n, err := ps2.LoadOrIndex(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadOrIndex: %v", err)
	}
	if n != 1 {
		t.Errorf("LoadOrIndex = %d, want 1", n)
	}
	if failing.calls != 0 {
		t.Errorf("embedder was called %d times; want 0 (DB had rows)", failing.calls)
	}
}

func TestPersistentStoreIndexDirCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	ps := newTestPersistentStore(t)
	if err := ps.IndexDir(context.Background(), dir); err != nil {
		t.Fatalf("IndexDir missing: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("IndexDir missing should mkdir the dir: %v", err)
	}
}

func TestPersistentStoreRetrieveUsesInMemoryState(t *testing.T) {
	// Use a deterministic embedder keyed by the prompt text so
	// the prompt embedding matches the indexed embedding above
	// the retrieval threshold.
	emb := &vectorEmbedder{
		vecs: map[string][]float64{
			"match": {1, 0, 0},
		},
	}
	ps, err := OpenPersistentStore(":memory:", emb, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	ctx := context.Background()
	if err := ps.Upsert(ctx, FewShotExample{
		Filename:  "match.go",
		Content:   "match",
		Embedding: []float64{1, 0, 0},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	ex, _, err := ps.Retrieve(ctx, "match")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil || ex.Filename != "match.go" {
		t.Errorf("Retrieve returned %v, want match.go", ex)
	}
}

func TestPersistentStoreCloseIsIdempotent(t *testing.T) {
	ps := newTestPersistentStore(t)
	if err := ps.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ps.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestPersistentStoreCorruptEmbeddingIsSkipped exercises the
// graceful-degradation path: a row whose BLOB is unparseable
// produces a warning and is dropped from the in-memory slice, but
// does not abort the Load.
func TestPersistentStoreCorruptEmbeddingIsSkipped(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rag.db")
	ps, err := OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	// Insert one good row directly via Upsert (so we know the
	// schema is sound) then corrupt the embedding blob of one row
	// with raw SQL.
	if err := ps.Upsert(context.Background(), FewShotExample{
		Filename:  "good.go",
		Content:   "good",
		Embedding: []float64{1, 0},
	}); err != nil {
		t.Fatalf("Upsert good: %v", err)
	}

	// Direct INSERT with a malformed blob — bypasses the typed
	// encode helper so the on-disk row is genuinely corrupt.
	if _, err := ps.db.ExecContext(context.Background(),
		"INSERT INTO rag_examples (filename, content, embedding, indexed_at) VALUES (?, ?, ?, ?)",
		"corrupt.go", "corrupt", []byte("not a gob"), time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	n, err := ps.Load(context.Background())
	if err != nil {
		t.Fatalf("Load with corrupt row: %v", err)
	}
	if n != 1 {
		t.Errorf("Load with corrupt row = %d, want 1 (corrupt skipped)", n)
	}
	if ps.Size() != 1 {
		t.Errorf("Size with corrupt row = %d, want 1", ps.Size())
	}
}

// countingErrEmbedder always errors on Embed but counts calls, so
// tests can assert it was or wasn't invoked.
type countingErrEmbedder struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (c *countingErrEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	c.mu.Lock()
	c.calls++
	err := c.err
	c.mu.Unlock()
	return nil, err
}

func (c *countingErrEmbedder) IsHealthy(_ context.Context) bool {
	return c.err == nil
}

// Calls returns the number of Embed invocations observed by this
// embedder. Safe for concurrent use.
func (c *countingErrEmbedder) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// vectorEmbedder is like stubEmbedder but is concurrency-safe, so
// Retrieve can call it from a goroutine while the test reads
// other state.
type vectorEmbedder struct {
	mu   sync.Mutex
	vecs map[string][]float64
	err  error
}

func (v *vectorEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return nil, v.err
	}
	if x, ok := v.vecs[text]; ok {
		out := make([]float64, len(x))
		copy(out, x)
		return out, nil
	}
	return []float64{0, 0, 0}, nil
}

func (v *vectorEmbedder) IsHealthy(_ context.Context) bool {
	return v.err == nil
}

// indexedCallCounter counts every Embed call so tests can
// distinguish the disk-cache fast path (1 call — only the
// prompt) from a regression where the cache was bypassed
// (N+1 calls — prompt plus every indexed row).
type indexedCallCounter struct {
	mu    sync.Mutex
	vecs  map[string][]float64
	calls int
}

func (c *indexedCallCounter) Embed(_ context.Context, text string) ([]float64, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if v, ok := c.vecs[text]; ok {
		out := make([]float64, len(v))
		copy(out, v)
		return out, nil
	}
	return []float64{0, 0, 0}, nil
}

func (c *indexedCallCounter) IsHealthy(_ context.Context) bool {
	return true
}

func (c *indexedCallCounter) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
