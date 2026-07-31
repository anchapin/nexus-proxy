package router

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ProviderStats is the per-provider snapshot the selector scores. It is
// populated by an external data source — typically the metrics store
// (internal/metrics.ProviderStats) — and consumed by ProviderSelector.
//
// Name identifies the provider as the chat handler knows it. The current
// implementation keys on the metrics row's `model` column (which the chat
// handler sets to the upstream model name); callers that distinguish
// "frontier" and "zai" by model can map via FrontierProvider.Model before
// calling SelectFrontier.
//
// P50LatencyMs / P95LatencyMs are millisecond percentiles over the
// observation window. P50 is the steady-state latency the selector
// scores against; P95 is the long-tail latency. ProviderSelector.TailWeight
// blends P95 into the effective latency — see DefaultSelectorTailWeight
// below for the byte-for-byte preserving default (issue #450).
//
// AvgCostUSD is the average per-request USD cost observed over the
// window. The selector treats it as a tie-breaker / weight — providers
// with comparable latency are penalised in proportion to observed cost.
//
// SampleCount is the number of requests the stats were computed from.
// SelectFrontier excludes providers below MinSamples so the cold-start
// path is deterministic (no flapping until each provider has a real
// track record).
//
// ErrorRate is the fraction of requests that landed in the `requests`
// table with a non-empty `error` column. SelectFrontier excludes
// providers above MaxErrorRate — a provider that is up but flaky is
// worse than one that is slow but reliable.
type ProviderStats struct {
	Name         string
	P50LatencyMs int64
	P95LatencyMs int64
	AvgCostUSD   float64
	SampleCount  int
	ErrorRate    float64
}

// Defaults for the selector thresholds. Operators can override via
// ProviderSelector fields; tests use the package constants directly so
// the assertions remain stable across renames.
const (
	// DefaultSelectorMinSamples is the per-provider observation floor
	// below which the provider is treated as "no data". With fewer
	// samples the selector returns "" and the caller falls back to
	// the first configured provider (deterministic, no flapping on
	// cold start).
	DefaultSelectorMinSamples = 5

	// DefaultSelectorMaxErrorRate is the per-provider error ceiling
	// above which the provider is excluded from selection. 0.3
	// (30%) matches the issue's proposed penalty: a provider that
	// fails more than three times out of ten is not worth picking
	// even when its latency is the best of the lot.
	DefaultSelectorMaxErrorRate = 0.3

	// DefaultSelectorWindow is the look-back window the selector
	// assumes when querying the data source. Matches the issue's
	// one-hour default; main.go passes it through from config so an
	// operator can lengthen or shorten the observation period.
	DefaultSelectorWindow = time.Hour

	// DefaultSelectorRefreshInterval is how often the ProviderStatsCache
	// background goroutine re-queries the data source. The chat
	// hot path always reads Snapshot(), so the refresh cadence is the
	// only thing this knob tunes.
	DefaultSelectorRefreshInterval = 60 * time.Second

	// DefaultSelectorTailWeight (issue #450) is the blend factor
	// applied to (P95 - P50) when computing effective latency. The
	// zero default preserves the legacy P50-only scoring formula
	// byte-for-byte — score = 1 / (p50 * (cost + epsilon)). A
	// nonzero weight penalises providers whose long-tail stalls
	// drag the user-visible completion time, while leaving the
	// relative ranking of equal-tail providers untouched. Values
	// outside [0,1] are not enforced here; the config loader
	// rejects them at boot so a misconfigured operator gets a clear
	// error rather than silent re-ranking.
	DefaultSelectorTailWeight = 0.0

	// selectorEpsilon is added to the cost weight so a provider that
	// has served zero cost (e.g. all requests happened to be free
	// local traffic) is still scored deterministically — the divisor
	// is never zero.
	selectorEpsilon = 1e-9
)

// ProviderSelector scores []ProviderStats and returns the best provider
// by latency-cost-adjusted score. It is pure logic: no DB, no HTTP,
// no goroutine. The chat handler holds one and calls SelectFrontier
// per request when more than one frontier provider is configured.
//
// The scoring formula is the issue's:
//
//	score = 1.0 / (effective_latency_ms * (avg_cost_usd + epsilon))
//
// where effective_latency_ms = p50 + tail_weight * max(0, p95 - p50).
//
// Higher score = better. Lower latency AND lower cost raise the score.
// Providers with SampleCount < MinSamples or ErrorRate > MaxErrorRate
// are excluded. When all providers are excluded (or the input slice is
// empty) SelectFrontier returns "" so the caller can fall back to the
// first configured provider — deterministic, no flapping on cold start.
type ProviderSelector struct {
	// MinSamples is the per-provider observation floor. Zero falls
	// back to DefaultSelectorMinSamples.
	MinSamples int

	// MaxErrorRate is the per-provider error ceiling. Negative
	// values fall back to DefaultSelectorMaxErrorRate; zero means
	// "exclude any provider with errors"; values >=1 effectively
	// exclude nothing (ErrorRate is bounded at 1.0).
	MaxErrorRate float64

	// TailWeight (issue #450) blends P95 into the effective latency
	// used for scoring. 0 disables the blend entirely (byte-for-byte
	// legacy ordering); 1 uses (P50 + (P95 - P50)) = P95 as the
	// effective latency. The config loader rejects values outside
	// [0,1] at boot; negative values here are treated as 0 so a
	// misconfigured selector never inverts the ranking.
	TailWeight float64
}

// NewProviderSelector constructs a selector with default thresholds.
// Pass the struct fields directly when an operator has overridden the
// defaults via env vars.
func NewProviderSelector() *ProviderSelector {
	return &ProviderSelector{
		MinSamples:   DefaultSelectorMinSamples,
		MaxErrorRate: DefaultSelectorMaxErrorRate,
		TailWeight:   DefaultSelectorTailWeight,
	}
}

// SelectFrontier scores stats and returns the name of the best provider.
// It returns "" when no provider qualifies (insufficient data, every
// provider over the error ceiling, or empty input) — the caller should
// fall back to the first configured provider. The returned ProviderStats
// is the entry that won, or the zero value when no provider qualified.
//
// The function is deterministic: ties are broken by name (ascending)
// so the same input always produces the same winner. Tests rely on
// this property to assert scoring without flaky ordering.
func (s *ProviderSelector) SelectFrontier(stats []ProviderStats) (string, ProviderStats) {
	minSamples := s.MinSamples
	if minSamples <= 0 {
		minSamples = DefaultSelectorMinSamples
	}
	maxErr := s.MaxErrorRate
	// Negative values fall back to the package default. Zero is
	// a legitimate "exclude any provider with errors" knob, so it
	// passes through unchanged.
	if maxErr < 0 {
		maxErr = DefaultSelectorMaxErrorRate
	}
	// TailWeight is bounded at [0,1] by the config loader. Treat
	// a negative struct value as the legacy default rather than
	// silently inverting the ranking; values > 1 are accepted as-is
	// so a deliberate experimental setting (e.g. emphasising the
	// tail more strongly) still works.
	tailWeight := s.TailWeight
	if tailWeight < 0 {
		tailWeight = DefaultSelectorTailWeight
	}

	// Filter then sort: the sort is on (score desc, name asc) so
	// ties are broken deterministically. We copy the filtered slice
	// rather than sorting the input to keep SelectFrontier
	// side-effect-free for callers that re-use the slice.
	filtered := make([]ProviderStats, 0, len(stats))
	for _, p := range stats {
		if p.SampleCount < minSamples {
			continue
		}
		if p.ErrorRate > maxErr {
			continue
		}
		if p.P50LatencyMs <= 0 {
			// A zero latency is almost certainly a data
			// anomaly (request never completed, or all
			// observations came from a cache hit). Skip —
			// the cost-adjusted score would be near
			// infinite and could mask real differences.
			continue
		}
		filtered = append(filtered, p)
	}

	if len(filtered) == 0 {
		return "", ProviderStats{}
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		si := providerScore(filtered[i], tailWeight)
		sj := providerScore(filtered[j], tailWeight)
		if si != sj {
			return si > sj
		}
		// Tie-break by name ascending so deterministic picks
		// surface in the slog line without operator
		// confusion.
		return filtered[i].Name < filtered[j].Name
	})

	winner := filtered[0]
	return winner.Name, winner
}

// providerScore returns the cost-adjusted latency score for p.
// Higher = better. Implements the issue's formula:
//
//	score = 1.0 / (effective_latency_ms * (avg_cost_usd + epsilon))
//
// where effective_latency_ms = p50 + tail_weight * max(0, p95 - p50).
//
// The selectorEpsilon keeps the divisor strictly positive even when a
// provider observed zero cost. When tailWeight == 0 the formula
// collapses to the legacy P50-only score so an operator who never
// sets the env var sees the original ordering byte-for-byte. Callers
// should not invoke this directly — it is exported only via the
// package's test surface.
func providerScore(p ProviderStats, tailWeight float64) float64 {
	p50 := float64(p.P50LatencyMs)
	tail := float64(p.P95LatencyMs - p.P50LatencyMs)
	if tail < 0 {
		// P95 below P50 is a data anomaly (e.g. the percentile
		// query returned fewer samples for P95 than P50).
		// Treat as zero so the blend can only penalise, never
		// reward, a noisy tail.
		tail = 0
	}
	effective := p50 + tailWeight*tail
	return 1.0 / (effective * (p.AvgCostUSD + selectorEpsilon))
}

// ProviderStatsSource is the data source the ProviderStatsCache reads
// from. Implementations must be safe for concurrent use: the cache
// calls Source.ProviderStats from a single background goroutine, so
// the only required concurrency is "the metrics store may be
// reading/writing from other goroutines at the same time".
type ProviderStatsSource interface {
	// ProviderStats returns the per-provider snapshot for the
	// window ending at now (i.e. since = now - window). Implementations
	// should bound their own query latency and return an error
	// rather than block the cache.
	ProviderStats(ctx context.Context, since time.Time) ([]ProviderStats, error)
}

// ErrSelectorDisabled is returned by ProviderStatsCache.Snapshot when
// no data source has been wired. Callers should treat it as "no
// recommendation; use the default provider" — the same path as
// "insufficient data".
var ErrSelectorDisabled = errors.New("router: provider selector disabled (no source)")

// ProviderStatsCache holds the latest snapshot of provider stats and
// refreshes it on a fixed cadence from a ProviderStatsSource. The chat
// handler calls Snapshot() on every route=frontier request (when more
// than one provider is configured); the cache absorbs the cost of the
// underlying SQL aggregation so the hot path is just a struct copy.
//
// Run starts the background refresh loop and blocks until ctx is
// cancelled. Stop is a convenience for tests that want to drive
// Refresh directly.
type ProviderStatsCache struct {
	selector *ProviderSelector
	source   ProviderStatsSource
	window   time.Duration
	refresh  time.Duration

	mu        sync.RWMutex
	stats     []ProviderStats
	lastError error
	updated   time.Time
}

// NewProviderStatsCache constructs a cache. Pass nil for source to
// disable the cache entirely (Snapshot returns ErrSelectorDisabled).
// Zero window / refresh values fall back to the package defaults.
func NewProviderStatsCache(sel *ProviderSelector, source ProviderStatsSource, window, refresh time.Duration) *ProviderStatsCache {
	if sel == nil {
		sel = NewProviderSelector()
	}
	if window <= 0 {
		window = DefaultSelectorWindow
	}
	if refresh <= 0 {
		refresh = DefaultSelectorRefreshInterval
	}
	return &ProviderStatsCache{
		selector: sel,
		source:   source,
		window:   window,
		refresh:  refresh,
	}
}

// Refresh pulls a fresh snapshot from the source and stores it. Errors
// are recorded but do not clear the previous snapshot — a transient
// SQL hiccup should not blank the cache. Returns the last error (if
// any) so callers that drive Refresh manually (tests) can assert.
func (c *ProviderStatsCache) Refresh(ctx context.Context) error {
	if c.source == nil {
		c.mu.Lock()
		c.lastError = ErrSelectorDisabled
		c.mu.Unlock()
		return ErrSelectorDisabled
	}
	since := time.Now().UTC().Add(-c.window)
	stats, err := c.source.ProviderStats(ctx, since)
	c.mu.Lock()
	if err != nil {
		c.lastError = err
		c.mu.Unlock()
		return err
	}
	c.stats = stats
	c.lastError = nil
	c.updated = time.Now().UTC()
	c.mu.Unlock()
	return nil
}

// Snapshot returns the latest cached stats without consulting the
// source. Safe for concurrent use; the chat hot path calls this. An
// empty slice means either "Refresh has not run yet" or "the source
// returned nothing"; both paths leave the caller to fall back to its
// default provider.
func (c *ProviderStatsCache) Snapshot() []ProviderStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.stats) == 0 {
		return nil
	}
	out := make([]ProviderStats, len(c.stats))
	copy(out, c.stats)
	return out
}

// LastError returns the most recent error recorded by Refresh. Exposed
// for /healthz-style diagnostics; not consulted by the chat handler.
func (c *ProviderStatsCache) LastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastError
}

// LastUpdated returns the timestamp of the most recent successful
// refresh. Returns the zero time when Refresh has not succeeded.
func (c *ProviderStatsCache) LastUpdated() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.updated
}

// SelectFrontier applies the configured selector to the cached stats
// and returns the recommended provider name (or "" when no provider
// qualifies). Wraps ProviderSelector.SelectFrontier with the cached
// snapshot so the chat handler never sees the cache's storage type.
func (c *ProviderStatsCache) SelectFrontier() (string, ProviderStats) {
	return c.selector.SelectFrontier(c.Snapshot())
}

// Run drives Refresh on the configured cadence until ctx is cancelled.
// Intended to be invoked once from main.go as a goroutine. The first
// refresh fires immediately so the chat handler has data on the very
// first request after boot.
func (c *ProviderStatsCache) Run(ctx context.Context) {
	if c.source == nil {
		return
	}
	// Prime the cache synchronously so the first request after boot
	// does not race against a background refresh.
	_ = c.Refresh(ctx)
	t := time.NewTicker(c.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Refresh(ctx)
		}
	}
}
