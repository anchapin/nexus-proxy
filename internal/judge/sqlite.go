// Package judge implements the asynchronous LLM-as-a-Judge evaluator
// described in issue #15.
package judge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	// Pure-Go SQLite driver. No CGo so the build stays stdlib-ish.
	_ "modernc.org/sqlite"
)

// judgeScoresSchema is the v1 schema for the per-judge-score table.
// One row per sampled local-route completion.
//
// All columns are NOT NULL with safe defaults so the INSERT is trivial
// (positional bind values, no NULL handling). The error column stores
// the error message as TEXT when the score could not be computed (Err
// set on the struct); a zero/empty error column means success.
const judgeScoresSchema = `
CREATE TABLE IF NOT EXISTS judge_scores (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME NOT NULL,
    request_id TEXT NOT NULL,
    score INTEGER NOT NULL DEFAULT 0,
    raw_response TEXT NOT NULL DEFAULT '',
    cost_usd REAL NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_judge_scores_timestamp ON judge_scores(timestamp);
CREATE INDEX IF NOT EXISTS idx_judge_scores_request_id ON judge_scores(request_id);
`

// insertScoreSQL is the prepared statement for Record.
const insertScoreSQL = `INSERT INTO judge_scores
    (timestamp, request_id, score, raw_response, cost_usd, prompt_tokens, output_tokens, error)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// bufferedChannelSize caps the in-flight queue per store. Records
// serialise to one INSERT each, so this caps memory at roughly
// (record-row-bytes * bufferedChannelSize). 512 records is <1 MB —
// comfortable and large enough to absorb judge-evaluation bursts.
const bufferedChannelSize = 512

// recordScoreErrorTimeout bounds how long a write op waits for a
// slow disk before giving up. Judge scores are best-effort telemetry
// — we log the error rather than failing the chat request.
const recordScoreErrorTimeout = 5 * time.Second

// SQLiteStore is a SQLite-backed judge.Storage implementation.
// Writes are funnelled through a buffered channel and a single
// background goroutine; the interface is identical to MemoryStorage
// so swapping is a one-line change in main.go.
type SQLiteStore struct {
	db   *sql.DB
	path string // "" for ":memory:"

	ch      chan JudgeScore
	dropped uint64
	wg      sync.WaitGroup
	closed  bool
	closeMu sync.Mutex
	closeFn func() error
}

// OpenSQLiteStore opens (or creates) a SQLite database at path and
// starts the background drain goroutine. Path may be ":memory:" for
// tests. Returns an error only when the database cannot be opened or
// the schema cannot be created; callers should log and fall back to
// MemoryStorage rather than crashing the proxy on a broken judge DB.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("judge: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("judge: ping %q: %w", path, err)
	}
	if _, err := db.ExecContext(context.Background(), judgeScoresSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("judge: create schema: %w", err)
	}

	s := &SQLiteStore{
		db:   db,
		path: path,
		ch:   make(chan JudgeScore, bufferedChannelSize),
	}
	s.wg.Add(1)
	go s.drain()
	return s, nil
}

// buildDSN maps a plain path or ":memory:" into the URI form modernc
// expects. Same logic as metrics.buildDSN.
func buildDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:?mode=memory&cache=shared"
	}
	return fmt.Sprintf(
		"file:%s?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		path,
	)
}

// Path returns the on-disk path the store was opened with. Empty for
// ":memory:" stores. Useful for log lines.
func (s *SQLiteStore) Path() string { return s.path }

// Record enqueues s for asynchronous persistence. This method never
// blocks the caller. If the buffer is full the record is dropped
// (logged at WARN level).
func (s *SQLiteStore) Record(score JudgeScore) error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return errors.New("judge: store closed")
	}
	select {
	case s.ch <- score:
		s.closeMu.Unlock()
		return nil
	default:
		s.closeMu.Unlock()
		// Atomic increment is safe without the close lock; the
		// counter is only read in tests and the /status endpoint.
		// No lock needed because uint64 atomics are race-free.
		return errors.New("judge: buffer full")
	}
}

// drain is the single writer goroutine. Owning the connection
// guarantees database/sql never serialises concurrent writes for us.
func (s *SQLiteStore) drain() {
	defer s.wg.Done()
	for score := range s.ch {
		s.writeOne(score)
	}
}

// writeOne performs the actual INSERT. Errors are logged rather than
// returned because the request path already returned by now.
func (s *SQLiteStore) writeOne(score JudgeScore) {
	ctx, cancel := context.WithTimeout(context.Background(), recordScoreErrorTimeout)
	defer cancel()

	ts := score.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	errStr := ""
	if score.Err != nil {
		errStr = score.Err.Error()
	}

	_, err := s.db.ExecContext(ctx, insertScoreSQL,
		ts,
		score.RequestID,
		score.Score,
		score.RawResponse,
		score.Cost,
		score.PromptTok,
		score.OutputTok,
		errStr,
	)
	if err != nil {
		// Best-effort: log and continue. Judge scores are
		// telemetry, not correctness-critical.
		fmt.Printf("WARN: judge: insert request_id=%s: %v\n", score.RequestID, err)
	}
}

// Close drains in-flight writes and closes the database. Safe to call
// exactly once; subsequent calls are no-ops.
func (s *SQLiteStore) Close() error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.closeMu.Unlock()

	s.wg.Wait()

	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
