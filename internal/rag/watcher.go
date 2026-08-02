package rag

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
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

// Watcher monitors an examples directory for changes and reconciles
// the PersistentStore against the on-disk state. New files are
// embedded and upserted; modified files are re-embedded and replaced;
// deleted files are removed from the store.
//
// Primary detection uses github.com/fsnotify/fsnotify for sub-second
// notification. A fallback ticker fires when fsnotify is unavailable
// (e.g., network-mounted filesystems) or when NEXUS_RAG_POLL_INTERVAL=0
// is set explicitly.
//
// A single goroutine owns the watcher loop and the `known` map, so
// no internal locking is required. The watcher interacts with the
// store through thread-safe methods (Upsert / Remove).
type Watcher struct {
	store     *PersistentStore
	dir       string
	interval  time.Duration // fallback poll interval (fsnotify-unavailable cases)
	recursive bool          // walk subdirectories during scanOnce (issue #1149)

	mu    sync.Mutex
	known map[string]fileSnapshot

	stopCh       chan struct{}
	doneCh       chan struct{}
	once         sync.Once
	newWatcherFn func() (*fsnotify.Watcher, error) // injectable for tests
}

// NewWatcher constructs a Watcher; call Start to spawn the watcher
// goroutine. The directory must already exist (the persistent
// store's LoadOrIndex creates it on first boot); if the directory
// is removed later the watcher logs and waits for it to reappear.
//
// When interval > 0, fsnotify is used for immediate file-change
// detection with fallback polling at interval. When interval <= 0,
// fsnotify is disabled and a 60-second fallback poll is used (NFS
// / network-mount edge case).
func NewWatcher(store *PersistentStore, dir string, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 60 * time.Second // fallback poll for fsnotify-unavailable cases
	}
	return &Watcher{
		store:        store,
		dir:          dir,
		interval:     interval,
		known:        make(map[string]fileSnapshot),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		newWatcherFn: fsnotify.NewWatcher,
	}
}

// SetRecursive enables recursive subdirectory walking in scanOnce
// (issue #1149). When true, filepath.WalkDir descends into all
// subdirectories and file paths are stored relative to the root.
// Must be called before Start.
func (w *Watcher) SetRecursive(r bool) {
	w.recursive = r
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
// performs an initial scan to seed the state, then waits for
// fsnotify events and/or fallback ticker ticks. Errors are logged
// and the loop continues so a transient filesystem glitch doesn't
// kill the watcher.
func (w *Watcher) run(parent context.Context) {
	defer close(w.doneCh)

	if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("rag: initial scan failed",
			slog.String("component", "rag"),
			slog.String("dir", w.dir),
			slog.Any("err", err),
		)
	}

	// Attempt to open an fsnotify watcher. If it fails (e.g.,
	// network mount with no inotify support), fall back to polling-only.
	fw, err := w.newWatcherFn()
	if err != nil {
		slog.Warn("rag: fsnotify unavailable, using fallback polling",
			slog.String("component", "rag"),
			slog.String("dir", w.dir),
			slog.Any("err", err),
		)
		fw = nil // ensures no channel reads below
	} else {
		if err := fw.Add(w.dir); err != nil {
			slog.Warn("rag: fsnotify add failed, using fallback polling",
				slog.String("component", "rag"),
				slog.String("dir", w.dir),
				slog.Any("err", err),
			)
			_ = fw.Close()
			fw = nil
		} else {
			// Recursively add subdirectories so changes in nested
			// directories are detected by fsnotify (issue #1149).
			if w.recursive {
				w.addWatchSubdirs(fw)
			}
			// Capture the pointer in a local so the deferred func
			// always sees the original value, even after fw is
			// set to nil in the channel-closed degradation branch.
			fsNotifier := fw
			defer func() { _ = fsNotifier.Close() }()
		}
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		// When fw is nil (unavailable), both fw.Events and
		// fw.Errors are nil channels — reading from them would
		// block forever, so we guard those cases.
		if fw == nil {
			// Polling-only path.
			select {
			case <-parent.Done():
				return
			case <-w.stopCh:
				return
			case <-ticker.C:
				if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("rag: scan failed",
						slog.String("component", "rag"),
						slog.String("dir", w.dir),
						slog.Any("err", err),
					)
				}
			}
			continue
		}

		// fsnotify + fallback ticker path.
		select {
		case <-parent.Done():
			return
		case <-w.stopCh:
			return
		case evt, ok := <-fw.Events:
			if !ok {
				// Watcher closed; degrade to polling.
				_ = fw.Close()
				fw = nil
				slog.Warn("rag: fsnotify watcher closed, degrading to polling",
					slog.String("component", "rag"),
					slog.String("dir", w.dir),
				)
				continue
			}
			// macOS FSEvents buffer overflow fires a Remove event
			// with an empty name; reconcile by scanning.
			if evt.Has(fsnotify.Remove) || evt.Has(fsnotify.Rename) {
				if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("rag: scan failed",
						slog.String("component", "rag"),
						slog.String("dir", w.dir),
						slog.Any("err", err),
					)
				}
			} else if evt.Has(fsnotify.Write) || evt.Has(fsnotify.Create) {
				// In recursive mode, add newly-created subdirectories
				// to the fsnotify watcher so future events inside them
				// are detected (issue #1149).
				if w.recursive && evt.Has(fsnotify.Create) {
					if info, statErr := os.Stat(evt.Name); statErr == nil && info.IsDir() {
						_ = fw.Add(evt.Name)
					}
				}
				if err := w.scanOnce(parent); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("rag: scan failed",
						slog.String("component", "rag"),
						slog.String("dir", w.dir),
						slog.Any("err", err),
					)
				}
			}
		case err, ok := <-fw.Errors:
			if !ok {
				// Watcher closed; degrade to polling.
				_ = fw.Close()
				fw = nil
				slog.Warn("rag: fsnotify watcher closed, degrading to polling",
					slog.String("component", "rag"),
					slog.String("dir", w.dir),
				)
				continue
			}
			slog.Warn("rag: fsnotify error",
				slog.String("component", "rag"),
				slog.String("dir", w.dir),
				slog.Any("err", err),
			)
			// Continue to ticker to keep reconciling.
		case <-ticker.C:
			// Fallback ticker: reconciles state periodically and
			// handles buffer-overflow gaps on macOS FSEvents.
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
// When w.recursive is true, filepath.WalkDir descends into all
// subdirectories and file paths are stored relative to the root
// (issue #1149).
//
// Security: symlinks are skipped (issue #107) to prevent confidentiality
// leaks via injected few-shot examples.
func (w *Watcher) scanOnce(ctx context.Context) error {
	if _, err := os.Stat(w.dir); err != nil && os.IsNotExist(err) {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	seen := make(map[string]struct{})

	if w.recursive {
		err := filepath.WalkDir(w.dir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if isSymlink(d) {
					return filepath.SkipDir
				}
				return nil
			}
			if isSymlink(d) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(w.dir, path)
			if err != nil {
				return nil
			}
			name := filepath.ToSlash(rel)
			snap := fileSnapshot{name: name, modTime: info.ModTime(), size: info.Size()}
			seen[name] = struct{}{}

			prev, exists := w.known[name]
			if exists && prev.modTime.Equal(snap.modTime) && prev.size == snap.size {
				return nil
			}

			if err := w.indexFile(ctx, name); err != nil {
				slog.Warn("rag: index failed",
					slog.String("component", "rag"),
					slog.String("filename", name),
					slog.Any("err", err),
				)
				return nil
			}
			w.known[name] = snap
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		files, err := os.ReadDir(w.dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
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
// into the persistent store. name is either a bare filename (flat
// mode) or a forward-slash relative path like "sub/deep.go" (recursive
// mode). Pulled out so tests can exercise it without the goroutine.
func (w *Watcher) indexFile(ctx context.Context, name string) error {
	content, err := os.ReadFile(filepath.Join(w.dir, name))
	if err != nil {
		return err
	}
	emb, err := w.store.embedder.Embed(ctx, string(content))
	if err != nil {
		return err
	}
	ex := FewShotExample{
		Filename:  name,
		Content:   string(content),
		Embedding: emb,
	}
	if w.recursive {
		parent := filepath.ToSlash(filepath.Dir(name))
		if parent != "." {
			ex.Dir = parent
		}
	}
	return w.store.Upsert(ctx, ex)
}

// addWatchSubdirs recursively adds all subdirectories under w.dir to
// the fsnotify watcher so events in nested directories are detected
// (issue #1149). Errors are logged and skipped — a missing subdirectory
// watch degrades gracefully to the fallback ticker.
func (w *Watcher) addWatchSubdirs(fw *fsnotify.Watcher) {
	_ = filepath.WalkDir(w.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != w.dir {
			if isSymlink(d) {
				return filepath.SkipDir
			}
			if addErr := fw.Add(path); addErr != nil {
				slog.Debug("rag: fsnotify add subdir skipped",
					slog.String("component", "rag"),
					slog.String("dir", path),
					slog.Any("err", addErr),
				)
			}
		}
		return nil
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
