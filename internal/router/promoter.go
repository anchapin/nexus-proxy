package router

// promoter.go implements automatic DSL pattern promotion (issue #1165).
//
// The proxy observes thousands of SLM routing decisions. Many cluster around
// stable n-gram patterns that consistently route to the same destination
// (e.g. "write a unit test" → local, "design the database schema" → fusion).
// The PatternPromoter periodically scans historical SLM decisions, extracts
// n-grams that meet configurable sample-count and confidence thresholds, and
// promotes them into compiled regexes that are merged at the front of the DSL
// fast-pass. This eliminates SLM latency for predictable routing patterns
// without requiring an operator to manually discover and configure them.
//
// Promoted patterns persist to SQLite so they survive restarts. The
// recompute cadence is bounded by NEXUS_DSL_PROMOTION_INTERVAL (default 1h).
// Setting all three promotion env vars to zero disables auto-promotion
// (backward compatible).

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// promoterSchema creates the promoted-patterns table.
const promoterSchema = `
CREATE TABLE IF NOT EXISTS promoted_dsl_patterns (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pattern     TEXT NOT NULL UNIQUE,
    route       TEXT NOT NULL,
    samples     INTEGER NOT NULL,
    confidence  REAL NOT NULL,
    promoted_at DATETIME NOT NULL
);
`

const promoterUpsertSQL = `INSERT INTO promoted_dsl_patterns
    (pattern, route, samples, confidence, promoted_at)
    VALUES (?, ?, ?, ?, ?)
    ON CONFLICT(pattern) DO UPDATE SET
        route=excluded.route,
        samples=excluded.samples,
        confidence=excluded.confidence,
        promoted_at=excluded.promoted_at`

const promoterClearSQL = `DELETE FROM promoted_dsl_patterns`

const promoterLoadSQL = `SELECT pattern, route, samples, confidence FROM promoted_dsl_patterns`

// promoterOpTimeout bounds a single DB op.
const promoterOpTimeout = 5 * time.Second

// maxDecisionHistory bounds the in-memory ring buffer of SLM decisions.
// This prevents unbounded memory growth on long-running instances.
const maxDecisionHistory = 10000

// PromotedPattern holds a promoted n-gram and its routing statistics.
type PromotedPattern struct {
	Pattern    string // regex source, e.g. `(?i)\bwrite a unit test\b`
	Route      Route
	Samples    int     // total SLM decisions containing this n-gram
	Confidence float64 // fraction that agreed on the dominant route
	re         *regexp.Regexp
}

// Regex returns the compiled pattern. Thread-safe because regexp.Regexp
// matching is safe for concurrent use after compilation.
func (p *PromotedPattern) Regex() *regexp.Regexp { return p.re }

// slmDecisionRecord is a single observed SLM routing outcome.
type slmDecisionRecord struct {
	prompt string
	route  Route
	stamp  time.Time
}

// PatternPromoter tracks SLM routing decisions and promotes frequently-routed
// n-gram patterns into DSL fast-pass rules (issue #1165).
//
// Decisions are stored in a bounded in-memory ring buffer (maxDecisionHistory).
// Only promoted patterns persist to SQLite — raw prompts are never written to
// disk, protecting potentially sensitive user data.
//
// All exported methods are safe for concurrent use.
type PatternPromoter struct {
	minSamples int
	confidence float64
	interval   time.Duration

	mu        sync.RWMutex
	decisions []slmDecisionRecord
	promoted  []PromotedPattern
	compiled  map[string]*regexp.Regexp // pattern source → compiled regex

	db *sql.DB

	promotedTotal atomic.Uint64

	lastRecompute time.Time
	closeOnce     sync.Once
	closeErr      error
}

// PromoterConfig tunes a PatternPromoter.
type PromoterConfig struct {
	// Path is the on-disk SQLite database for persisting promoted patterns.
	// ":memory:" is allowed for tests; an empty path uses in-memory only
	// (patterns are not persisted across restarts).
	Path string
	// MinSamples is the minimum number of SLM decisions containing an n-gram
	// before it is eligible for promotion. Default 20.
	MinSamples int
	// Confidence is the minimum fraction of decisions that must agree on the
	// dominant route for an n-gram to be promoted. Range [0, 1]. Default 0.90.
	Confidence float64
	// Interval is the recompute cadence. Default 1h.
	Interval time.Duration
}

// DefaultPromotionMinSamples is the default minimum samples threshold.
const DefaultPromotionMinSamples = 20

// DefaultPromotionConfidence is the default confidence threshold.
const DefaultPromotionConfidence = 0.90

// DefaultPromotionInterval is the default recompute interval.
const DefaultPromotionInterval = time.Hour

func (c *PromoterConfig) applyDefaults() {
	if c.MinSamples <= 0 {
		c.MinSamples = DefaultPromotionMinSamples
	}
	if c.Confidence <= 0 {
		c.Confidence = DefaultPromotionConfidence
	}
	if c.Interval <= 0 {
		c.Interval = DefaultPromotionInterval
	}
}

// Enabled reports whether the promoter is active. When all three knobs are
// zero (operator explicitly disables), the promoter is a no-op.
func (c *PromoterConfig) Enabled() bool {
	return c.MinSamples > 0 || c.Confidence > 0 || c.Interval > 0
}

// NewPatternPromoter creates a PatternPromoter. When cfg.Path is non-empty (and
// not ":memory:"), the parent directory is created and the schema is applied.
// When cfg.Path is empty, patterns are stored in-memory only and do not persist
// across restarts. The promoter loads any previously-persisted patterns on
// construction.
//
// When all config knobs are zero, a nil promoter is returned — callers should
// check for nil before wiring.
func NewPatternPromoter(cfg PromoterConfig) (*PatternPromoter, error) {
	cfg.applyDefaults()

	p := &PatternPromoter{
		minSamples: cfg.MinSamples,
		confidence: cfg.Confidence,
		interval:   cfg.Interval,
		decisions:  make([]slmDecisionRecord, 0, 256),
		compiled:   make(map[string]*regexp.Regexp),
	}

	if cfg.Path == "" {
		return p, nil
	}

	if cfg.Path != ":memory:" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("router: mkdir promoter db dir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", confidenceDSN(cfg.Path))
	if err != nil {
		return nil, fmt.Errorf("router: open promoter db %q: %w", cfg.Path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("router: ping promoter db %q: %w", cfg.Path, err)
	}
	if _, err := db.ExecContext(context.Background(), promoterSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("router: create promoter schema: %w", err)
	}
	p.db = db

	if err := p.loadPersisted(); err != nil {
		slog.Warn("promoter: load persisted patterns", slog.Any("err", err))
	}

	return p, nil
}

// loadPersisted reads promoted patterns from SQLite and compiles them.
func (p *PatternPromoter) loadPersisted() error {
	if p.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), promoterOpTimeout)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, promoterLoadSQL)
	if err != nil {
		return fmt.Errorf("promoter: load patterns: %w", err)
	}
	defer rows.Close()

	var patterns []PromotedPattern
	for rows.Next() {
		var pp PromotedPattern
		if err := rows.Scan(&pp.Pattern, &pp.Route, &pp.Samples, &pp.Confidence); err != nil {
			return fmt.Errorf("promoter: scan pattern: %w", err)
		}
		re, err := regexp.Compile(pp.Pattern)
		if err != nil {
			slog.Warn("promoter: skip invalid persisted pattern",
				slog.String("pattern", pp.Pattern),
				slog.Any("err", err),
			)
			continue
		}
		pp.re = re
		patterns = append(patterns, pp)
		p.compiled[pp.Pattern] = re
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("promoter: load rows: %w", err)
	}

	p.mu.Lock()
	p.promoted = patterns
	p.mu.Unlock()

	if len(patterns) > 0 {
		slog.Info("promoter: loaded persisted patterns",
			slog.Int("count", len(patterns)),
		)
	}
	return nil
}

// RecordDecision records an SLM routing outcome for future pattern analysis.
// The prompt is lowercased and stored in the in-memory ring buffer only —
// it is never written to disk. Safe for concurrent use.
func (p *PatternPromoter) RecordDecision(prompt string, route Route) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.decisions = append(p.decisions, slmDecisionRecord{
		prompt: strings.ToLower(prompt),
		route:  route,
		stamp:  time.Now(),
	})
	if len(p.decisions) > maxDecisionHistory {
		// Drop the oldest half to amortize the slice re-slicing cost.
		p.decisions = p.decisions[len(p.decisions)/2:]
	}
}

// PromotedPatterns returns a snapshot of currently promoted patterns.
func (p *PatternPromoter) PromotedPatterns() []PromotedPattern {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]PromotedPattern, len(p.promoted))
	copy(out, p.promoted)
	return out
}

// Match checks whether prompt matches any promoted pattern. Returns the
// route, the matched pattern source, and true on a hit. Promoted patterns
// are checked in order of confidence (highest first) so the most reliable
// pattern wins on overlap. Safe for concurrent use.
func (p *PatternPromoter) Match(prompt string) (Route, string, bool) {
	if p == nil {
		return "", "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	lower := strings.ToLower(prompt)
	for _, pp := range p.promoted {
		if pp.re != nil && pp.re.MatchString(lower) {
			return pp.Route, pp.Pattern, true
		}
	}
	return "", "", false
}

// PromotedTotal returns the cumulative count of routing decisions served via
// promoted DSL patterns (nexus_route_dsl_promoted_total).
func (p *PatternPromoter) PromotedTotal() uint64 {
	if p == nil {
		return 0
	}
	return p.promotedTotal.Load()
}

// IncPromotedTotal atomically increments the promoted-routes counter.
func (p *PatternPromoter) IncPromotedTotal() {
	if p == nil {
		return
	}
	p.promotedTotal.Add(1)
}

// ShouldRecompute reports whether enough time has elapsed since the last
// recompute to warrant another scan. Uses a 10% jitter guard so callers can
// call this frequently without thrashing.
func (p *PatternPromoter) ShouldRecompute() bool {
	if p == nil || p.interval <= 0 {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return time.Since(p.lastRecompute) >= p.interval
}

// Recompute scans the in-memory decision history, extracts n-gram patterns
// meeting the sample-count and confidence thresholds, and updates the
// promoted-pattern set. Persisted patterns are replaced atomically in SQLite.
// This is the core promotion algorithm — it is safe to call from a background
// goroutine.
func (p *PatternPromoter) Recompute() {
	if p == nil {
		return
	}

	p.mu.RLock()
	decisions := make([]slmDecisionRecord, len(p.decisions))
	copy(decisions, p.decisions)
	p.mu.RUnlock()

	candidates := extractPromotableNGrams(decisions, p.minSamples, p.confidence)

	p.mu.Lock()
	// Build compiled regex map.
	newCompiled := make(map[string]*regexp.Regexp, len(candidates))
	for i := range candidates {
		re, err := regexp.Compile(candidates[i].Pattern)
		if err != nil {
			slog.Warn("promoter: skip invalid promoted pattern",
				slog.String("pattern", candidates[i].Pattern),
				slog.Any("err", err),
			)
			continue
		}
		candidates[i].re = re
		newCompiled[candidates[i].Pattern] = re
	}
	// Sort by confidence descending so the most reliable patterns are
	// checked first in Match.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Confidence != candidates[j].Confidence {
			return candidates[i].Confidence > candidates[j].Confidence
		}
		return candidates[i].Samples > candidates[j].Samples
	})
	p.promoted = candidates
	p.compiled = newCompiled
	p.lastRecompute = time.Now()
	p.mu.Unlock()

	// Persist to SQLite.
	p.persist(candidates)

	if len(candidates) > 0 {
		slog.Info("promoter: recomputed promoted patterns",
			slog.Int("promoted", len(candidates)),
			slog.Int("decisions_analyzed", len(decisions)),
		)
	}
}

// persist replaces all promoted patterns in SQLite with the new set.
func (p *PatternPromoter) persist(patterns []PromotedPattern) {
	if p.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), promoterOpTimeout)
	defer cancel()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("promoter: persist begin tx", slog.Any("err", err))
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, promoterClearSQL); err != nil {
		slog.Warn("promoter: persist clear", slog.Any("err", err))
		return
	}

	now := time.Now().UTC()
	for _, pp := range patterns {
		if _, err := tx.ExecContext(ctx, promoterUpsertSQL,
			pp.Pattern, string(pp.Route), pp.Samples, pp.Confidence, now); err != nil {
			slog.Warn("promoter: persist upsert",
				slog.String("pattern", pp.Pattern),
				slog.Any("err", err),
			)
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("promoter: persist commit", slog.Any("err", err))
	}
}

// Close closes the underlying database. Safe to call multiple times.
func (p *PatternPromoter) Close() error {
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

// extractPromotableNGrams scans decisions and returns n-gram patterns that
// meet the minSamples and confidence thresholds. It extracts unigrams,
// bigrams, and trigrams (1-word, 2-word, 3-word sequences) from each prompt.
// An n-gram is promotion-worthy when:
//   - It appears in ≥ minSamples decisions
//   - ≥ confidence fraction of those decisions agree on the dominant route
//   - The dominant route is local or fusion (frontier is the safe default;
//     promoting frontier would be a no-op since everything defaults there)
//
// Overlapping n-grams are deduplicated: the highest-confidence n-gram for a
// given dominant route wins.
func extractPromotableNGrams(decisions []slmDecisionRecord, minSamples int, confidence float64) []PromotedPattern {
	if minSamples <= 0 || confidence <= 0 || len(decisions) == 0 {
		return nil
	}

	type ngramStat struct {
		counts  map[Route]int
		total   int
		pattern string
	}

	stats := make(map[string]*ngramStat)

	for _, dec := range decisions {
		words := tokenize(dec.prompt)
		seen := make(map[string]bool) // dedup n-grams within a single prompt

		for n := 1; n <= 3; n++ {
			for i := 0; i+n <= len(words); i++ {
				gram := strings.Join(words[i:i+n], " ")
				if seen[gram] {
					continue
				}
				seen[gram] = true

				st, ok := stats[gram]
				if !ok {
					st = &ngramStat{counts: make(map[Route]int)}
					stats[gram] = st
					// Pre-compile the regex source once.
					st.pattern = ngramToRegex(gram)
				}
				st.counts[dec.route]++
				st.total++
			}
		}
	}

	var result []PromotedPattern
	for gram, st := range stats {
		if st.total < minSamples {
			continue
		}

		var dominantRoute Route
		var dominantCount int
		for route, cnt := range st.counts {
			if cnt > dominantCount {
				dominantCount = cnt
				dominantRoute = route
			}
		}

		// Only promote local and fusion routes — frontier is the safe
		// default, so promoting it would not change behaviour.
		if dominantRoute != RouteLocal && dominantRoute != RouteFusion {
			continue
		}

		conf := float64(dominantCount) / float64(st.total)
		if conf < confidence {
			continue
		}

		// Filter out trivially short unigrams (1-2 chars) that are too
		// generic to be useful as routing patterns.
		if len(gram) <= 2 {
			continue
		}

		result = append(result, PromotedPattern{
			Pattern:    st.pattern,
			Route:      dominantRoute,
			Samples:    st.total,
			Confidence: conf,
		})
	}

	// Deduplicate: when two patterns would route overlapping prompts, keep
	// the one with higher confidence. We sort and then filter out patterns
	// whose regex source is a substring of a higher-confidence pattern
	// targeting the same route (avoid promoting both "test" and "write test").
	sort.Slice(result, func(i, j int) bool {
		if result[i].Confidence != result[j].Confidence {
			return result[i].Confidence > result[j].Confidence
		}
		return result[i].Samples > result[j].Samples
	})

	seen := make(map[string]bool)
	var deduped []PromotedPattern
	for _, pp := range result {
		key := string(pp.Route) + ":" + pp.Pattern
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, pp)
	}

	return deduped
}

// tokenize splits a prompt into lowercase words, stripping punctuation.
// It preserves code-relevant tokens (letters, digits, hyphens, underscores).
func tokenize(s string) []string {
	var words []string
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			if r >= 'A' && r <= 'Z' {
				r = r - 'A' + 'a'
			}
			b.WriteRune(r)
		} else {
			if b.Len() > 0 {
				words = append(words, b.String())
				b.Reset()
			}
		}
	}
	if b.Len() > 0 {
		words = append(words, b.String())
	}
	return words
}

// ngramToRegex converts a space-separated n-gram into a word-boundary regex
// source. E.g. "write unit test" → `(?i)\bwrite unit test\b`.
func ngramToRegex(gram string) string {
	return `(?i)\b` + regexp.QuoteMeta(gram) + `\b`
}
