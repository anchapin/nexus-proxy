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

	sqlite3 "modernc.org/sqlite/lib"
)

// ragSchema is the v2 schema for the few-shot cache table. One row
// per indexed file; filename is the natural primary key because the
// file watcher (issue #46) addresses rows by basename. The embedding
// blob is a gob-encoded []float64 — no third-party serialization
// dependency needed.
//
// indexed_at is informational (helps operators see when a row was
// last refreshed); the authoritative freshness signal is the file's
// mtime, which the watcher compares against the directory listing.
//
// embedder_model and dims (issue #536) stamp each row with the
// embedder model name and vector dimension at UPSERT time so Load
// can detect a changed embedder and refuse to serve stale vectors.
const ragSchema = `
CREATE TABLE IF NOT EXISTS rag_examples (
    filename TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    embedding BLOB NOT NULL,
    indexed_at DATETIME NOT NULL,
    embedder_model TEXT NOT NULL DEFAULT '',
    dims INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_rag_indexed_at ON rag_examples(indexed_at);
`

const ragUpsertSQL = `INSERT INTO rag_examples
    (filename, content, embedding, indexed_at, embedder_model, dims)
    VALUES (?, ?, ?, ?, ?, ?)
    ON CONFLICT(filename) DO UPDATE SET
        content = excluded.content,
        embedding = excluded.embedding,
        indexed_at = excluded.indexed_at,
        embedder_model = excluded.embedder_model,
        dims = excluded.dims`

const ragDeleteSQL = `DELETE FROM rag_examples WHERE filename = ?`

const ragSelectAllSQL = `SELECT filename, content, embedding, indexed_at, embedder_model, dims
    FROM rag_examples ORDER BY filename`

// ragOpTimeout bounds a single DB op. The table is small and the
// read is one-shot on boot, so the timeout only guards a
// pathological disk stall.
const ragOpTimeout = 5 * time.Second

const ragSchemaVersionSQL = `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`

const currentSchemaVersion = 2

var ragMigrations = []string{
	`ALTER TABLE rag_examples ADD COLUMN embedder_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE rag_examples ADD COLUMN dims INTEGER NOT NULL DEFAULT 0`,
}

// wrapCorruptErr wraps sqlite3 errors with a descriptive message when the
// error code indicates database corruption (SQLITE_CORRUPT, SQLITE_NOTADB) or
// an interrupted operation (SQLITE_INTERRUPT), guiding operators toward the
// correct recovery action instead of an opaque error.
func wrapCorruptErr(ctx string, err error) error {
	var sqErr interface{ Code() int }
	if errors.As(err, &sqErr) {
		switch sqErr.Code() {
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_INTERRUPT:
			return fmt.Errorf("%s: database may be corrupted — backup and re-index: %w", ctx, err)
		}
	}
	return fmt.Errorf("%s: %w", ctx, err)
}

func runRAGMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ragSchemaVersionSQL); err != nil {
		return fmt.Errorf("rag: create schema_version table: %w", err)
	}

	var version int
	err := db.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err == sql.ErrNoRows {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rag_examples").Scan(&count); err != nil {
			version = currentSchemaVersion
		} else if count == 0 {
			version = currentSchemaVersion
		} else {
			version = 0
		}
		// Use INSERT OR IGNORE: if another concurrent init already inserted the row
		// (e.g., parallel t.Parallel() tests each opening their own :memory: connection
		// share the same in-process DB), this is not an error.
		if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO schema_version (version) VALUES (?)", version); err != nil {
			return fmt.Errorf("rag: init schema version: %w", err)
		}
		if version == 0 {
			goto runMigrations
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("rag: read schema version: %w", err)
	}

	if version > currentSchemaVersion {
		return fmt.Errorf("rag: schema_version is at %d, which is higher than currentSchemaVersion %d: database may be corrupted or was created by a newer version; backup and re-index", version, currentSchemaVersion)
	}

	if version >= currentSchemaVersion {
		return nil
	}

runMigrations:

	if version >= currentSchemaVersion {
		return nil
	}

	for i := version; i < currentSchemaVersion; i++ {
		migration := ragMigrations[i]
		const maxRetries = 3
		var migrationErr error
	retry:
		for attempt := 0; attempt < maxRetries; attempt++ {
			_, err := db.ExecContext(ctx, migration)
			if err == nil {
				break retry
			}
			migrationErr = err
			var sqErr interface{ Code() int }
			if errors.As(err, &sqErr) {
				switch sqErr.Code() {
				case sqlite3.SQLITE_BUSY:
					time.Sleep(time.Millisecond * 100 * time.Duration(attempt+1))
					continue
				case sqlite3.SQLITE_FULL:
					return fmt.Errorf("rag: disk full during migration (free up disk space and retry): %w", err)
				case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_INTERRUPT:
					return fmt.Errorf("rag: database may be corrupted — backup and re-index: %w", err)
				}
			}
			return fmt.Errorf("rag: migrate: %w", err)
		}
		if migrationErr != nil {
			return fmt.Errorf("rag: migrate: SQLITE_BUSY exceeded max retries for: %s", migration)
		}
		version = i + 1
		if _, err := db.ExecContext(ctx, "UPDATE schema_version SET version = ?", version); err != nil {
			return fmt.Errorf("rag: record schema version: %w", err)
		}
	}
	return nil
}

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

	closeOnce     sync.Once
	closeErr      error
	embedderModel string
	embedderDims  int
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
//
// opts are applied to the embedded Store (e.g. WithBatchSize to
// control batch embedding in IndexDir).
func OpenPersistentStore(path string, embedder Embedder, threshold float64, opts ...StoreOption) (*PersistentStore, error) {
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
		return nil, wrapCorruptErr(fmt.Sprintf("rag: ping %q", path), err)
	}
	// Tighten permissions on the SQLite DB file so an upgrade from a
	// pre-fix binary locks it down (issue #108).
	if path != ":memory:" {
		chmodIfWider(path, 0o600)
	}
	if _, err := db.ExecContext(context.Background(), ragSchema); err != nil {
		_ = db.Close()
		return nil, wrapCorruptErr("rag: create schema", err)
	}
	if err := runRAGMigrations(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rag: run migrations: %w", err)
	}

	embedderModel := extractEmbedderModel(embedder)
	var embedderDims int
	if embedderModel != "" && embedderModel != "unknown" {
		dims, probeErr := probeEmbedderDims(context.Background(), embedder)
		embedderDims = dims
		if probeErr != nil {
			// Issue #593: surface the probe failure instead of
			// silently discarding it. embedderDims stays 0, so the
			// per-row dimension check in Load cannot fire — Load
			// falls back to model-name-based mismatch detection
			// (issue #536) to avoid serving stale vectors. The WARN
			// tells operators dimension validation is degraded.
			slog.Warn("rag: embedder dimension probe failed at boot; dimension validation degraded to model-name check",
				slog.String("embedder", embedderURL(embedder)),
				slog.String("model", embedderModel),
				slog.Any("err", probeErr),
			)
		}
	}

	storePath := path
	if path == ":memory:" {
		storePath = ""
	}
	return &PersistentStore{
		Store:         NewStore(embedder, threshold, opts...),
		db:            db,
		path:          storePath,
		embedderModel: embedderModel,
		embedderDims:  embedderDims,
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

// EmbedderDims reports the embedder vector dimension probed at boot
// and whether it is usable for per-row validation. Returns 0,false
// when the probe failed (embedder unreachable at boot) or the
// embedder model is unknown (issue #593). Operators can inspect this
// via /status to detect when dimension validation was skipped; in
// that state Load falls back to model-name-based mismatch detection.
func (p *PersistentStore) EmbedderDims() (int, bool) {
	if p == nil {
		return 0, false
	}
	return p.embedderDims, p.embedderDims > 0
}

// Load reads every row from the DB and replaces the in-memory
// examples slice in a single atomic swap. Returns the number of rows
// loaded. Ollama is not contacted — this is the headline win for
// boot time on a populated cache.
//
// Mismatch detection (issue #536): if any stored row's embedder_model
// or dims differs from the currently-configured embedder, the row is
// treated as stale and the entire in-memory store is cleared. Callers
// (LoadOrIndex) should treat n=0 after Load as a signal to re-index.
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
	var lastIndexedAt time.Time
	var mismatch bool
	for rows.Next() {
		var (
			name        string
			content     string
			embBlob     []byte
			indexedAt   time.Time
			storedModel string
			storedDims  int
		)
		if err := rows.Scan(&name, &content, &embBlob, &indexedAt, &storedModel, &storedDims); err != nil {
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
		if p.embedderModel != "" && p.embedderModel != "unknown" && storedModel != "" && storedModel != "unknown" && storedModel != p.embedderModel {
			slog.Warn("rag: embedder model changed, clearing stale cache",
				slog.String("filename", name),
				slog.String("stored_model", storedModel),
				slog.String("current_model", p.embedderModel),
			)
			mismatch = true
			break
		}
		if p.embedderModel != "" && p.embedderModel != "unknown" && storedModel != "" && storedModel != "unknown" && p.embedderDims > 0 && storedDims > 0 && storedDims != p.embedderDims {
			slog.Warn("rag: embedding dimension mismatch, clearing stale cache",
				slog.String("filename", name),
				slog.Int("stored_dims", storedDims),
				slog.Int("current_dims", p.embedderDims),
				slog.String("current_model", p.embedderModel),
			)
			mismatch = true
			break
		}
		out = append(out, FewShotExample{
			Filename:  name,
			Content:   content,
			Embedding: emb,
		})
		if indexedAt.After(lastIndexedAt) {
			lastIndexedAt = indexedAt
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rag: iterate: %w", err)
	}
	slog.Debug("rag: Load done", slog.Int("out_len", len(out)), slog.Bool("mismatch", mismatch))
	if mismatch {
		p.replace(nil)
		return 0, nil
	}
	p.replace(out)
	if !lastIndexedAt.IsZero() {
		p.markIndexed(lastIndexedAt)
	}
	return len(out), nil
}

// LoadOrIndex implements the boot path described in the issue: try
// Load first; if the DB has zero rows, index the directory (which
// embeds each file via Ollama and persists it via Upsert). Returns
// the number of examples in memory after the operation.
//
// When Load detects a model/dimension mismatch (issue #536) it clears
// the in-memory store and returns n=0, which triggers the IndexDir
// fallthrough so stale embeddings are never served.
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
// When the embedded Store has batchSize > 0, files are partitioned
// into batches and embedded via EmbedBatch (matching Store.IndexDir's
// logic). When batchSize == 0, each file is embedded individually via
// Embed.
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
			slog.Error("rag read file",
				slog.String("filename", f.Name()),
				slog.Any("err", err),
			)
			continue
		}
		validFiles = append(validFiles, fileInfo{name: f.Name(), content: string(content)})
	}

	if p.Store.batchSize > 0 && len(validFiles) > 0 {
		for i := 0; i < len(validFiles); i += p.Store.batchSize {
			end := i + p.Store.batchSize
			if end > len(validFiles) {
				end = len(validFiles)
			}
			batch := validFiles[i:end]
			texts := make([]string, len(batch))
			for j, fi := range batch {
				texts[j] = fi.content
			}
			embs, err := p.embedder.EmbedBatch(ctx, texts)
			if err != nil {
				slog.Error("rag embed batch", slog.Any("err", err))
				continue
			}
			for j, fi := range batch {
				if err := p.Upsert(ctx, FewShotExample{
					Filename:  fi.name,
					Content:   fi.content,
					Embedding: embs[j],
				}); err != nil {
					slog.Error("rag persist file",
						slog.String("filename", fi.name),
						slog.Any("err", err),
					)
					continue
				}
				slog.Info("rag indexed", slog.String("filename", fi.name))
			}
		}
	} else {
		for _, fi := range validFiles {
			emb, err := p.embedder.Embed(ctx, fi.content)
			if err != nil {
				slog.Error("rag embed file",
					slog.String("filename", fi.name),
					slog.Any("err", err),
				)
				continue
			}
			if err := p.Upsert(ctx, FewShotExample{
				Filename:  fi.name,
				Content:   fi.content,
				Embedding: emb,
			}); err != nil {
				slog.Error("rag persist file",
					slog.String("filename", fi.name),
					slog.Any("err", err),
				)
				continue
			}
			slog.Info("rag indexed", slog.String("filename", fi.name))
		}
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

	indexedAt := time.Now().UTC()
	if _, err := p.db.ExecContext(cctx, ragUpsertSQL,
		ex.Filename, ex.Content, blob, indexedAt, p.embedderModel, len(ex.Embedding),
	); err != nil {
		return fmt.Errorf("rag: upsert %q: %w", ex.Filename, err)
	}
	p.upsertExample(ex)
	p.markIndexed(indexedAt)
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

// extractEmbedderModel returns the model name from the embedder via
// type assertion. Returns "unknown" when the concrete type is unknown
// (test stubs). This is safe to call on nil embedders.
//
// EmbedCache (issue #115/#303) is unwrapped first so model extraction
// — and therefore the boot-time dimension probe — works in the common
// cache-enabled configuration (issue #593).
func extractEmbedderModel(embedder Embedder) string {
	embedder = unwrapEmbedder(embedder)
	if embedder == nil {
		return "unknown"
	}
	switch e := embedder.(type) {
	case *OllamaEmbedder:
		return e.Model
	case *OpenAIEmbedder:
		return e.Model
	case *CohereEmbedder:
		return e.Model
	case interface{ Model() string }:
		return e.Model()
	default:
		return "unknown"
	}
}

// embedderURL returns the base URL of the embedder for diagnostic
// logging (issue #593). Returns "" for stubs/unknown types.
func embedderURL(embedder Embedder) string {
	embedder = unwrapEmbedder(embedder)
	if embedder == nil {
		return ""
	}
	switch e := embedder.(type) {
	case *OllamaEmbedder:
		return e.BaseURL
	case *OpenAIEmbedder:
		return e.BaseURL
	case *CohereEmbedder:
		return e.BaseURL
	case interface{ URL() string }:
		return e.URL()
	default:
		return ""
	}
}

// unwrapEmbedder peels off EmbedCache (and any other wrapper that
// exposes an Unwrap method) so introspection reaches the concrete
// provider.
func unwrapEmbedder(embedder Embedder) Embedder {
	for embedder != nil {
		if c, ok := embedder.(*EmbedCache); ok {
			embedder = c.inner
			continue
		}
		if u, ok := embedder.(interface{ Unwrap() Embedder }); ok {
			embedder = u.Unwrap()
			continue
		}
		break
	}
	return embedder
}

// probeEmbedderDims calls Embed with a probe string and returns the
// resulting vector length. Returns 0 and nil error if the embedder
// cannot be probed (e.g., stub in tests or unreachable server).
// The probe result is not cached — callers only call this once at boot.
func probeEmbedderDims(ctx context.Context, embedder Embedder) (int, error) {
	if embedder == nil {
		return 0, nil
	}
	vec, err := embedder.Embed(ctx, "nexus rag dimension probe")
	if err != nil || len(vec) == 0 {
		return 0, err
	}
	return len(vec), nil
}
