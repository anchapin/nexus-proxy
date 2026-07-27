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
// logged and returned so the caller can decide. Scores outside the 1..5
// judge range are ignored (return nil) so a parse-failure JudgeScore
// (Score == 0) never skews the aggregate. An empty category is rejected
// with an error instead of being silently coerced to CategoryOther so
// upstream RecordOutcome bugs are visible (issue #591).
func (s *SQLiteConfidenceStore) RecordOutcome(category string, route Route, judgeScore int) error {
	return s.recordAt(category, route, judgeScore, time.Now().UTC())
}

// recordAt is RecordOutcome with an explicit timestamp. It exists so tests
// can seed old rows and exercise the sliding-window expiry path.
func (s *SQLiteConfidenceStore) recordAt(category string, route Route, judgeScore int, ts time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	if category == "" {
		return fmt.Errorf("router: empty category in recordAt (caller failed to categorize the prompt)")
	}
	if judgeScore < 1 || judgeScore > 5 {
		return nil
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
		return err
	}
	return nil
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

// CategoryStats holds the raw aggregate for one task category in the
// sliding window. It is the unfiltered view used by the stats CLI —
// the minSamples gate that LocalConfidence applies internally is NOT
// enforced here so operators can see whether a category has any data
// at all.
type CategoryStats struct {
	Category   string
	Samples    int
	Confidence float64 // fraction of samples scoring >= successScore; 0 if no samples
}

// WindowStats returns per-category aggregates for every fixed Category*
// constant in the sliding window. Empty categories (no rows) are returned
// with Samples=0 and Confidence=0 so the stats table can show them with
// a "—" placeholder. Errors are logged and skipped — a corrupt row does
// not poison the whole table.
func (s *SQLiteConfidenceStore) WindowStats() []CategoryStats {
	if s == nil || s.db == nil {
		return nil
	}
	cutoff := time.Now().UTC().Add(-s.window)

	ctx, cancel := context.WithTimeout(context.Background(), confidenceOpTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT
		    category,
		    COUNT(*) AS samples,
		    COALESCE(AVG(CASE WHEN score >= ? THEN 1.0 ELSE 0.0 END), 0) AS confidence
		FROM routing_outcomes
		WHERE route = ? AND timestamp > ?
		GROUP BY category`,
		s.successScore, string(RouteLocal), cutoff)
	if err != nil {
		slog.Warn("confidence: window stats query", slog.Any("err", err))
		return nil
	}
	defer rows.Close()

	// Map from category name → stats. Missing categories get zeroed entries.
	stats := make(map[string]CategoryStats)
	for rows.Next() {
		var cs CategoryStats
		if err := rows.Scan(&cs.Category, &cs.Samples, &cs.Confidence); err != nil {
			slog.Warn("confidence: window stats scan", slog.Any("err", err))
			continue
		}
		stats[cs.Category] = cs
	}
	if err := rows.Err(); err != nil {
		slog.Warn("confidence: window stats rows", slog.Any("err", err))
	}

	// Emit every fixed category so empty ones appear with 0 samples.
	all := make([]CategoryStats, 0, len(categoryKeywords)+1)
	for _, kw := range categoryKeywords {
		if cs, ok := stats[kw.category]; ok {
			all = append(all, cs)
		} else {
			all = append(all, CategoryStats{Category: kw.category})
		}
	}
	// "other" is the fallback; include it last.
	if cs, ok := stats[CategoryOther]; ok {
		all = append(all, cs)
	} else {
		all = append(all, CategoryStats{Category: CategoryOther})
	}
	return all
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
