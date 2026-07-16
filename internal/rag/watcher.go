package rag

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileSnapshot is the per-file state the watcher remembers between
// scans. mtime + size together detect "file changed" reliably
// without a hash: cheap to compute, no extra reads, and catches the
// realistic case where a developer rewrites a snippet in place.
type fileSnapshot struct {
	name    string
	modTime time.Time
	size    int64
}

// Watcher polls an examples directory on a fixed cadence and
// reconciles the PersistentStore against the on-disk state. New
// files are embedded and upserted; modified files are re-embedded
// and replaced; deleted files are removed from the store.
//
// The polling approach is stdlib-only by design (no fsnotify
// dependency): the directory is low-cardinality (operator-curated
// snippets), so a 30s poll is cheap and the implementation stays
// inside the project's "stdlib-ish spirit" (AGENTS.md).
//
// A single goroutine owns the watcher loop and the `known` map, so
// no internal locking is required. The watcher interacts with the
// store through thread-safe methods (Upsert / Remove).
type Watcher struct {
	store    *PersistentStore
	dir      string
	interval time.Duration

	mu    sync.Mutex
	known map[string]fileSnapshot

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// NewWatcher constructs a Watcher; call Start to spawn the polling
// goroutine. The directory must already exist (the persistent
// store's LoadOrIndex creates it on first boot); if the directory
// is removed later the watcher logs and waits for it to reappear.
func NewWatcher(store *PersistentStore, dir string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Watcher{
		store:    store,
		dir:      dir,
		interval: interval,
		known:    make(map[string]fileSnapshot),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start launches the polling goroutine and returns immediately. The
// goroutine runs until Stop is called or parent is cancelled.
func (w *Watcher) Start(parent context.Context) {
	go w.run(parent)
}

// Stop signals the goroutine to exit and blocks until it has
// returned. Safe to call multiple times.
func (w *Watcher) Stop() {
	w.once.Do(func() {
		close(w.stopCh)
	})
	<-w.doneCh
}

// run is the single goroutine that owns the `known` map. It
// performs an initial scan to seed the state, then ticks on the
// configured interval. Errors are logged and the loop continues so
// a transient filesystem glitch doesn't kill the watcher.
func (w *Watcher) run(parent context.Context) {
	defer close(w.doneCh)

	if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("rag: initial scan failed",
			slog.String("component", "rag"),
			slog.String("dir", w.dir),
			slog.Any("err", err),
		)
	}

	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-parent.Done():
			return
		case <-w.stopCh:
			return
		case <-t.C:
			if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("rag: scan failed",
					slog.String("component", "rag"),
					slog.String("dir", w.dir),
					slog.Any("err", err),
				)
			}
		}
	}
}

// scanOnce reads the directory once, diffs against `known`, and
// applies the delta. Safe to call directly from tests.
//
// Security: symlinks are skipped (issue #107) to prevent confidentiality
// leaks via injected few-shot examples.
func (w *Watcher) scanOnce(ctx context.Context) error {
	files, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Directory was removed out from under us; log
			// and bail out — the next tick (or a future
			// Create) will reconcile.
			return nil
		}
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if isSymlink(f) {
			slog.Warn("rag: skipping symlink in examples dir (issue #107)",
				slog.String("component", "rag"),
				slog.String("filename", f.Name()),
				slog.String("dir", w.dir),
			)
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		name := f.Name()
		snap := fileSnapshot{name: name, modTime: info.ModTime(), size: info.Size()}
		seen[name] = struct{}{}

		prev, exists := w.known[name]
		if exists && prev.modTime.Equal(snap.modTime) && prev.size == snap.size {
			continue
		}

		if err := w.indexFile(ctx, name); err != nil {
			slog.Warn("rag: index failed",
				slog.String("component", "rag"),
				slog.String("filename", name),
				slog.Any("err", err),
			)
			continue // known still holds old snapshot → next poll retries
		}
		w.known[name] = snap // only update on success
	}

	// Detect deletions: anything in `known` that wasn't in `seen`
	// has been removed from the directory.
	for name := range w.known {
		if _, ok := seen[name]; ok {
			continue
		}
		if err := w.store.Remove(ctx, name); err != nil {
			slog.Warn("rag: remove failed",
				slog.String("component", "rag"),
				slog.String("filename", name),
				slog.Any("err", err),
			)
			continue
		}
		delete(w.known, name)
		slog.Info("rag: removed",
			slog.String("component", "rag"),
			slog.String("filename", name),
		)
	}
	return nil
}

// indexFile reads a single file, embeds its content, and upserts it
// into the persistent store. Pulled out so tests can exercise it
// without the goroutine.
func (w *Watcher) indexFile(ctx context.Context, name string) error {
	content, err := os.ReadFile(filepath.Join(w.dir, name))
	if err != nil {
		return err
	}
	emb, err := w.store.embedder.Embed(ctx, string(content))
	if err != nil {
		return err
	}
	return w.store.Upsert(ctx, FewShotExample{
		Filename:  name,
		Content:   string(content),
		Embedding: emb,
	})
}

// Seen is a test hook that returns the watcher's current view of
// the directory. Not part of the production API.
func (w *Watcher) Seen() map[string]fileSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]fileSnapshot, len(w.known))
	for k, v := range w.known {
		out[k] = v
	}
	return out
}
