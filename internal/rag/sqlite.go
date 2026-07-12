package rag

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	// Pure-Go SQLite driver (no CGo). The same driver backs
	// internal/metrics and internal/router/confidence_sqlite;
	// importing it here registers the "sqlite" driver name. See
	// modernc.org/sqlite.
	_ "modernc.org/sqlite"
)

// ragSchema is the v1 schema for the few-shot cache table. One row
// per indexed file; filename is the natural primary key because the
// file watcher (issue #46) addresses rows by basename. The embedding
// blob is a gob-encoded []float64 — no third-party serialization
// dependency needed.
//
// indexed_at is informational (helps operators see when a row was
// last refreshed); the authoritative freshness signal is the file's
// mtime, which the watcher compares against the directory listing.
const ragSchema = `
CREATE TABLE IF NOT EXISTS rag_examples (
    filename TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    embedding BLOB NOT NULL,
    indexed_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rag_indexed_at ON rag_examples(indexed_at);
`

const ragUpsertSQL = `INSERT INTO rag_examples
    (filename, content, embedding, indexed_at)
    VALUES (?, ?, ?, ?)
    ON CONFLICT(filename) DO UPDATE SET
        content = excluded.content,
        embedding = excluded.embedding,
        indexed_at = excluded.indexed_at`

const ragDeleteSQL = `DELETE FROM rag_examples WHERE filename = ?`

const ragSelectAllSQL = `SELECT filename, content, embedding, indexed_at
    FROM rag_examples ORDER BY filename`

// ragOpTimeout bounds a single DB op. The table is small and the
// read is one-shot on boot, so the timeout only guards a
// pathological disk stall.
const ragOpTimeout = 5 * time.Second

// PersistentStore is the SQLite-backed RAG store (issue #46). It
// embeds *Store so the public retrieval API (Retrieve / Add / Size
// / Threshold) is identical to the in-memory implementation and the
// chat handler doesn't need to know which one it's talking to.
//
// Persistence is transparent to callers: the boot path calls Load
// (or LoadOrIndex on a fresh DB) and the optional Watcher keeps the
// table in sync with the examples directory at runtime. Upsert and
// Remove update both the DB and the in-memory slice atomically with
// respect to Retrieve.
type PersistentStore struct {
	*Store
	db   *sql.DB
	path string

	closeOnce sync.Once
	closeErr  error
}

// OpenPersistentStore opens (creating on demand) the SQLite database
// at path and returns a ready PersistentStore backed by an empty
// in-memory *Store. The parent directory is created for on-disk
// paths. ":memory:" is supported for tests. An empty path is
// rejected.
//
// The returned store has zero examples — callers should follow up
// with Load (or LoadOrIndex) before serving traffic so the in-memory
// slice reflects what's already on disk.
func OpenPersistentStore(path string, embedder Embedder, threshold float64) (*PersistentStore, error) {
	if path == "" {
		return nil, errors.New("rag: empty persistent db path")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("rag: mkdir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", ragDSN(path))
	if err != nil {
		return nil, fmt.Errorf("rag: open %q: %w", path, err)
	}
	// Single writer keeps SQLite happy; the read path still proceeds
	// concurrently thanks to WAL. Mirrors the metrics and confidence
	// stores so a single mental model applies across the codebase.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rag: ping %q: %w", path, err)
	}
	// Tighten permissions on the SQLite DB file so an upgrade from a
	// pre-fix binary locks it down (issue #108).
	if path != ":memory:" {
		chmodIfWider(path, 0o600)
	}
	if _, err := db.ExecContext(context.Background(), ragSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rag: create schema: %w", err)
	}

	return &PersistentStore{
		Store: NewStore(embedder, threshold),
		db:    db,
		path:  path,
	}, nil
}

// ragDSN mirrors the metrics store's DSN: WAL journalling for
// concurrent readers, a bounded busy timeout, and NORMAL synchronous
// mode (this is a cache, not durable accounting).
func ragDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:?mode=memory&cache=shared"
	}
	return fmt.Sprintf(
		"file:%s?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		path,
	)
}

// Path returns the on-disk path the store was opened with. Empty for
// ":memory:" stores.
func (p *PersistentStore) Path() string { return p.path }

// Load reads every row from the DB and replaces the in-memory
// examples slice in a single atomic swap. Returns the number of rows
// loaded. Ollama is not contacted — this is the headline win for
// boot time on a populated cache.
//
// Callers should treat a Load error as fatal for the persistence
// path (return to caller; main.go falls back to re-indexing) but the
// store itself remains usable as an in-memory cache.
func (p *PersistentStore) Load(ctx context.Context) (int, error) {
	if p == nil || p.db == nil {
		return 0, errors.New("rag: persistent store not opened")
	}
	cctx, cancel := context.WithTimeout(ctx, ragOpTimeout)
	defer cancel()

	rows, err := p.db.QueryContext(cctx, ragSelectAllSQL)
	if err != nil {
		return 0, fmt.Errorf("rag: select all: %w", err)
	}
	defer rows.Close()

	out := make([]FewShotExample, 0, 64)
	for rows.Next() {
		var (
			name      string
			content   string
			embBlob   []byte
			indexedAt time.Time
		)
		if err := rows.Scan(&name, &content, &embBlob, &indexedAt); err != nil {
			return 0, fmt.Errorf("rag: scan %q: %w", name, err)
		}
		emb, err := decodeEmbedding(embBlob)
		if err != nil {
			slog.Warn("rag: corrupt embedding, skipping",
				slog.String("filename", name),
				slog.Any("err", err),
			)
			continue
		}
		out = append(out, FewShotExample{
			Filename:  name,
			Content:   content,
			Embedding: emb,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rag: iterate: %w", err)
	}
	p.replace(out)
	return len(out), nil
}

// LoadOrIndex implements the boot path described in the issue: try
// Load first; if the DB has zero rows, index the directory (which
// embeds each file via Ollama and persists it via Upsert). Returns
// the number of examples in memory after the operation.
func (p *PersistentStore) LoadOrIndex(ctx context.Context, dir string) (int, error) {
	n, err := p.Load(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return n, nil
	}
	if err := p.IndexDir(ctx, dir); err != nil {
		return 0, err
	}
	return p.Size(), nil
}

// IndexDir walks dir, embedding every regular file's contents and
// persisting each result via Upsert. Mirrors Store.IndexDir's
// permissive error handling — a missing directory is created, per-
// file read/embed errors are logged and skipped — but every
// successful embedding also lands in SQLite so the next boot can
// skip Ollama entirely.
//
// Security: symlinks are skipped (issue #107) to prevent confidentiality
// leaks via injected few-shot examples.
func (p *PersistentStore) IndexDir(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
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
			slog.Error("rag read file",
				slog.String("filename", f.Name()),
				slog.Any("err", err),
			)
			continue
		}
		emb, err := p.embedder.Embed(ctx, string(content))
		if err != nil {
			slog.Error("rag embed file",
				slog.String("filename", f.Name()),
				slog.Any("err", err),
			)
			continue
		}
		if err := p.Upsert(ctx, FewShotExample{
			Filename:  f.Name(),
			Content:   string(content),
			Embedding: emb,
		}); err != nil {
			slog.Error("rag persist file",
				slog.String("filename", f.Name()),
				slog.Any("err", err),
			)
			continue
		}
		slog.Info("rag indexed", slog.String("filename", f.Name()))
	}
	return nil
}

// Upsert writes a single example to SQLite and updates the in-memory
// slice in lock-step so Retrieve sees the change immediately. Safe to
// call from the file watcher goroutine and from boot-time
// IndexDir concurrently — the in-memory write holds the store's
// RWMutex and the DB write is serialised by SetMaxOpenConns(1).
func (p *PersistentStore) Upsert(ctx context.Context, ex FewShotExample) error {
	if p == nil || p.db == nil {
		return errors.New("rag: persistent store not opened")
	}
	if ex.Filename == "" {
		return errors.New("rag: empty filename")
	}
	blob, err := encodeEmbedding(ex.Embedding)
	if err != nil {
		return fmt.Errorf("rag: encode embedding %q: %w", ex.Filename, err)
	}

	cctx, cancel := context.WithTimeout(ctx, ragOpTimeout)
	defer cancel()

	if _, err := p.db.ExecContext(cctx, ragUpsertSQL,
		ex.Filename, ex.Content, blob, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("rag: upsert %q: %w", ex.Filename, err)
	}
	p.upsertExample(ex)
	return nil
}

// Remove deletes a single example by filename from both the DB and
// the in-memory slice. Missing rows are not an error (idempotent,
// which matters for the watcher reconciling deletes against a
// restart).
func (p *PersistentStore) Remove(ctx context.Context, filename string) error {
	if p == nil || p.db == nil {
		return errors.New("rag: persistent store not opened")
	}
	if filename == "" {
		return errors.New("rag: empty filename")
	}

	cctx, cancel := context.WithTimeout(ctx, ragOpTimeout)
	defer cancel()

	if _, err := p.db.ExecContext(cctx, ragDeleteSQL, filename); err != nil {
		return fmt.Errorf("rag: delete %q: %w", filename, err)
	}
	p.removeExample(filename)
	return nil
}

// Snapshot returns a defensive copy of the in-memory examples. The
// file watcher uses this to compute diffs without holding the lock
// across an Embed round trip.
func (p *PersistentStore) Snapshot() []FewShotExample { return p.snapshot() }

// Close releases the underlying database handle. Safe to call
// multiple times.
func (p *PersistentStore) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.db != nil {
			p.closeErr = p.db.Close()
		}
	})
	return p.closeErr
}

// encodeEmbedding serialises a float slice with encoding/gob. Gob is
// stdlib and round-trips []float64 losslessly; no third-party
// serialisation library is required.
func encodeEmbedding(v []float64) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeEmbedding reverses encodeEmbedding. A nil/empty blob yields
// a nil embedding (the caller treats that as a corrupt row and
// skips).
func decodeEmbedding(b []byte) ([]float64, error) {
	if len(b) == 0 {
		return nil, errors.New("empty embedding blob")
	}
	var v []float64
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// chmodIfWider checks the current mode of path and, if any
// group/other bits are set, tightens the file to the requested mode.
// Errors are logged — chmod failures are non-fatal.
func chmodIfWider(path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("rag: stat for chmod",
			slog.String("path", path),
			slog.Any("err", err),
		)
		return
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return
	}
	slog.Warn("rag: tightening file permissions",
		slog.String("path", path),
		slog.Int("was", int(perm)),
		slog.Int("now", int(mode)),
	)
	if err := os.Chmod(path, mode); err != nil {
		slog.Warn("rag: chmod failed",
			slog.String("path", path),
			slog.Any("err", err),
		)
	}
}
