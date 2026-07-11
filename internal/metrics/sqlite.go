package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
//
// The route_source / route_reason / slm_confidence / slm_task_type
// columns (issue #74) are added here for fresh databases. Existing
// databases are migrated via migrateRouteSourceColumns at Open time
// so additive ALTER TABLE statements bring them up to the same shape
// without data loss.
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
    error TEXT NOT NULL DEFAULT '',
    route_source TEXT NOT NULL DEFAULT '',
    route_reason TEXT NOT NULL DEFAULT '',
    slm_confidence REAL NOT NULL DEFAULT 0,
    slm_task_type TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_request_id ON requests(request_id);
`

// routeSourceMigrations is the set of additive ALTER TABLE statements
// that bring an existing requests table up to the issue #74 schema.
// Each is idempotent: SQLite errors on duplicate-column are swallowed
// by the caller (migrateRouteSourceColumns) so re-running against an
// already-migrated database is a no-op.
var routeSourceMigrations = []string{
	`ALTER TABLE requests ADD COLUMN route_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN route_reason TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN slm_confidence REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN slm_task_type TEXT NOT NULL DEFAULT ''`,
}

// migrateRouteSourceColumns runs the additive ALTER TABLE migrations
// for issue #74. Each statement is attempted individually; "duplicate
// column" errors mean the column already exists (the database was
// created or migrated by a newer build) and are silently ignored.
// Any other error aborts Open so the operator sees the problem at
// boot rather than on the first failed INSERT.
func migrateRouteSourceColumns(ctx context.Context, db *sql.DB) error {
	for _, stmt := range routeSourceMigrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// modernc.org/sqlite returns the error string
			// containing "duplicate column name" for the
			// already-exists case. We match on that substring
			// rather than a typed error so the check survives
			// driver-level error wrapping.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("metrics: migrate: %w", err)
		}
	}
	return nil
}

// insertSQL is the prepared statement for RecordRequest. Kept as a
// package const so the goroutine can reuse the same SQL string after
// statement caching.
const insertSQL = `INSERT INTO requests
    (timestamp, request_id, route, model,
     input_tokens, output_tokens, toon_savings_tokens,
     rag_injected, rag_filename, estimated_cost_usd,
     ttft_ms, total_latency_ms, tps, streaming, fusion_arbiter_skipped, error,
     route_source, route_reason, slm_confidence, slm_task_type)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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

	// Issue #74: bring existing databases up to the route-source
	// schema. Fresh databases already have the columns (from
	// requestsSchema above), so every ALTER will no-op on the
	// "duplicate column name" error and the migration completes in
	// microseconds.
	if err := migrateRouteSourceColumns(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: migrate route-source: %w", err)
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
		req.RouteSource, req.RouteReason, req.SLMConfidence, req.SLMTaskType,
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

// providerStatsAggregateSQL computes count, average cost, and error
// rate per model over the [since, now] window. The p50/p95 latencies
// are computed by a separate query below — modernc.org/sqlite supports
// window functions but keeping the percentile step in its own query
// avoids repeating the window scan twice.
const providerStatsAggregateSQL = `
SELECT model,
       COUNT(*) AS sample_count,
       COALESCE(AVG(estimated_cost_usd), 0) AS avg_cost,
       SUM(CASE WHEN error != '' THEN 1 ELSE 0 END) AS error_count
FROM requests
WHERE timestamp >= ? AND model != 'unknown'
GROUP BY model
ORDER BY model`

// providerStatsPercentileSQL picks the row at the (rank)-th
// percentile position for each model. The caller binds the percentile
// rank via the LIMIT/OFFSET trick — a subquery returns COUNT(*) per
// model, and a single SELECT with ROW_NUMBER() picks the row whose
// rank matches. Returns a row per model with its p_NN latency.
//
// Two queries are required because SQLite cannot bind LIMIT/OFFSET in
// a single windowed aggregate across multiple percentiles. Both
// queries share the same index (idx_requests_timestamp) so the cost
// is two cheap range scans.
const providerStatsPercentileSQL = `
WITH ranked AS (
    SELECT model, total_latency_ms,
           ROW_NUMBER() OVER (PARTITION BY model ORDER BY total_latency_ms) AS rn,
           COUNT(*) OVER (PARTITION BY model) AS cnt
    FROM requests
    WHERE timestamp >= ? AND model != 'unknown'
)
SELECT model, total_latency_ms
FROM ranked
WHERE rn = MAX(1, CAST((cnt * ? + 99) / 100 AS INTEGER))
ORDER BY model`

// providerStatsQueryTimeout bounds a single ProviderStats call. The
// aggregation runs over the requests table; even with millions of
// rows the indexed range scan finishes in tens of milliseconds. The
// 10s ceiling exists only so a stalled disk cannot pin the refresh
// goroutine forever.
const providerStatsQueryTimeout = 10 * time.Second

// ProviderStats returns the per-provider latency, cost, sample-count
// and error-rate aggregate for the sliding window ending now and
// starting at since. It satisfies router.ProviderStatsSource so the
// chat handler's route=frontier dispatch can score frontier and z.ai
// against each other without hitting the DB on the hot path (the
// router.ProviderStatsCache calls this from a background goroutine on
// a fixed cadence).
//
// "Provider" is identified by the requests.model column — each
// configured frontier endpoint uses a distinct model name (e.g.
// "gpt-4o" for frontier, "glm-4.6" for z.ai), so the per-model
// aggregation maps cleanly to the configured provider list. Rows
// where model = "unknown" (the sentinel used by RecordRequest when no
// model was supplied) are filtered out so an under-instrumented
// provider cannot pollute the aggregate.
//
// p50 is the row at the 50th percentile of total_latency_ms; p95 at
// the 95th. The selector currently consumes only p50, but p95 is
// surfaced so future operators can reason about tail latency without
// extending the schema.
//
// Safe for concurrent use; SQLiteStore owns a single connection so
// concurrent ProviderStats calls serialise but never block on the
// request hot path (which never calls this directly).
func (s *SQLiteStore) ProviderStats(since time.Time) ([]router.ProviderStats, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerStatsQueryTimeout)
	defer cancel()

	// Step 1: per-model sample_count, avg_cost, error_count.
	rows, err := s.db.QueryContext(ctx, providerStatsAggregateSQL, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("metrics: provider stats aggregate: %w", err)
	}
	type aggRow struct {
		name        string
		sampleCount int
		avgCost     float64
		errorCount  int
	}
	var aggs []aggRow
	for rows.Next() {
		var r aggRow
		if err := rows.Scan(&r.name, &r.sampleCount, &r.avgCost, &r.errorCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("metrics: scan aggregate: %w", err)
		}
		aggs = append(aggs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: aggregate rows: %w", err)
	}
	if len(aggs) == 0 {
		return nil, nil
	}

	// Step 2: per-model p50 (one windowed scan).
	p50ByName, err := s.providerStatsPercentile(ctx, since, 50)
	if err != nil {
		return nil, err
	}
	// Step 3: per-model p95 (another windowed scan). Kept
	// separate so the percentile position is computed once per
	// model rather than twice in the same query, and so a future
	// operator who only needs p50 can short-circuit by skipping
	// the second call.
	p95ByName, err := s.providerStatsPercentile(ctx, since, 95)
	if err != nil {
		return nil, err
	}

	out := make([]router.ProviderStats, 0, len(aggs))
	for _, a := range aggs {
		var errRate float64
		if a.sampleCount > 0 {
			errRate = float64(a.errorCount) / float64(a.sampleCount)
		}
		out = append(out, router.ProviderStats{
			Name:         a.name,
			P50LatencyMs: p50ByName[a.name],
			P95LatencyMs: p95ByName[a.name],
			AvgCostUSD:   a.avgCost,
			SampleCount:  a.sampleCount,
			ErrorRate:    errRate,
		})
	}
	return out, nil
}

// providerStatsPercentile runs the windowed percentile query for a
// single rank and returns a name -> latency map. The rank is a
// percentile in 1..100. Empty / missing rows return a zero latency;
// callers should treat that as "no data" (the selector already
// filters P50 <= 0).
func (s *SQLiteStore) providerStatsPercentile(ctx context.Context, since time.Time, percentile int) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, providerStatsPercentileSQL, since.UTC(), percentile)
	if err != nil {
		return nil, fmt.Errorf("metrics: provider stats p%d: %w", percentile, err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var name string
		var lat int64
		if err := rows.Scan(&name, &lat); err != nil {
			return nil, fmt.Errorf("metrics: scan p%d: %w", percentile, err)
		}
		out[name] = lat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: p%d rows: %w", percentile, err)
	}
	return out, nil
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
		RouteSource:          r.RouteSource,
		RouteReason:          r.RouteReason,
		SLMConfidence:        r.SLMConfidence,
		SLMTaskType:          r.SLMTaskType,
		// TOONSavingsTokens / RAGInjected / RAGFilename /
		// EstimatedCostUSD default zero — populated by callers
		// that explicitly use RecordRequest.
	})
}
