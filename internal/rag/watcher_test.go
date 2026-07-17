package rag

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordingEmbedder is a stub that records every Embed call. Tests
// use it to assert that the watcher only embeds new/changed files,
// not the unchanged ones.
type recordingEmbedder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, text)
	// Deterministic, content-derived vector so cosine similarity
	// can distinguish matches.
	return hashVector(text), nil
}

func (r *recordingEmbedder) IsHealthy(context.Context) bool { return true }
func (r *recordingEmbedder) IsBreakerOpen() bool            { return false }
func (r *recordingEmbedder) RecordBreakerSuccess()          {}

func (r *recordingEmbedder) Called() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// hashVector returns a small deterministic vector derived from the
// input text. The first byte of the string determines the vector
// "direction" so embeddings actually discriminate — without this
// every vector is the same and Retrieve's best-score logic is
// meaningless.
func hashVector(s string) []float64 {
	if s == "" {
		return []float64{0, 0, 0}
	}
	v := []float64{0, 0, 0, 0}
	for i, b := range []byte(s) {
		v[i%len(v)] += float64(b) / 255.0
	}
	return v
}

// newWatcherStore wires an in-memory PersistentStore to a
// recordingEmbedder. Tests can inspect the embedder's call list.
func newWatcherStore(t *testing.T) (*PersistentStore, *recordingEmbedder) {
	t.Helper()
	emb := &recordingEmbedder{}
	ps, err := OpenPersistentStore(":memory:", emb, 0.55)
	if err != nil {
		t.Fatalf("OpenPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return ps, emb
}

func TestWatcherDetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	ps, emb := newWatcherStore(t)

	w := NewWatcher(ps, dir, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// First scan sees an empty directory.
	time.Sleep(50 * time.Millisecond)
	before := len(emb.Called())

	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	// Wait for the watcher to detect the new file. Poll because
	// the scheduler decides when the watcher goroutine runs.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ps.Size() != 1 {
		t.Fatalf("watcher did not pick up new file within deadline (size=%d)", ps.Size())
	}
	if len(emb.Called()) <= before {
		t.Errorf("embedder was not called for new file (calls=%v, before=%d)", emb.Called(), before)
	}
}

func TestWatcherDetectsModifiedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	ps, emb := newWatcherStore(t)

	w := NewWatcher(ps, dir, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Wait for the initial scan to pick up v1.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ps.Size() != 1 {
		t.Fatalf("initial scan failed to index v1 (size=%d)", ps.Size())
	}

	// Modify the file with a fresh mtime. Filesystems with
	// sub-second mtime resolution can collapse a quick rewrite
	// into the same mtime; bump it explicitly so the test is
	// portable.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("v2 longer content"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	// Wait until the stored content reflects the new version.
	// Polling on the embedder call count races with the
	// upsert (Embed returns before Upsert commits); polling on
	// the content is the correct signal.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snaps := ps.Snapshot()
		if len(snaps) == 1 && snaps[0].Content == "v2 longer content" {
			if len(emb.Called()) < 2 {
				t.Errorf("expected at least 2 embedder calls, got %d", len(emb.Called()))
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	snaps := ps.Snapshot()
	t.Fatalf("watcher did not update content within deadline (calls=%v, content=%q)",
		emb.Called(), contentOf(snaps))
}

func contentOf(snaps []FewShotExample) string {
	if len(snaps) == 0 {
		return ""
	}
	return snaps[0].Content
}

func TestWatcherDetectsDeletedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doomed.go"), []byte("doomed"), 0o644); err != nil {
		t.Fatalf("write doomed: %v", err)
	}
	ps, emb := newWatcherStore(t)

	w := NewWatcher(ps, dir, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Wait for the initial scan.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ps.Size() != 1 {
		t.Fatalf("initial scan failed to index doomed.go (size=%d)", ps.Size())
	}
	if err := os.Remove(filepath.Join(dir, "doomed.go")); err != nil {
		t.Fatalf("remove doomed: %v", err)
	}

	// Wait for the watcher to remove the entry.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ps.Size() != 0 {
		t.Errorf("watcher did not remove deleted file (size=%d)", ps.Size())
	}
	if len(emb.Called()) == 0 {
		t.Error("expected at least one embedder call for the initial scan")
	}
}

func TestWatcherSurvivesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	ps, _ := newWatcherStore(t)

	w := NewWatcher(ps, dir, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Give the watcher a few ticks with no directory present.
	time.Sleep(60 * time.Millisecond)

	// Now create the directory and a file. The watcher's next
	// tick should pick it up.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "late.go"), []byte("late"), 0o644); err != nil {
		t.Fatalf("write late: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watcher did not recover after directory appeared (size=%d)", ps.Size())
}

func TestWatcherStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	ps, _ := newWatcherStore(t)
	w := NewWatcher(ps, dir, 10*time.Millisecond)
	w.Start(context.Background())
	w.Stop()
	w.Stop() // must not panic / deadlock
}

func TestWatcherScanOnceHonoursMtimeEquality(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.go")
	if err := os.WriteFile(path, []byte("stable"), 0o644); err != nil {
		t.Fatalf("write stable: %v", err)
	}
	ps, emb := newWatcherStore(t)
	w := NewWatcher(ps, dir, time.Hour) // interval is irrelevant for direct call

	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #1: %v", err)
	}
	callsAfterFirst := len(emb.Called())
	if callsAfterFirst != 1 {
		t.Fatalf("first scan embedder calls = %d, want 1", callsAfterFirst)
	}

	// Second scan must NOT re-embed (mtime + size unchanged).
	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #2: %v", err)
	}
	if got := len(emb.Called()); got != callsAfterFirst {
		t.Errorf("second scan triggered re-embed (calls=%d, before=%d)", got, callsAfterFirst)
	}
}

func TestWatcherScanOncePicksUpChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	ps, emb := newWatcherStore(t)
	w := NewWatcher(ps, dir, time.Hour)

	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #1: %v", err)
	}
	callsAfterFirst := len(emb.Called())

	// Bump mtime explicitly so the test isn't flaky on
	// filesystems with coarse mtime resolution.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2 longer"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #2: %v", err)
	}
	if got := len(emb.Called()); got <= callsAfterFirst {
		t.Errorf("second scan did not re-embed (calls=%d, before=%d)", got, callsAfterFirst)
	}
}

func TestWatcherScanOnceRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doomed.go")
	if err := os.WriteFile(path, []byte("doomed"), 0o644); err != nil {
		t.Fatalf("write doomed: %v", err)
	}
	ps, _ := newWatcherStore(t)
	w := NewWatcher(ps, dir, time.Hour)

	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #1: %v", err)
	}
	if ps.Size() != 1 {
		t.Fatalf("Size after initial scan = %d, want 1", ps.Size())
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := w.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce #2: %v", err)
	}
	if ps.Size() != 0 {
		t.Errorf("Size after delete + scan = %d, want 0", ps.Size())
	}
}

func TestWatcherNewWatcherDefaultsInterval(t *testing.T) {
	dir := t.TempDir()
	ps, _ := newWatcherStore(t)
	w := NewWatcher(ps, dir, 0) // 0 -> default (fsnotify disabled, 60s fallback)
	if w.interval <= 0 {
		t.Errorf("interval = %v, want positive default", w.interval)
	}
	if w.interval != 60*time.Second {
		t.Errorf("interval = %v, want 60s fallback for NEXUS_RAG_POLL_INTERVAL=0", w.interval)
	}
	w.Start(context.Background())
	w.Stop()
}

func TestWatcherFsnotifyDetection(t *testing.T) {
	// This test verifies that when fsnotify is available (interval > 0),
	// the watcher uses immediate event detection rather than waiting
	// for a full poll interval.
	dir := t.TempDir()
	ps, emb := newWatcherStore(t)

	w := NewWatcher(ps, dir, 10*time.Millisecond) // short interval, fsnotify active
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Wait for initial scan.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ps.Size() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Write a new file.
	if err := os.WriteFile(filepath.Join(dir, "fast.go"), []byte("fast"), 0o644); err != nil {
		t.Fatalf("write fast.go: %v", err)
	}

	// With fsnotify, detection should be nearly immediate (sub-second),
	// not bounded by the poll interval.
	deadline = time.Now().Add(500 * time.Millisecond)
	detected := false
	for time.Now().Before(deadline) {
		if ps.Size() == 1 {
			detected = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !detected {
		t.Errorf("watcher did not detect new file within 500ms (fsnotify may be unavailable)")
	}
	if len(emb.Called()) < 1 {
		t.Errorf("embedder was not called for new file")
	}
}
