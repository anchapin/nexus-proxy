package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	// Pure-Go SQLite driver. Imports register the driver under the
	// name "sqlite" (see modernc.org/sqlite/sqlite.go). No CGo so
	// the build stays inside the stdlib-ish spirit of the PRD.
	_ "modernc.org/sqlite"

	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// requestsSchema is the v1 schema for the per-request metrics table.
// One row per proxied request — including failed ones — so the dashboard
// can compute its own error / success ratios from the same column set.
//
// All columns are NOT NULL with safe defaults; this keeps the INSERT
// trivial (positional bind values, no NULL handling) and means future
// schema extensions can ALTER TABLE ADD COLUMN without rewriting
// existing rows.
//
// Index on timestamp is the only one beyond the implicit PK index;
// daily aggregations are the dominant read pattern.
const requestsSchema = `
CREATE TABLE IF NOT EXISTS requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME NOT NULL,
    request_id TEXT NOT NULL,
    route TEXT NOT NULL,
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    toon_savings_tokens INTEGER NOT NULL DEFAULT 0,
    rag_injected INTEGER NOT NULL DEFAULT 0,
    rag_filename TEXT NOT NULL DEFAULT '',
    estimated_cost_usd REAL NOT NULL DEFAULT 0,
    ttft_ms INTEGER NOT NULL DEFAULT 0,
    total_latency_ms INTEGER NOT NULL DEFAULT 0,
    tps REAL NOT NULL DEFAULT 0,
    streaming INTEGER NOT NULL DEFAULT 1,
    fusion_arbiter_skipped INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_request_id ON requests(request_id);
`

// insertSQL is the prepared statement for RecordRequest. Kept as a
// package const so the goroutine can reuse the same SQL string after
// statement caching.
const insertSQL = `INSERT INTO requests
    (timestamp, request_id, route, model,
     input_tokens, output_tokens, toon_savings_tokens,
     rag_injected, rag_filename, estimated_cost_usd,
     ttft_ms, total_latency_ms, tps, streaming, fusion_arbiter_skipped, error)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SQLiteStore is the production Store implementation (issue #4).
// Writes are funnelled through a buffered channel and a single
// background goroutine; reads are issued synchronously from the
// caller's goroutine against the same *sql.DB.
//
// SQLiteStore additionally implements telemetry.Recorder so the
// existing chat handler can keep its Recorder-plugging code path:
// when Record(telemetry.Record) is called, the row lands with the new
// Request fields defaulting to zero. The handler can populate the
// full RecordRequest surface via MetricsStore in main.go — both paths
// use the same underlying table.
type SQLiteStore struct {
	db     *sql.DB
	logger Logger

	// Buffered write channel + background drainer (same shape as
	// telemetry.JSONLRecorder). The drainer owns the single
	// writeable connection so concurrent Record callers never
	// share a *sql.Tx. Overflow drops with a counter so an
	// operator can spot saturation.
	ch      chan Request
	dropped atomicDropped

	wg     sync.WaitGroup
	closed bool
	close  closeOnce
	path   string // "" for ":memory:"
}

// newSQLiteStore opens the database, creates the schema (idempotent),
// and starts the background drain goroutine.
func newSQLiteStore(path string, lg Logger) (*SQLiteStore, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("metrics: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1) // writer goroutine owns the only conn; reads share
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: ping %q: %w", path, err)
	}
	if _, err := db.ExecContext(context.Background(), requestsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: create schema: %w", err)
	}

	s := &SQLiteStore{
		db:     db,
		logger: lg,
		ch:     make(chan Request, bufferedChannelSize),
		path:   path,
	}
	s.wg.Add(1)
	go s.drain()
	return s, nil
}

// buildDSN maps a plain path or ":memory:" into the URI form modernc
// expects. The _pragma pair flips the connection to WAL and sets a
// short busy timeout — both are recommended for hot-path append-only
// workloads with concurrent readers (DailySummary).
func buildDSN(path string) string {
	if path == ":memory:" {
		// Per-connection :memory: gives each Open a private DB.
		// For DailySummary this is fine — tests use it as a
		// single process-owned scratch DB.
		return "file::memory:?mode=memory&cache=shared"
	}
	// file:<path>?mode=rwc&_pragma=...   (create-or-open, RW)
	// _pragma=journal_mode(WAL)        — concurrent readers don't
	//                                    block the writer.
	// _pragma=busy_timeout(5000)       — bounded wait on contention.
	// _pragma=synchronous(NORMAL)      — fewer fsyncs; safe for a
	//                                    best-effort metrics log.
	// _pragma=foreign_keys(ON)         — future-proof for relational
	//                                    extensions.
	return fmt.Sprintf(
		"file:%s?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		path,
	)
}

// Path returns the on-disk path the store was opened with. Empty for
// ":memory:" stores. Useful for log lines and the reopen-on-disk
// pattern used by tests that need to verify the row landed beyond
// the buffer.
func (s *SQLiteStore) Path() string { return s.path }

// Dropped returns the number of records dropped because the write
// buffer was full. See DroppedCounter.
func (s *SQLiteStore) Dropped() uint64 { return s.dropped.get() }

// RecordRequest writes req to the `requests` table. The actual write
// happens asynchronously on the drain goroutine; this method never
// blocks the caller. If the buffer is full the record is dropped and
// the dropped counter is incremented.
//
// The error return is reserved for "DB is closed" / "Open failed"
// conditions where there is no goroutine to fail — current callers
// ignore it. Synchronous observers can use DailySummary to verify
// rows landed.
func (s *SQLiteStore) RecordRequest(req Request) error {
	if s.closed {
		return errors.New("metrics: store closed")
	}
	select {
	case s.ch <- req:
		return nil
	default:
		s.dropped.add()
		s.logger("WARN: buffer full, dropped record request_id=%s", req.RequestID)
		return nil
	}
}

// drain is the single writer goroutine. Owning the connection
// guarantees database/sql never serialises concurrent writes for us
// (which it would, but with extra context switches).
func (s *SQLiteStore) drain() {
	defer s.wg.Done()
	for req := range s.ch {
		s.writeOne(req)
	}
}

// writeOne performs the actual INSERT. Errors are logged at WARN
// rather than returned — the request path already returned by now.
func (s *SQLiteStore) writeOne(req Request) {
	ctx, cancel := context.WithTimeout(context.Background(), recordRequestErrorTimeout)
	defer cancel()
	route := req.Route
	if route == "" {
		route = string(router.RouteFrontier)
	}
	model := req.Model
	if model == "" {
		model = "unknown"
	}
	ts := req.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	ragInjected := 0
	if req.RAGInjected {
		ragInjected = 1
	}
	streaming := 0
	if req.Streaming {
		streaming = 1
	}
	fusionArbiterSkipped := 0
	if req.FusionArbiterSkipped {
		fusionArbiterSkipped = 1
	}
	_, err := s.db.ExecContext(ctx, insertSQL,
		ts.UTC(), req.RequestID, route, model,
		req.InputTokens, req.OutputTokens, req.TOONSavingsTokens,
		ragInjected, req.RAGFilename, req.EstimatedCostUSD,
		req.TTFTMs, req.TotalLatencyMs, req.TPS, streaming,
		fusionArbiterSkipped, req.Error,
	)
	if err != nil {
		s.logger("ERROR: insert request_id=%s: %v", req.RequestID, err)
	}
}

// DailySummary returns the roll-up for the UTC day containing date.
// Always reads; safe to call concurrently with writes.
func (s *SQLiteStore) DailySummary(date time.Time) (Summary, error) {
	day := date.UTC().Truncate(24 * time.Hour)
	next := day.Add(24 * time.Hour)

	// One aggregate per metric — the statement is built once
	// per call because the date range is parametric. Indexes on
	// idx_requests_timestamp keep the range scan cheap.
	const summarySQL = `
SELECT
    COUNT(*),
    COALESCE(SUM(CASE WHEN route = 'local'    THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN route = 'frontier' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN route = 'fusion'   THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(input_tokens), 0),
    COALESCE(SUM(toon_savings_tokens), 0),
    COALESCE(SUM(CASE WHEN rag_injected = 1 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(estimated_cost_usd), 0),
    COALESCE(SUM(total_latency_ms), 0),
    COALESCE(SUM(CASE WHEN error != '' THEN 1 ELSE 0 END), 0)
FROM requests
WHERE timestamp >= ? AND timestamp < ?`

	ctx, cancel := context.WithTimeout(context.Background(), recordRequestErrorTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx, summarySQL, day, next)
	var sum Summary
	sum.Date = day
	if err := row.Scan(
		&sum.RequestCount,
		&sum.LocalCount,
		&sum.FrontierCount,
		&sum.FusionCount,
		&sum.TotalInputTokens,
		&sum.TOONSavingsTokens,
		&sum.RAGInjectedCount,
		&sum.EstimatedCostTotal,
		&sum.TotalLatencyMsSum,
		&sum.ErrorCount,
	); err != nil {
		return Summary{}, fmt.Errorf("metrics: daily summary: %w", err)
	}
	return sum, nil
}

// Close drains in-flight writes and closes the database. Safe to
// call exactly once; subsequent calls are no-ops.
func (s *SQLiteStore) Close() error {
	if s == nil {
		return nil
	}
	if !s.closed {
		s.closed = true
		close(s.ch)
		s.wg.Wait()
	}
	return s.close.Close(func() error {
		if s.db != nil {
			return s.db.Close()
		}
		return nil
	})
}

// --- telemetry.Recorder compatibility ------------------------------------
//
// The chat handler still uses the Recorder interface defined in
// internal/telemetry (issue #16). SQLiteStore satisfies it so the
// existing wiring in cmd/nexus/main.go can adopt the SQLite path
// with a one-line swap. Field defaults to zero / false so legacy
// callers don't lose information that was never tracked to begin
// with.

// Record implements telemetry.Recorder. The method NEVER blocks; if
// the buffer is full the record is dropped with a counter increment,
// mirroring telemetry.JSONLRecorder's contract.
//
// Errors during the actual write are logged asynchronously and are
// not returned (the handler has long since left).
func (s *SQLiteStore) Record(r telemetry.Record) {
	_ = s.RecordRequest(Request{
		Timestamp:            r.Timestamp,
		RequestID:            r.RequestID,
		Route:                r.Route,
		Model:                r.Model,
		InputTokens:          r.InputTokens,
		OutputTokens:         r.OutputTokens,
		TTFTMs:               r.TTFTMs,
		TotalLatencyMs:       r.TotalLatencyMs,
		TPS:                  r.TPS,
		Streaming:            r.Streaming,
		FusionArbiterSkipped: r.FusionArbiterSkipped,
		Error:                r.Error,
		// TOONSavingsTokens / RAGInjected / RAGFilename /
		// EstimatedCostUSD default zero — populated by callers
		// that explicitly use RecordRequest.
	})
}
