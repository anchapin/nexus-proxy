package router

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	// Pure-Go SQLite driver (no CGo). The same driver backs
	// internal/metrics; importing it here registers the "sqlite"
	// driver name. See modernc.org/sqlite.
	_ "modernc.org/sqlite"
)

// confidenceSchema is the v1 schema for the routing-confidence table.
// One row per judged local outcome. The index on (category, timestamp)
// keeps the sliding-window aggregate cheap — it is the only read pattern.
const confidenceSchema = `
CREATE TABLE IF NOT EXISTS routing_outcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    category  TEXT NOT NULL,
    route     TEXT NOT NULL,
    score     INTEGER NOT NULL,
    timestamp DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_routing_outcomes_cat_ts
    ON routing_outcomes(category, timestamp);
`

const insertOutcomeSQL = `INSERT INTO routing_outcomes
    (category, route, score, timestamp) VALUES (?, ?, ?, ?)`

// confidenceQuerySQL aggregates the fraction of recent local outcomes for
// a category that scored at or above the success threshold. COUNT(*) is
// returned alongside so the caller can enforce the minimum-samples gate.
const confidenceQuerySQL = `
SELECT
    COUNT(*),
    COALESCE(AVG(CASE WHEN score >= ? THEN 1.0 ELSE 0.0 END), 0)
FROM routing_outcomes
WHERE category = ? AND route = ? AND timestamp > ?`

// confidenceOpTimeout bounds a single DB op. Aggregates over the small
// indexed table finish in microseconds; the timeout only guards a
// pathological disk stall so RecordOutcome / LocalConfidence never pin a
// goroutine.
const confidenceOpTimeout = 5 * time.Second

// SQLiteConfidenceStore is the production ConfidenceStore. Writes and reads
// are synchronous against a single-connection *sql.DB — the volume is low
// (only ~10% of local requests are judged) so a background drain goroutine
// is unnecessary. All exported methods are safe for concurrent use.
//
// The store degrades gracefully: any DB error on RecordOutcome is logged
// and dropped (the outcome is best-effort telemetry), and any error on
// LocalConfidence returns NeutralConfidence so routing falls back to the
// pre-issue-47 behaviour.
type SQLiteConfidenceStore struct {
	db           *sql.DB
	path         string
	successScore int
	minSamples   int
	window       time.Duration

	closeOnce sync.Once
	closeErr  error
}

// ConfidenceConfig tunes a SQLiteConfidenceStore. Zero values get safe
// defaults applied by OpenConfidenceStore.
type ConfidenceConfig struct {
	// Path is the on-disk SQLite database. ":memory:" is allowed for
	// tests; an empty path is rejected.
	Path string
	// SuccessScore is the judge score (1..5) at/above which an outcome
	// counts as a success. Defaults to DefaultSuccessScore.
	SuccessScore int
	// MinSamples is the minimum recent outcomes a category needs before
	// LocalConfidence reports a non-neutral value. Defaults to
	// DefaultConfidenceMinSamples.
	MinSamples int
	// Window is the sliding window; outcomes older than now-Window are
	// ignored. Defaults to 7 days.
	Window time.Duration
}

func (c *ConfidenceConfig) applyDefaults() {
	if c.SuccessScore <= 0 {
		c.SuccessScore = DefaultSuccessScore
	}
	if c.MinSamples <= 0 {
		c.MinSamples = DefaultConfidenceMinSamples
	}
	if c.Window <= 0 {
		c.Window = 168 * time.Hour
	}
}

// OpenConfidenceStore opens (creating on demand) the SQLite database at
// cfg.Path and returns a ready ConfidenceStore. The parent directory is
// created for on-disk paths. An empty path is rejected.
func OpenConfidenceStore(cfg ConfidenceConfig) (*SQLiteConfidenceStore, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("router: empty confidence db path")
	}
	cfg.applyDefaults()

	if cfg.Path != ":memory:" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("router: mkdir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", confidenceDSN(cfg.Path))
	if err != nil {
		return nil, fmt.Errorf("router: open confidence db %q: %w", cfg.Path, err)
	}
	// Single writer connection keeps SQLite happy without a dedicated
	// drain goroutine; WAL (see DSN) still lets the read path proceed.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("router: ping confidence db %q: %w", cfg.Path, err)
	}
	if _, err := db.ExecContext(context.Background(), confidenceSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("router: create confidence schema: %w", err)
	}

	return &SQLiteConfidenceStore{
		db:           db,
		path:         cfg.Path,
		successScore: cfg.SuccessScore,
		minSamples:   cfg.MinSamples,
		window:       cfg.Window,
	}, nil
}

// confidenceDSN mirrors the metrics store's DSN: WAL journalling for
// concurrent readers, a bounded busy timeout, and NORMAL synchronous
// mode (this is best-effort telemetry, not durable accounting).
func confidenceDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:?mode=memory&cache=shared"
	}
	return fmt.Sprintf(
		"file:%s?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
		path,
	)
}

// Path returns the on-disk path the store was opened with.
func (s *SQLiteConfidenceStore) Path() string { return s.path }

// RecordOutcome implements ConfidenceStore. Best-effort: DB errors are
// logged and dropped. Scores outside the 1..5 judge range are ignored so a
// parse-failure JudgeScore (Score == 0) never skews the aggregate.
func (s *SQLiteConfidenceStore) RecordOutcome(category string, route Route, judgeScore int) {
	s.recordAt(category, route, judgeScore, time.Now().UTC())
}

// recordAt is RecordOutcome with an explicit timestamp. It exists so tests
// can seed old rows and exercise the sliding-window expiry path.
func (s *SQLiteConfidenceStore) recordAt(category string, route Route, judgeScore int, ts time.Time) {
	if s == nil || s.db == nil {
		return
	}
	if judgeScore < 1 || judgeScore > 5 {
		return
	}
	if category == "" {
		category = CategoryOther
	}
	ctx, cancel := context.WithTimeout(context.Background(), confidenceOpTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, insertOutcomeSQL,
		category, string(route), judgeScore, ts.UTC()); err != nil {
		slog.Warn("confidence: record outcome",
			slog.String("category", category),
			slog.String("route", string(route)),
			slog.Any("err", err),
		)
	}
}

// LocalConfidence implements ConfidenceStore. Returns NeutralConfidence
// when fewer than minSamples recent local outcomes exist for the category
// or when the query fails.
func (s *SQLiteConfidenceStore) LocalConfidence(category string) float64 {
	if s == nil || s.db == nil {
		return NeutralConfidence
	}
	if category == "" {
		category = CategoryOther
	}
	cutoff := time.Now().UTC().Add(-s.window)

	ctx, cancel := context.WithTimeout(context.Background(), confidenceOpTimeout)
	defer cancel()

	var (
		count int
		frac  float64
	)
	row := s.db.QueryRowContext(ctx, confidenceQuerySQL,
		s.successScore, category, string(RouteLocal), cutoff)
	if err := row.Scan(&count, &frac); err != nil {
		slog.Warn("confidence: query",
			slog.String("category", category),
			slog.Any("err", err),
		)
		return NeutralConfidence
	}
	if count < s.minSamples {
		return NeutralConfidence
	}
	return frac
}

// Close closes the underlying database. Safe to call multiple times.
func (s *SQLiteConfidenceStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
	})
	return s.closeErr
}

// Compile-time guard: SQLiteConfidenceStore satisfies ConfidenceStore.
var _ ConfidenceStore = (*SQLiteConfidenceStore)(nil)
