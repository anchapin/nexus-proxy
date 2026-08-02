package metrics

// SQLite metrics store (issue #4). Uses modernc.org/sqlite — the only
// allowed third-party runtime dependency. See AGENTS.md and README.md.
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
// databases are migrated via runAdditiveMigrations at Open time
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
    input_cost_usd REAL NOT NULL DEFAULT 0,
    output_cost_usd REAL NOT NULL DEFAULT 0,
    baseline_cost_usd REAL NOT NULL DEFAULT 0,
    savings_usd REAL NOT NULL DEFAULT 0,
    ttft_ms INTEGER NOT NULL DEFAULT 0,
    total_latency_ms REAL NOT NULL DEFAULT 0,
    tps REAL NOT NULL DEFAULT 0,
    streaming INTEGER NOT NULL DEFAULT 1,
    fusion_arbiter_skipped INTEGER NOT NULL DEFAULT 0,
    fusion_jaccard_similarity REAL NOT NULL DEFAULT 0,
    fusion_arbiter_cost_usd REAL NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    route_source TEXT NOT NULL DEFAULT '',
    route_reason TEXT NOT NULL DEFAULT '',
    slm_confidence REAL NOT NULL DEFAULT 0,
    slm_task_type TEXT NOT NULL DEFAULT '',
    arbiter_cache_key TEXT NOT NULL DEFAULT '',
    arbiter_synthesis TEXT NOT NULL DEFAULT '',
    tenant TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_request_id ON requests(request_id);
CREATE INDEX IF NOT EXISTS idx_requests_route_timestamp ON requests(route, timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_model_timestamp ON requests(model, timestamp);
`

// additiveMigrations is the set of additive ALTER TABLE statements
// that bring an existing requests table up to the current schema.
// Each is idempotent: SQLite errors on duplicate-column are swallowed
// by the caller (runAdditiveMigrations) so re-running against an
// already-migrated database is a no-op.
var additiveMigrations = []string{
	// Issue #74: route-source columns
	`ALTER TABLE requests ADD COLUMN route_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN route_reason TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN slm_confidence REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN slm_task_type TEXT NOT NULL DEFAULT ''`,
	// Issue #200: Jaccard similarity
	`ALTER TABLE requests ADD COLUMN fusion_jaccard_similarity REAL NOT NULL DEFAULT 0`,
	// Issue #247: TOON compression method
	`ALTER TABLE requests ADD COLUMN toon_compression_method TEXT NOT NULL DEFAULT ''`,
	// Issue #239: arbiter cost tracking
	`ALTER TABLE requests ADD COLUMN fusion_arbiter_cost_usd REAL NOT NULL DEFAULT 0`,
	// Issue #1183: per-provider cost split (input/output token streams)
	`ALTER TABLE requests ADD COLUMN input_cost_usd REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN output_cost_usd REAL NOT NULL DEFAULT 0`,
	// Issue #227: rag cache hit tracking
	`ALTER TABLE requests ADD COLUMN rag_cache_hit INTEGER NOT NULL DEFAULT 0`,
	// Issue #1176: arbiter cache key + synthesis for boot-time pre-warming
	`ALTER TABLE requests ADD COLUMN arbiter_cache_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN arbiter_synthesis TEXT NOT NULL DEFAULT ''`,
	// Issue #1154: tenant attribution (multi-key inbound auth)
	`ALTER TABLE requests ADD COLUMN tenant TEXT NOT NULL DEFAULT ''`,
}

// runAdditiveMigrations executes the additive ALTER TABLE migrations.
// Each statement is attempted individually; "duplicate column" errors
// mean the column already exists (the database was created or migrated
// by a newer build) and are silently ignored. Any other error aborts
// Open so the operator sees the problem at boot rather than on the
// first failed INSERT.
func runAdditiveMigrations(ctx context.Context, db *sql.DB) error {
	for _, stmt := range additiveMigrations {
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

// schemaMigrations holds additive ALTER TABLE statements that bring a
// pre-existing database up to the current schema version. Each statement
// is safe to re-run: the "duplicate column name" error is suppressed so
// a fresh database (already created with the full schema) is unaffected.
//
// Schema additions must remain additive-only (new columns, never drop /
// rename) so existing rows keep their values and the INSERT positional
// bind list can simply append the new column at the end.
var schemaMigrations = []string{
	`ALTER TABLE requests ADD COLUMN baseline_cost_usd REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN savings_usd REAL NOT NULL DEFAULT 0`,
}

// insertSQL is the prepared statement for RecordRequest. Kept as a
// package const so the goroutine can reuse the same SQL string after
// statement caching.
const insertSQL = `INSERT INTO requests
    (timestamp, request_id, route, model,
     input_tokens, output_tokens, toon_savings_tokens, toon_compression_method,
     rag_injected, rag_filename, rag_cache_hit, estimated_cost_usd,
     input_cost_usd, output_cost_usd,
     baseline_cost_usd, savings_usd,
     ttft_ms, total_latency_ms, tps, streaming,
     fusion_arbiter_skipped, fusion_jaccard_similarity, fusion_arbiter_cost_usd, error,
      route_source, route_reason, slm_confidence, slm_task_type,
      arbiter_cache_key, arbiter_synthesis, tenant)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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

	// Retention prune goroutine (issue #483). pruneStop is non-nil
	// only when retentionDays > 0; Close closes it to unblock the
	// goroutine before wg.Wait.
	pruneStop chan struct{}

	// pruneLastRows / pruneLastTimestamp are updated atomically by
	// pruneOnce so the /metrics gauge provider can read them without
	// locking. Zero until the first successful prune tick.
	pruneLastRows      atomic.Int64
	pruneLastTimestamp atomic.Int64

	wg     sync.WaitGroup
	closed bool
	close  closeOnce
	path   string // "" for ":memory:"
}

// newSQLiteStore opens the database, creates the schema (idempotent),
// and starts the background drain goroutine. When retentionDays > 0 a
// second goroutine periodically DELETEs rows older than the retention
// window (issue #483).
func newSQLiteStore(path string, retentionDays int, lg Logger) (*SQLiteStore, error) {
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
	if err := migrateSchema(ctx_bg(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: migrate schema: %w", err)
	}

	// Issue #74: bring existing databases up to the route-source
	// schema. Fresh databases already have the columns (from
	// requestsSchema above), so every ALTER will no-op on the
	// "duplicate column name" error and the migration completes in
	// microseconds.
	if err := runAdditiveMigrations(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metrics: migrate route-source: %w", err)
	}

	// Issue #595: ensure auto_vacuum=INCREMENTAL so that
	// incremental_vacuum in pruneOnce actually reclaims pages.
	// The DSN pragma handles fresh databases; this is the safety
	// net for databases upgraded from a pre-fix build. Errors are
	// logged as warnings and do not prevent the store from opening.
	migrateAutoVacuum(context.Background(), db, lg)

	s := &SQLiteStore{
		db:     db,
		logger: lg,
		ch:     make(chan Request, bufferedChannelSize),
		path:   path,
	}
	s.wg.Add(1)
	go s.drain()

	// Retention prune goroutine (issue #483). Only started when the
	// operator set a non-zero retention window; the default (0) is
	// byte-for-byte identical to the pre-#483 path.
	if retentionDays > 0 {
		s.pruneStop = make(chan struct{})
		s.wg.Add(1)
		go s.prune(retentionDays)
	}
	return s, nil
}

// ctx_bg returns a fresh background context for one-off migration
// calls. Kept as a tiny helper so the migration code reads cleanly
// without inlining context.WithTimeout boilerplate.
func ctx_bg() context.Context {
	return context.Background()
}

// migrateSchema runs every additive ALTER TABLE in schemaMigrations.
// Each statement is attempted unconditionally; the "duplicate column
// name" error (SQLite returns this when the column already exists) is
// suppressed so the migration is idempotent on both fresh databases
// (columns already present from CREATE TABLE) and databases upgraded
// from a prior schema version.
func migrateSchema(ctx context.Context, db *sql.DB) error {
	for _, ddl := range schemaMigrations {
		_, err := db.ExecContext(ctx, ddl)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// autoVacuumIncremental is the integer value SQLite stores in the
// database header for auto_vacuum=INCREMENTAL (0=NONE, 1=FULL, 2=INCREMENTAL).
const autoVacuumIncremental = 2

// migrateAutoVacuum ensures the database is in auto_vacuum=INCREMENTAL
// mode so that incremental_vacuum in pruneOnce actually reclaims freed
// pages (issue #595).
//
// The DSN _pragma=auto_vacuum(INCREMENTAL) handles fresh databases by
// setting the mode before any tables exist. This function is the safety
// net for databases created by a pre-fix build (auto_vacuum=NONE): it
// checks the current mode and, if not already INCREMENTAL, sets the
// pragma and runs VACUUM to rebuild the file with the new mode. On
// SQLite, setting auto_vacuum on an existing database with tables is
// silently ignored unless followed by VACUUM.
//
// Errors are logged as warnings and do NOT prevent the store from
// opening — a failed migration simply means incremental_vacuum remains
// a no-op until the operator runs a manual VACUUM.
func migrateAutoVacuum(ctx context.Context, db *sql.DB, lg Logger) {
	var mode int
	if err := db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		lg("WARN: metrics: check auto_vacuum mode: %v", err)
		return
	}
	if mode == autoVacuumIncremental {
		return
	}
	if _, err := db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		lg("WARN: metrics: set auto_vacuum=INCREMENTAL: %v", err)
		return
	}
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		lg("WARN: metrics: VACUUM for auto_vacuum migration: %v"+
			" (incremental_vacuum will remain a no-op until manual VACUUM)", err)
		return
	}
	lg("metrics: migrated auto_vacuum to INCREMENTAL (was mode=%d)", mode)
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
	// _pragma=auto_vacuum(INCREMENTAL) — set before any tables exist
	//                                    on a fresh database so
	//                                    incremental_vacuum in
	//                                    pruneOnce actually reclaims
	//                                    pages (issue #595).
	return fmt.Sprintf(
		"file:%s?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=auto_vacuum(INCREMENTAL)",
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
		req.InputTokens, req.OutputTokens, req.TOONSavingsTokens, req.TOONCompressionMethod,
		ragInjected, req.RAGFilename, req.RAGCacheHit, req.EstimatedCostUSD,
		req.InputCostUSD, req.OutputCostUSD,
		req.BaselineCostUSD, req.SavingsUSD,
		req.TTFTMs, req.TotalLatencyMs, req.TPS, streaming,
		fusionArbiterSkipped, req.FusionJaccardSimilarity, req.FusionArbiterCostUSD, req.Error,
		req.RouteSource, req.RouteReason, req.SLMConfidence, req.SLMTaskType,
		req.ArbiterCacheKeyHex, req.ArbiterSynthesis,
		req.Tenant,
	)
	if err != nil {
		s.logger("ERROR: insert request_id=%s: %v", req.RequestID, err)
	}
}

// --- Retention pruning (issue #483) ---------------------------------------
//
// The requests table is the highest-volume table in the metrics DB
// (~52M rows/year at 100 req/min). Without a retention window the
// table grows without bound. When the operator sets
// NEXUS_METRICS_RETENTION_DAYS > 0, newSQLiteStore starts a background
// goroutine that wakes every pruneInterval and DELETEs rows whose
// timestamp is older than the retention window.
//
// The cutoff is computed in Go and passed as a time.Time parameter so
// the comparison uses the same storage format modernc.org/sqlite uses
// for the stored timestamps — no reliance on SQLite datetime() string
// format compatibility.

// pruneInterval is how often the background prune goroutine wakes.
// ~1 hour per the issue spec; short enough to meet the "~1 hour"
// acceptance criterion without burning CPU on a cold table.
const pruneInterval = time.Hour

// pruneTimeout bounds a single prune pass so a stalled disk cannot
// pin the goroutine indefinitely. The DELETE is indexed by
// idx_requests_timestamp so it is cheap even on millions of rows.
const pruneTimeout = 30 * time.Second

// pruneVacuumThreshold is the minimum DELETE row count that triggers an
// incremental_vacuum pass for best-effort space reclamation. Below this
// the overhead outweighs the benefit. Requires auto_vacuum=INCREMENTAL,
// which is set at database creation time via the DSN pragma (issue #595).
const pruneVacuumThreshold = 1000

// pruneSQL deletes every row whose timestamp is strictly older than
// the cutoff (a Go time.Time bound by the caller). The index
// idx_requests_timestamp makes the range scan cheap.
const pruneSQL = `DELETE FROM requests WHERE timestamp < ?`

// PruneLastRows returns the number of rows removed by the most recent
// prune pass. Zero until the first prune runs. Safe for concurrent use.
func (s *SQLiteStore) PruneLastRows() int64 { return s.pruneLastRows.Load() }

// PruneLastTimestamp returns the Unix timestamp (seconds) of the most
// recent successful prune pass. Zero until the first prune runs.
// Safe for concurrent use.
func (s *SQLiteStore) PruneLastTimestamp() int64 { return s.pruneLastTimestamp.Load() }

// prune is the background retention goroutine. It runs an immediate
// prune at startup (so retention is enforced right after boot, not up
// to an hour later), then wakes every pruneInterval. It exits when
// pruneStop is closed (during Close).
func (s *SQLiteStore) prune(retentionDays int) {
	defer s.wg.Done()
	s.pruneOnce(retentionDays)
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.pruneStop:
			return
		case <-ticker.C:
			s.pruneOnce(retentionDays)
		}
	}
}

// pruneOnce executes one DELETE pass and records the outcome in the
// atomic gauges. Exported via the struct (lowercase) so tests can call
// it directly without waiting for the hourly ticker.
func (s *SQLiteStore) pruneOnce(retentionDays int) {
	if retentionDays <= 0 {
		return // retention disabled — no-op guard
	}
	ctx, cancel := context.WithTimeout(context.Background(), pruneTimeout)
	defer cancel()

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	res, err := s.db.ExecContext(ctx, pruneSQL, cutoff)
	if err != nil {
		s.logger("WARN: retention prune failed: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	s.pruneLastRows.Store(n)
	s.pruneLastTimestamp.Store(time.Now().Unix())

	// Best-effort space reclamation. Effective on databases created
	// with auto_vacuum=INCREMENTAL (the default since issue #595).
	if n >= pruneVacuumThreshold {
		if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum(100)"); err != nil {
			s.logger("WARN: incremental_vacuum after prune: %v", err)
		}
	}
	if n > 0 {
		s.logger("retention prune: removed %d rows older than %d days", n, retentionDays)
	}
}

// DailySummary returns the roll-up for the UTC day containing date.
// Always reads; safe to call concurrently with writes.
func (s *SQLiteStore) DailySummary(date time.Time) (Summary, error) {
	day := date.UTC().Truncate(24 * time.Hour)
	next := day.Add(24 * time.Hour)
	return s.scanRange(day, next, day)
}

// RangeSummary returns a single Summary aggregating every request whose
// timestamp falls in the half-open interval [start, end). It collapses a
// weekly / monthly / quarterly window into one row in a single SQL
// round-trip, replacing N per-day DailySummary calls for long-horizon
// dashboard views (issue #1170). The Date field of the returned Summary
// is the truncated start. An empty range (start not before end) returns
// an error so a caller never silently sees a zero-row aggregate that
// masquerades as "no traffic". Safe to call concurrently with writes.
func (s *SQLiteStore) RangeSummary(start, end time.Time) (Summary, error) {
	s0 := start.UTC().Truncate(24 * time.Hour)
	e0 := end.UTC().Truncate(24 * time.Hour)
	if !s0.Before(e0) {
		return Summary{}, fmt.Errorf("metrics: range summary: empty range [%s, %s)", s0.Format("2006-01-02"), e0.Format("2006-01-02"))
	}
	return s.scanRange(s0, e0, s0)
}

// rangeAggregateSQL is the shared aggregation statement used by both
// DailySummary and RangeSummary. It collapses all rows whose timestamp
// falls in the half-open interval [from, to) into a single Summary.
// The idx_requests_timestamp index keeps the range scan cheap even over
// months of data.
const rangeAggregateSQL = `
SELECT
    COUNT(*),
    COALESCE(SUM(CASE WHEN route = 'local'    THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN route = 'frontier' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN route = 'fusion'   THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(input_tokens), 0),
    COALESCE(SUM(toon_savings_tokens), 0),
    COALESCE(SUM(CASE WHEN rag_injected = 1 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(estimated_cost_usd), 0),
    COALESCE(SUM(input_cost_usd), 0),
    COALESCE(SUM(output_cost_usd), 0),
    COALESCE(SUM(baseline_cost_usd), 0),
    COALESCE(SUM(savings_usd), 0),
    COALESCE(SUM(total_latency_ms), 0),
    COALESCE(SUM(CASE WHEN error != '' THEN 1 ELSE 0 END), 0)
FROM requests
WHERE timestamp >= ? AND timestamp < ?`

// scanRange executes rangeAggregateSQL over the half-open [from, to)
// window and returns the single aggregated Summary, stamping its Date
// field with dateLabel. Callers pre-truncate the bounds to UTC days.
func (s *SQLiteStore) scanRange(from, to, dateLabel time.Time) (Summary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), recordRequestErrorTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx, rangeAggregateSQL, from, to)
	var sum Summary
	sum.Date = dateLabel
	if err := row.Scan(
		&sum.RequestCount,
		&sum.LocalCount,
		&sum.FrontierCount,
		&sum.FusionCount,
		&sum.TotalInputTokens,
		&sum.TOONSavingsTokens,
		&sum.RAGInjectedCount,
		&sum.EstimatedCostTotal,
		&sum.InputCostTotal,
		&sum.OutputCostTotal,
		&sum.BaselineCostTotal,
		&sum.SavingsTotal,
		&sum.TotalLatencyMsSum,
		&sum.ErrorCount,
	); err != nil {
		return Summary{}, fmt.Errorf("metrics: range summary: %w", err)
	}
	return sum, nil
}

// ArbiterSynthesisRow is one historical arbiter synthesis entry returned
// by RecentArbiterSyntheses (issue #1176). CacheKeyHex is the hex-encoded
// SHA-256 hash of the two panel-member contents; the caller decodes it
// back to [32]byte for ArbiterCache.Warm.
type ArbiterSynthesisRow struct {
	CacheKeyHex string
	Synthesis   string
	Timestamp   time.Time
}

// recentArbiterSynthesesSQL selects the most recent synthesis per unique
// cache key within the TTL window. GROUP BY deduplicates repeated
// disagreements on identical panel content so the warmer does not process
// redundant rows. The composite index on (route, timestamp) plus the
// arbiter_cache_key != ” filter keeps the scan narrow.
const recentArbiterSynthesesSQL = `
SELECT arbiter_cache_key, arbiter_synthesis, MAX(timestamp) AS ts
FROM requests
WHERE arbiter_cache_key != '' AND arbiter_synthesis != '' AND timestamp >= ?
GROUP BY arbiter_cache_key
ORDER BY ts DESC
LIMIT ?`

// recentArbiterSynthesesTimeout bounds the boot-time query so a large
// metrics DB cannot stall startup.
const recentArbiterSynthesesTimeout = 10 * time.Second

// ArbiterSynthesisReader is the capability interface implemented by
// SQLiteStore for querying historical arbiter syntheses (issue #1176).
// Callers type-assert to check whether the Store supports pre-warming.
type ArbiterSynthesisReader interface {
	RecentArbiterSyntheses(ctx context.Context, limit int, since time.Time) ([]ArbiterSynthesisRow, error)
}

// RecentArbiterSyntheses returns the most recent arbiter synthesis per
// unique cache key written since the given timestamp (issue #1176).
// limit caps the number of rows returned. The caller (boot-time warmer)
// passes since = now - ArbiterCacheTTL so only entries still within
// the TTL window are loaded.
func (s *SQLiteStore) RecentArbiterSyntheses(ctx context.Context, limit int, since time.Time) ([]ArbiterSynthesisRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, recentArbiterSynthesesTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(queryCtx, recentArbiterSynthesesSQL, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("metrics: recent arbiter syntheses: %w", err)
	}
	defer rows.Close()

	var result []ArbiterSynthesisRow
	for rows.Next() {
		var r ArbiterSynthesisRow
		var tsStr string
		if err := rows.Scan(&r.CacheKeyHex, &r.Synthesis, &tsStr); err != nil {
			return nil, fmt.Errorf("metrics: scan arbiter synthesis: %w", err)
		}
		// SQLite stores DATETIME as a string; parse it back to time.Time.
		if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", tsStr); err == nil {
			r.Timestamp = t
		} else if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			r.Timestamp = t
		} else if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			r.Timestamp = t
		} else {
			// Fall back to raw parse; if it fails, use zero time so the
			// warmer will treat it as stale and skip it.
			r.Timestamp, _ = time.Parse(time.DateTime, tsStr)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: arbiter synthesis rows: %w", err)
	}
	return result, nil
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
	defer rows.Close()
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
			return nil, fmt.Errorf("metrics: scan aggregate: %w", err)
		}
		aggs = append(aggs, r)
	}
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
		if s.pruneStop != nil {
			close(s.pruneStop)
		}
		s.wg.Wait()
	}
	return s.close.Close(func() error {
		if s.db != nil {
			return s.db.Close()
		}
		return nil
	})
}

// Sync is a no-op for SQLiteStore because the database engine handles
// durability internally via its write-ahead log (WAL). This method
// satisfies the telemetry.Recorder interface.
func (s *SQLiteStore) Sync() {}

// Writable checks whether the database is currently reachable by attempting
// a Ping. Returns false when the database is closed or inaccessible.
func (s *SQLiteStore) Writable() bool {
	if s == nil || s.db == nil {
		return false
	}
	return s.db.Ping() == nil
}

// Path returns the configured database path ("" for ":memory:").
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
