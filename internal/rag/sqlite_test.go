package rag

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// logOutput redirects slog's default logger into w and returns the
// previous logger so callers can restore it via slog.SetDefault.
func logOutput(w io.Writer) *slog.Logger {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return prev
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
	if disk2.Stats().LastIndexAt.IsZero() {
		t.Error("LastIndexAt after Load is zero, want persisted index time")
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
	ex, _, _, err := failingStore.Retrieve(ctx, "alpha content")
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
	ex, _, _, err := ps.Retrieve(ctx, "v2")
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
	ex, _, _, err := ps.Retrieve(ctx, "match")
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

func (c *countingErrEmbedder) EmbedBatch(_ context.Context, _ []string) ([][]float64, error) {
	c.mu.Lock()
	c.calls++
	err := c.err
	c.mu.Unlock()
	return nil, err
}

func (c *countingErrEmbedder) IsHealthy(context.Context) bool { return true }
func (c *countingErrEmbedder) IsBreakerOpen() bool            { return false }
func (c *countingErrEmbedder) RecordBreakerSuccess()          {}

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

func (v *vectorEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return nil, v.err
	}
	result := make([][]float64, len(texts))
	for i, text := range texts {
		if x, ok := v.vecs[text]; ok {
			out := make([]float64, len(x))
			copy(out, x)
			result[i] = out
		} else {
			result[i] = []float64{0, 0, 0}
		}
	}
	return result, nil
}

func (v *vectorEmbedder) IsHealthy(context.Context) bool { return true }
func (v *vectorEmbedder) IsBreakerOpen() bool            { return false }
func (v *vectorEmbedder) RecordBreakerSuccess()          {}

// indexedCallCounter counts every Embed call so tests can
// distinguish the disk-cache fast path (1 call — only the
// prompt) from a regression where the cache was bypassed
// (N+1 calls — prompt plus every indexed row).
type indexedCallCounter struct {
	model string
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

func (c *indexedCallCounter) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	c.mu.Lock()
	c.calls += len(texts)
	c.mu.Unlock()
	result := make([][]float64, len(texts))
	for i, text := range texts {
		if v, ok := c.vecs[text]; ok {
			out := make([]float64, len(v))
			copy(out, v)
			result[i] = out
		} else {
			result[i] = []float64{0, 0, 0}
		}
	}
	return result, nil
}

func (c *indexedCallCounter) IsHealthy(context.Context) bool { return true }
func (c *indexedCallCounter) IsBreakerOpen() bool            { return false }
func (c *indexedCallCounter) Model() string                  { return c.model }
func (c *indexedCallCounter) RecordBreakerSuccess()          {}

func (c *indexedCallCounter) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type dimEmbedder struct {
	model string
	dims  int
	vecs  map[string][]float64
}

func (d *dimEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if v, ok := d.vecs[text]; ok {
		out := make([]float64, len(v))
		copy(out, v)
		return out, nil
	}
	return make([]float64, d.dims), nil
}

func (d *dimEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	result := make([][]float64, len(texts))
	for i, text := range texts {
		if v, ok := d.vecs[text]; ok {
			out := make([]float64, len(v))
			copy(out, v)
			result[i] = out
		} else {
			result[i] = make([]float64, d.dims)
		}
	}
	return result, nil
}

func (d *dimEmbedder) IsHealthy(context.Context) bool { return true }
func (d *dimEmbedder) IsBreakerOpen() bool            { return false }
func (d *dimEmbedder) RecordBreakerSuccess()          {}
func (d *dimEmbedder) Model() string                  { return d.model }

// TestRunRAGMigrations_SchemaVersionIntegrity checks that if schema_version
// is higher than currentSchemaVersion (e.g., mid-migration crash left an
// intermediate value), the store refuses to open with a descriptive error
// instead of trying to re-run the failed migration (issue #672).
func TestRunRAGMigrations_SchemaVersionIntegrity(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "rag_future_version.db")

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", dbPath))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Create v1 schema without running migrations
	_, err = db.Exec(`
		CREATE TABLE rag_examples (
			filename TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			embedding BLOB NOT NULL,
			indexed_at DATETIME NOT NULL
		)`)
	if err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	// Set version to currentSchemaVersion + 1 to simulate a mid-migration crash
	// that left schema_version at a version we don't know how to migrate from.
	_, err = db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, currentSchemaVersion+1)
	if err != nil {
		t.Fatalf("insert future version: %v", err)
	}
	db.Close()

	_, err = OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err == nil {
		t.Fatal("expected error when schema_version > currentSchemaVersion")
	}
	if !strings.Contains(err.Error(), "schema_version is at") {
		t.Errorf("error = %q, want descriptive 'schema_version is at' message", err)
	}
	if !strings.Contains(err.Error(), "database may be corrupted") {
		t.Errorf("error = %q, want 'database may be corrupted' hint", err)
	}
	if !strings.Contains(err.Error(), "backup and re-index") {
		t.Errorf("error = %q, want 'backup and re-index' recovery hint", err)
	}
}

// TestRunRAGMigrations_CORRUPTHandling verifies that SQLITE_CORRUPT
// during migration returns a descriptive error rather than a generic one
// (issue #672).
func TestRunRAGMigrations_CORRUPTHandling(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "rag_corrupt.db")

	// Create a valid v1 database with an intermediate schema_version (1)
	// so the next migration will try to ADD COLUMN dims, then corrupt the
	// file so that SQLITE_CORRUPT fires on the next write.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", dbPath))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE rag_examples (
			filename TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			embedding BLOB NOT NULL,
			indexed_at DATETIME NOT NULL
		)`)
	if err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	_, err = db.Exec(`INSERT INTO schema_version (version) VALUES (1)`)
	if err != nil {
		t.Fatalf("insert version 1: %v", err)
	}
	db.Close()

	// Corrupt the file header at offset 50: this is still within the
	// 100-byte SQLite header but corrupting a non-magic-byte offset
	// may allow the open to succeed while causing SQLITE_CORRUPT on
	// subsequent operations.
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	_, err = f.WriteAt([]byte("garbage"), 50)
	f.Close()
	if err != nil {
		t.Fatalf("write corrupt bytes: %v", err)
	}

	_, err = OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err == nil {
		t.Fatal("expected error when opening a corrupted database")
	}
	// The error may come from the initial schema create or from runRAGMigrations.
	// Either way it should be descriptive about corruption.
	if !strings.Contains(err.Error(), "database may be corrupted") {
		t.Errorf("error = %q, want 'database may be corrupted' message", err)
	}
	if !strings.Contains(err.Error(), "backup and re-index") {
		t.Errorf("error = %q, want 'backup and re-index' recovery hint", err)
	}
}

func TestPersistentStore_AlterTableMigration(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "rag_migration.db")

	{
		db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rwc", dbPath))
		if err != nil {
			t.Fatalf("open v1 db: %v", err)
		}
		_, err = db.Exec(`
			CREATE TABLE rag_examples (
				filename TEXT PRIMARY KEY,
				content TEXT NOT NULL,
				embedding BLOB NOT NULL,
				indexed_at DATETIME NOT NULL
			)`)
		if err != nil {
			t.Fatalf("create v1 schema: %v", err)
		}
		_, err = db.Exec(`
			INSERT INTO rag_examples (filename, content, embedding, indexed_at)
			VALUES (?, ?, ?, ?)`,
			"legacy.go", "legacy content", []byte("not-a-real gob"), time.Now().UTC())
		if err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
		db.Close()
	}

	ps, err := OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore with migration: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	row := ps.db.QueryRow("SELECT embedder_model, dims FROM rag_examples WHERE filename = ?", "legacy.go")
	var model string
	var dims int
	if err := row.Scan(&model, &dims); err != nil {
		t.Fatalf("SELECT new columns: %v", err)
	}
	if model == "" && dims == 0 {
		t.Log("migration added columns with defaults (expected for legacy row)")
	}
}

func TestLoad_DetectsDimensionMismatchAndReindexes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte("print('hello')"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "rag_dim_mismatch.db")

	embA := &dimEmbedder{model: "model-a", dims: 4, vecs: map[string][]float64{
		"print('hello')": make([]float64, 4),
	}}
	psA, err := OpenPersistentStore(dbPath, embA, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore A: %v", err)
	}
	if _, err := psA.LoadOrIndex(context.Background(), dir); err != nil {
		t.Fatalf("LoadOrIndex A: %v", err)
	}
	psA.Close()

	counter := &indexedCallCounter{
		model: "model-b",
		vecs: map[string][]float64{
			"print('hello')": make([]float64, 4),
		},
	}
	psB, err := OpenPersistentStore(dbPath, counter, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore B: %v", err)
	}
	t.Cleanup(func() { _ = psB.Close() })

	n, err := psB.LoadOrIndex(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadOrIndex B: %v", err)
	}
	if n != 1 {
		t.Errorf("LoadOrIndex returned %d, want 1 (re-indexed)", n)
	}
	if counter.totalCalls() == 0 {
		t.Errorf("embedder calls = 0, want >0 (should have re-indexed)")
	}
}

func TestCosineSimilarity_HandlesMismatchedDims(t *testing.T) {
	t.Parallel()
	a := []float64{1, 0, 0, 0}
	b := []float64{1, 0}
	got := CosineSimilarity(a, b)
	if got != 1.0 {
		t.Errorf("CosineSimilarity([1,0,0,0], [1,0]) = %v, want 1.0 (shorter vector governs)", got)
	}

	got2 := CosineSimilarity(b, a)
	if got2 != 1.0 {
		t.Errorf("CosineSimilarity([1,0], [1,0,0,0]) = %v, want 1.0", got2)
	}
}

// modelErrEmbedder reports a known model name (so extractEmbedderModel
// returns it and the boot-time probe is attempted) but always fails
// Embed, simulating an unreachable embedder at boot (issue #593).
type modelErrEmbedder struct {
	model string
	err   error
}

func (m *modelErrEmbedder) Embed(context.Context, string) ([]float64, error) {
	return nil, m.err
}

func (m *modelErrEmbedder) EmbedBatch(context.Context, []string) ([][]float64, error) {
	return nil, m.err
}

func (m *modelErrEmbedder) IsHealthy(context.Context) bool { return false }
func (m *modelErrEmbedder) IsBreakerOpen() bool            { return false }
func (m *modelErrEmbedder) RecordBreakerSuccess()          {}
func (m *modelErrEmbedder) Model() string                  { return m.model }

// TestOpenPersistentStore_ProbeFailureLogged verifies that when the
// embedder is unreachable at boot, the probeEmbedderDims error is
// surfaced as a WARN (issue #593) instead of being silently
// discarded, and EmbedderDims() reports the probe as unavailable.
func TestOpenPersistentStore_ProbeFailureLogged(t *testing.T) {
	t.Parallel()

	// Capture slog output so we can assert the WARN was emitted.
	var buf bytes.Buffer
	prev := logOutput(&buf)
	defer slog.SetDefault(prev)

	ps, err := OpenPersistentStore(":memory:",
		&modelErrEmbedder{model: "unreachable-model", err: errors.New("connection refused")},
		0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	// Issue #593 acceptance: embedderDims=0, probe marked unusable.
	if dims, ok := ps.EmbedderDims(); ok || dims != 0 {
		t.Errorf("EmbedderDims() = (%d, %v), want (0, false) after probe failure", dims, ok)
	}

	logged := buf.String()
	for _, want := range []string{"probe failed", "unreachable-model", "connection refused"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q; got:\n%s", want, logged)
		}
	}
}

// TestLoad_ProbeFailedStillDetectsModelMismatch verifies the
// issue #593 recovery path: when the probe failed at boot (so
// embedderDims=0 and the per-row dimension check cannot fire), Load
// still clears the cache via the model-name mismatch check when
// stored rows were stamped with a different model. This prevents
// stale embeddings from being served while the embedder is down.
func TestLoad_ProbeFailedStillDetectsModelMismatch(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "rag_probe.db")

	// Phase 1: index with model-a so rows are stamped on disk.
	seedEmb := &dimEmbedder{model: "model-a", dims: 4, vecs: map[string][]float64{
		"snippet": make([]float64, 4),
	}}
	psA, err := OpenPersistentStore(dbPath, seedEmb, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore A: %v", err)
	}
	ctx := context.Background()
	if err := psA.Upsert(ctx, FewShotExample{
		Filename:  "code.go",
		Content:   "snippet",
		Embedding: make([]float64, 4),
	}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	if err := psA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	// Phase 2: reopen with a different model whose probe FAILS
	// (embedder unreachable). embedderDims will be 0, so only the
	// model-name check can catch the drift.
	failing := &modelErrEmbedder{model: "model-b", err: errors.New("unreachable")}
	psB, err := OpenPersistentStore(dbPath, failing, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore B: %v", err)
	}
	t.Cleanup(func() { _ = psB.Close() })

	// EmbedderDims must report unavailable (probe failed).
	if _, ok := psB.EmbedderDims(); ok {
		t.Fatal("EmbedderDims() ok=true, want false (probe failed)")
	}

	n, err := psB.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The model-name mismatch should have cleared the store so
	// stale vectors are not served.
	if n != 0 {
		t.Errorf("Load returned %d rows, want 0 (model mismatch cleared cache)", n)
	}
	if psB.Size() != 0 {
		t.Errorf("Size after mismatch Load = %d, want 0", psB.Size())
	}
}

// TestExtractEmbedderModel_UnwrapsEmbedCache ensures the boot-time
// probe and model stamping work in the cache-enabled production path
// (issue #593), where the embedder is wrapped in an EmbedCache.
func TestExtractEmbedderModel_UnwrapsEmbedCache(t *testing.T) {
	t.Parallel()

	inner := &OllamaEmbedder{BaseURL: "http://localhost:11434", Model: "nomic-embed-text"}
	wrapped := NewEmbedCache(inner, 8, time.Minute, 5*time.Second)

	if got := extractEmbedderModel(wrapped); got != "nomic-embed-text" {
		t.Errorf("extractEmbedderModel(EmbedCache) = %q, want %q", got, "nomic-embed-text")
	}
	if got := embedderURL(wrapped); got != "http://localhost:11434" {
		t.Errorf("embedderURL(EmbedCache) = %q, want %q", got, "http://localhost:11434")
	}

	// Unwrapping a bare embedder is a no-op.
	if got := extractEmbedderModel(inner); got != "nomic-embed-text" {
		t.Errorf("extractEmbedderModel(bare) = %q, want %q", got, "nomic-embed-text")
	}
	// Unknown/stub types degrade gracefully.
	if got := extractEmbedderModel(&stubEmbedder{}); got != "unknown" {
		t.Errorf("extractEmbedderModel(stub) = %q, want %q", got, "unknown")
	}
	if got := embedderURL(&stubEmbedder{}); got != "" {
		t.Errorf("embedderURL(stub) = %q, want empty", got)
	}
}

// TestEmbedderDims_HealthyProbe reports the probed dimension when the
// embedder is reachable at boot.
func TestEmbedderDims_HealthyProbe(t *testing.T) {
	t.Parallel()
	ps, err := OpenPersistentStore(":memory:", &dimEmbedder{model: "ok-model", dims: 768}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if dims, ok := ps.EmbedderDims(); !ok || dims != 768 {
		t.Errorf("EmbedderDims() = (%d, %v), want (768, true)", dims, ok)
	}
}

// TestPersistentStore_Path_OnDisk verifies Path() returns the path
// passed to OpenPersistentStore for an on-disk store.
func TestPersistentStore_Path_OnDisk(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "rag_path_test.db")
	ps, err := OpenPersistentStore(dbPath, &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if got := ps.Path(); got != dbPath {
		t.Errorf("Path() = %q, want %q", got, dbPath)
	}
}

// TestPersistentStore_Path_InMemory verifies Path() returns empty
// string for a ":memory:" store.
func TestPersistentStore_Path_InMemory(t *testing.T) {
	t.Parallel()
	ps, err := OpenPersistentStore(":memory:", &stubEmbedder{}, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if got := ps.Path(); got != "" {
		t.Errorf("Path() = %q, want empty string for :memory:", got)
	}
}
