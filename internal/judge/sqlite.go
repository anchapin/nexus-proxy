// Package judge implements the asynchronous LLM-as-a-Judge evaluator
// described in issue #15.
package judge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
    error TEXT NOT NULL DEFAULT '',
    rag_injected INTEGER NOT NULL DEFAULT 0,
    rag_similarity REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_judge_scores_timestamp ON judge_scores(timestamp);
CREATE INDEX IF NOT EXISTS idx_judge_scores_request_id ON judge_scores(request_id);
`

// judgeScoreMigrations holds additive ALTER TABLE statements that bring
// a pre-existing judge_scores table up to the current schema (issue #1167).
// Each is safe to re-run: the "duplicate column name" error is suppressed
// so a fresh database (created with the full schema above) is unaffected.
var judgeScoreMigrations = []string{
	`ALTER TABLE judge_scores ADD COLUMN rag_injected INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE judge_scores ADD COLUMN rag_similarity REAL NOT NULL DEFAULT 0`,
}

// insertScoreSQL is the prepared statement for Record.
const insertScoreSQL = `INSERT INTO judge_scores
    (timestamp, request_id, score, raw_response, cost_usd, prompt_tokens, output_tokens, error, rag_injected, rag_similarity)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
//
// Issue #1234 batch mode: when batchSize > 0, the drain accumulates
// records and commits BEGIN...INSERT...COMMIT either when the
// accumulator reaches batchSize or when batchTimeout elapses.
type SQLiteStore struct {
	db   *sql.DB
	path string // "" for ":memory:"

	ch      chan JudgeScore
	wg      sync.WaitGroup
	closed  bool
	closeMu sync.Mutex

	// Batch config (issue #1234). Same semantics as metrics store.
	batchSize    int
	batchTimeout time.Duration
}

// OpenSQLiteStore opens (or creates) a SQLite database at path and
// starts the background drain goroutine. Path may be ":memory:" for
// tests. batchSize and batchTimeout control the drain batching
// behaviour (issue #1234). Pass 0 for both to use per-record commits.
func OpenSQLiteStore(path string, batchSize int, batchTimeout time.Duration) (*SQLiteStore, error) {
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
	if err := migrateJudgeScores(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("judge: migrate: %w", err)
	}

	s := &SQLiteStore{
		db:           db,
		path:         path,
		ch:           make(chan JudgeScore, bufferedChannelSize),
		batchSize:    batchSize,
		batchTimeout: batchTimeout,
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

// migrateJudgeScores runs every additive ALTER TABLE in
// judgeScoreMigrations. Each statement is attempted individually;
// "duplicate column name" errors mean the column already exists (the
// database was created by a newer build) and are silently ignored.
// Any other error aborts Open so the operator sees the problem at boot
// (issue #1167).
func migrateJudgeScores(ctx context.Context, db *sql.DB) error {
	for _, stmt := range judgeScoreMigrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
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
//
// Issue #1234 batch mode: when s.batchSize > 0, drain accumulates
// records into an in-memory slice and commits BEGIN...INSERT...COMMIT
// either when the accumulator reaches s.batchSize or when
// s.batchTimeout elapses since the last commit.
func (s *SQLiteStore) drain() {
	defer s.wg.Done()

	var batchAcc []JudgeScore
	var batchTimer *time.Timer
	var batchTimerCh <-chan time.Time

	flush := func() {
		if len(batchAcc) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), recordScoreErrorTimeout)
		defer cancel()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			slog.Warn("judge batch begin failed", slog.Any("err", err))
			batchAcc = batchAcc[:0]
			return
		}
		stmt, err := tx.PrepareContext(ctx, insertScoreSQL)
		if err != nil {
			slog.Warn("judge batch prepare failed", slog.Any("err", err))
			_ = tx.Rollback()
			batchAcc = batchAcc[:0]
			return
		}
		for _, score := range batchAcc {
			if err := s.insertRow(ctx, stmt, score); err != nil {
				slog.Warn("judge batch insert failed", slog.String("request_id", score.RequestID), slog.Any("err", err))
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			slog.Warn("judge batch commit failed", slog.Any("err", err))
			_ = tx.Rollback()
			batchAcc = batchAcc[:0]
			return
		}
		batchAcc = batchAcc[:0]
	}

	resetTimer := func() {
		if batchTimer != nil {
			batchTimer.Stop()
			batchTimer = nil
			batchTimerCh = nil
		}
	}

	for score := range s.ch {
		if s.batchSize <= 0 {
			s.writeOne(score)
			continue
		}
		// BATCH MODE (issue #1234)
		if batchAcc == nil {
			batchAcc = append(batchAcc, score)
			if s.batchTimeout > 0 {
				resetTimer()
				batchTimer = time.NewTimer(s.batchTimeout)
				batchTimerCh = batchTimer.C
			}
			continue
		}
		batchAcc = append(batchAcc, score)
		if len(batchAcc) >= s.batchSize {
			flush()
			resetTimer()
			continue
		}
		if batchTimerCh != nil {
			select {
			case <-batchTimerCh:
				flush()
				resetTimer()
			default:
			}
		}
	}

	if len(batchAcc) > 0 {
		flush()
	}
	resetTimer()
}

// writeOne performs the actual INSERT. Errors are logged rather than
// returned because the request path already returned by now.
func (s *SQLiteStore) writeOne(score JudgeScore) {
	ctx, cancel := context.WithTimeout(context.Background(), recordScoreErrorTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("judge begin failed", slog.Any("err", err))
		return
	}
	stmt, err := tx.PrepareContext(ctx, insertScoreSQL)
	if err != nil {
		slog.Warn("judge prepare failed", slog.Any("err", err))
		_ = tx.Rollback()
		return
	}
	if err := s.insertRow(ctx, stmt, score); err != nil {
		slog.Warn("judge insert failed", slog.String("request_id", score.RequestID), slog.Any("err", err))
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		slog.Warn("judge commit failed", slog.Any("err", err))
	}
}

// insertRow executes a single INSERT using the provided statement.
func (s *SQLiteStore) insertRow(ctx context.Context, stmt *sql.Stmt, score JudgeScore) error {
	ts := score.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	errStr := ""
	if score.Err != nil {
		errStr = score.Err.Error()
	}
	ragInjected := 0
	if score.RAGInjected {
		ragInjected = 1
	}
	_, err := stmt.ExecContext(ctx,
		ts,
		score.RequestID,
		score.Score,
		score.RawResponse,
		score.Cost,
		score.PromptTok,
		score.OutputTok,
		errStr,
		ragInjected,
		score.RAGSimilarity,
	)
	return err
}

// RecentScores implements judge.Storage. It queries the most recent `limit`
// rows with a non-zero score (i.e., successful evaluations) ordered by
// timestamp DESC (newest first). Returns a nil slice (not empty) when no
// records exist.
func (s *SQLiteStore) RecentScores(limit int) ([]int, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Query is direct (no channel) so we can return immediately in tests
	// and for the adaptive sampling path which runs synchronously.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := `
		SELECT score FROM judge_scores
		WHERE score > 0
		ORDER BY timestamp DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("judge recent scores: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var score int
		if err := rows.Scan(&score); err != nil {
			return nil, fmt.Errorf("judge recent scores scan: %w", err)
		}
		out = append(out, score)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("judge recent scores rows: %w", err)
	}
	return out, nil
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
