package router

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestProviderSelector_SelectFrontier(t *testing.T) {
	cases := []struct {
		name     string
		selector *ProviderSelector
		stats    []ProviderStats
		wantName string
		wantZero bool
	}{
		{
			name:     "empty stats returns empty",
			selector: NewProviderSelector(),
			stats:    nil,
			wantZero: true,
		},
		{
			name:     "single provider with sufficient samples wins",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "only", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
			},
			wantName: "only",
		},
		{
			name:     "lower latency wins when costs are equal",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "slow", SampleCount: 10, P50LatencyMs: 1000, AvgCostUSD: 0.001},
				{Name: "fast", SampleCount: 10, P50LatencyMs: 200, AvgCostUSD: 0.001},
			},
			wantName: "fast",
		},
		{
			name:     "lower cost wins when latencies are equal",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "expensive", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.01},
				{Name: "cheap", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
			},
			wantName: "cheap",
		},
		{
			name:     "providers below MinSamples are excluded",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "data-poor", SampleCount: 2, P50LatencyMs: 100, AvgCostUSD: 0.001},
				{Name: "data-rich", SampleCount: 50, P50LatencyMs: 800, AvgCostUSD: 0.001},
			},
			wantName: "data-rich",
		},
		{
			name:     "all providers below MinSamples returns empty",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "a", SampleCount: 1, P50LatencyMs: 100, AvgCostUSD: 0.001},
				{Name: "b", SampleCount: 4, P50LatencyMs: 200, AvgCostUSD: 0.001},
			},
			wantZero: true,
		},
		{
			name:     "providers above MaxErrorRate are excluded",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "flaky", SampleCount: 50, P50LatencyMs: 100, AvgCostUSD: 0.001, ErrorRate: 0.5},
				{Name: "stable", SampleCount: 50, P50LatencyMs: 800, AvgCostUSD: 0.001, ErrorRate: 0.05},
			},
			wantName: "stable",
		},
		{
			name:     "all providers above MaxErrorRate returns empty",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "a", SampleCount: 50, P50LatencyMs: 100, AvgCostUSD: 0.001, ErrorRate: 0.4},
				{Name: "b", SampleCount: 50, P50LatencyMs: 200, AvgCostUSD: 0.001, ErrorRate: 0.6},
			},
			wantZero: true,
		},
		{
			name:     "providers with zero latency are excluded",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "no-latency", SampleCount: 50, P50LatencyMs: 0, AvgCostUSD: 0.001},
				{Name: "real", SampleCount: 50, P50LatencyMs: 800, AvgCostUSD: 0.001},
			},
			wantName: "real",
		},
		{
			name:     "tied scores break on name ascending",
			selector: NewProviderSelector(),
			stats: []ProviderStats{
				{Name: "zeta", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
				{Name: "alpha", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
				{Name: "mu", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
			},
			wantName: "alpha",
		},
		{
			// ErrorRate is bounded at 1.0, so MaxErrorRate=1.0
			// excludes nothing. This documents the boundary
			// rather than the useful behaviour — operators who
			// want "exclude on any error" should set
			// MaxErrorRate to 0 (the filter is strict
			// greater-than).
			name:     "MaxErrorRate = 1 excludes nothing",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 1.0},
			stats: []ProviderStats{
				{Name: "clean", SampleCount: 50, P50LatencyMs: 1000, AvgCostUSD: 0.001, ErrorRate: 0.0},
				{Name: "tiny-errors", SampleCount: 50, P50LatencyMs: 100, AvgCostUSD: 0.001, ErrorRate: 0.01},
			},
			wantName: "tiny-errors",
		},
		{
			// MaxErrorRate = 0 excludes every provider whose
			// ErrorRate is strictly > 0 — i.e. anyone with any
			// errors at all.
			name:     "MaxErrorRate = 0 excludes providers with any errors",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.0},
			stats: []ProviderStats{
				{Name: "clean", SampleCount: 50, P50LatencyMs: 1000, AvgCostUSD: 0.001, ErrorRate: 0.0},
				{Name: "tiny-errors", SampleCount: 50, P50LatencyMs: 100, AvgCostUSD: 0.001, ErrorRate: 0.01},
			},
			wantName: "clean",
		},
		{
			name:     "zero MinSamples falls back to default",
			selector: &ProviderSelector{MinSamples: 0, MaxErrorRate: 0.3},
			stats: []ProviderStats{
				// Below DefaultSelectorMinSamples=5; should be excluded.
				{Name: "young", SampleCount: 4, P50LatencyMs: 100, AvgCostUSD: 0.001},
				{Name: "old", SampleCount: 50, P50LatencyMs: 800, AvgCostUSD: 0.001},
			},
			wantName: "old",
		},
		{
			name:     "negative MaxErrorRate falls back to default 0.3",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: -1},
			stats: []ProviderStats{
				{Name: "a", SampleCount: 50, P50LatencyMs: 100, AvgCostUSD: 0.001, ErrorRate: 0.4},
				{Name: "b", SampleCount: 50, P50LatencyMs: 800, AvgCostUSD: 0.001, ErrorRate: 0.2},
			},
			wantName: "b",
		},
		// Issue #450: TailWeight blends P95 into effective latency.
		// The legacy P50-only ordering must remain byte-for-byte
		// stable when TailWeight is the package default (0). The
		// case below mirrors "lower latency wins when costs are
		// equal" but with both providers carrying identical
		// P95==P50 (no tail), confirming that the new field does
		// not perturb the existing ranking.
		{
			name:     "TailWeight 0 preserves legacy P50-only ordering",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: 0},
			stats: []ProviderStats{
				{Name: "slow", SampleCount: 10, P50LatencyMs: 1000, P95LatencyMs: 1000, AvgCostUSD: 0.001},
				{Name: "fast", SampleCount: 10, P50LatencyMs: 200, P95LatencyMs: 200, AvgCostUSD: 0.001},
			},
			wantName: "fast",
		},
		{
			// A provider with severe long-tail stalls should not
			// rank identically to a consistent provider with the
			// same median. With TailWeight=0 they are tied on
			// P50; with TailWeight=1 the noisy-tail provider loses
			// outright. The synthetic high-P95 dataset is
			// deliberately extreme (P95 = 10x P50) so any
			// non-zero weight changes the winner.
			name:     "TailWeight 1 penalises synthetic high-P95 provider",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: 1.0},
			stats: []ProviderStats{
				{Name: "noisy", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001},
				{Name: "steady", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 400, AvgCostUSD: 0.001},
			},
			wantName: "steady",
		},
		{
			// TailWeight=0 must leave the noisy provider in the
			// same tied position as steady. Sort.SliceStable
			// preserves input order for equal scores, but the
			// name-ascending tiebreaker is what actually picks
			// the winner here.
			name:     "TailWeight 0 ties the synthetic high-P95 pair",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: 0},
			stats: []ProviderStats{
				{Name: "noisy", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001},
				{Name: "steady", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 400, AvgCostUSD: 0.001},
			},
			wantName: "noisy", // name-ascending tiebreaker; tied on P50+cost
		},
		{
			// P95 below P50 (data anomaly — fewer samples in the
			// percentile query than the median) must never
			// improve a provider's score. The blend uses
			// max(0, P95 - P50) so the effective latency stays
			// at P50.
			name:     "P95 below P50 is treated as zero tail",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: 1.0},
			stats: []ProviderStats{
				{Name: "anomalous", SampleCount: 10, P50LatencyMs: 800, P95LatencyMs: 200, AvgCostUSD: 0.001},
				{Name: "normal", SampleCount: 10, P50LatencyMs: 800, P95LatencyMs: 800, AvgCostUSD: 0.001},
			},
			wantName: "anomalous", // both score 1/(800 * cost); name-ascending tiebreak
		},
		{
			// Negative TailWeight (e.g. a struct literal that
			// bypassed config validation) must be coerced to the
			// default 0 — never invert the ranking.
			name:     "negative TailWeight falls back to default 0",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: -0.5},
			stats: []ProviderStats{
				{Name: "noisy", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001},
				{Name: "steady", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 400, AvgCostUSD: 0.001},
			},
			wantName: "noisy", // same as TailWeight=0 tiebreak
		},
		{
			// Mid-range TailWeight (0.5) should pick the steady
			// provider because (P50 + 0.5 * (P95 - P50)) makes the
			// noisy provider's effective latency twice as large.
			name:     "TailWeight 0.5 prefers steady over high-P95",
			selector: &ProviderSelector{MinSamples: 5, MaxErrorRate: 0.3, TailWeight: 0.5},
			stats: []ProviderStats{
				{Name: "noisy", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 1000, AvgCostUSD: 0.001},
				{Name: "steady", SampleCount: 10, P50LatencyMs: 400, P95LatencyMs: 400, AvgCostUSD: 0.001},
			},
			wantName: "steady",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotStats := tc.selector.SelectFrontier(tc.stats)
			if tc.wantZero {
				if gotName != "" {
					t.Errorf("name = %q, want \"\"", gotName)
				}
				if gotStats != (ProviderStats{}) {
					t.Errorf("stats = %+v, want zero", gotStats)
				}
				return
			}
			if gotName != tc.wantName {
				t.Errorf("name = %q, want %q", gotName, tc.wantName)
			}
			if gotStats.Name != tc.wantName {
				t.Errorf("stats.Name = %q, want %q", gotStats.Name, tc.wantName)
			}
		})
	}
}

func TestProviderScore_FormulaAndLegacyOrdering(t *testing.T) {
	// Issue #450: providerScore must collapse to the legacy P50-only
	// formula when tailWeight is 0, and blend P95 into effective
	// latency when tailWeight > 0. The legacy score is computed
	// independently below so any drift in either formula is caught
	// immediately.
	legacy := func(p ProviderStats) float64 {
		return 1.0 / (float64(p.P50LatencyMs) * (p.AvgCostUSD + selectorEpsilon))
	}

	cases := []struct {
		name       string
		stats      ProviderStats
		tailWeight float64
		wantScore  float64 // expected; tolerance handled inside the loop
	}{
		{
			name:       "tailWeight 0 collapses to legacy P50-only score",
			stats:      ProviderStats{P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001},
			tailWeight: 0,
			wantScore:  legacy(ProviderStats{P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001}),
		},
		{
			name:       "tailWeight 1 uses full P95 as effective latency",
			stats:      ProviderStats{P50LatencyMs: 400, P95LatencyMs: 4000, AvgCostUSD: 0.001},
			tailWeight: 1.0,
			wantScore:  1.0 / (4000 * (0.001 + selectorEpsilon)),
		},
		{
			name:       "tailWeight 0.5 blends P50 and P95 evenly",
			stats:      ProviderStats{P50LatencyMs: 400, P95LatencyMs: 1000, AvgCostUSD: 0.001},
			tailWeight: 0.5,
			wantScore:  1.0 / ((400 + 0.5*(1000-400)) * (0.001 + selectorEpsilon)),
		},
		{
			// P95 < P50 (anomalous data) clamps tail to zero so
			// the score equals the P50-only score.
			name:       "P95 below P50 clamps tail to zero",
			stats:      ProviderStats{P50LatencyMs: 800, P95LatencyMs: 200, AvgCostUSD: 0.001},
			tailWeight: 1.0,
			wantScore:  legacy(ProviderStats{P50LatencyMs: 800, P95LatencyMs: 200, AvgCostUSD: 0.001}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerScore(tc.stats, tc.tailWeight)
			diff := got - tc.wantScore
			if diff < 0 {
				diff = -diff
			}
			if diff > 1e-12 {
				t.Errorf("providerScore(%+v, %v) = %v, want %v (diff %v)", tc.stats, tc.tailWeight, got, tc.wantScore, diff)
			}
		})
	}
}

func TestNewProviderSelector_TailWeightDefault(t *testing.T) {
	// NewProviderSelector must initialise TailWeight to
	// DefaultSelectorTailWeight so callers that do not set the
	// field directly continue to get the legacy P50-only ordering.
	sel := NewProviderSelector()
	if sel.TailWeight != DefaultSelectorTailWeight {
		t.Errorf("TailWeight = %v, want %v", sel.TailWeight, DefaultSelectorTailWeight)
	}
	if sel.TailWeight != 0 {
		t.Errorf("DefaultSelectorTailWeight = %v, want 0", sel.TailWeight)
	}
}

func TestProviderSelector_DeterministicTiebreak(t *testing.T) {
	// Same input twice must produce the same winner. This guards
	// against an accidental sort instability sneaking in via
	// SliceStable semantics changes.
	stats := []ProviderStats{
		{Name: "zeta", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
		{Name: "alpha", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}
	sel := NewProviderSelector()
	first, _ := sel.SelectFrontier(stats)
	for i := 0; i < 10; i++ {
		got, _ := sel.SelectFrontier(stats)
		if got != first {
			t.Fatalf("iteration %d: got %q, want %q", i, got, first)
		}
	}
}

func TestProviderSelector_DoesNotMutateInput(t *testing.T) {
	stats := []ProviderStats{
		{Name: "zeta", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
		{Name: "alpha", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}
	before := make([]ProviderStats, len(stats))
	copy(before, stats)
	_, _ = NewProviderSelector().SelectFrontier(stats)
	for i := range stats {
		if stats[i] != before[i] {
			t.Errorf("input mutated at %d: %+v vs %+v", i, stats[i], before[i])
		}
	}
}

// stubProviderSource is a ProviderStatsSource for tests. It always
// returns the same stats from a fixed timestamp; the Refresh tests use
// it to assert error propagation and snapshot stability.
type stubProviderSource struct {
	stats []ProviderStats
	err   error
	calls int
}

func (s *stubProviderSource) ProviderStats(_ context.Context, _ time.Time) ([]ProviderStats, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.stats, nil
}

func TestProviderStatsCache_RefreshAndSnapshot(t *testing.T) {
	src := &stubProviderSource{stats: []ProviderStats{
		{Name: "frontier", SampleCount: 10, P50LatencyMs: 800, AvgCostUSD: 0.005},
		{Name: "zai", SampleCount: 20, P50LatencyMs: 400, AvgCostUSD: 0.002},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := cache.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(got))
	}
	if src.calls != 1 {
		t.Errorf("source calls = %d, want 1", src.calls)
	}
	if cache.LastUpdated().IsZero() {
		t.Error("LastUpdated not set after successful Refresh")
	}
}

func TestProviderStatsCache_SelectFrontier(t *testing.T) {
	src := &stubProviderSource{stats: []ProviderStats{
		{Name: "frontier", SampleCount: 10, P50LatencyMs: 800, AvgCostUSD: 0.005},
		{Name: "zai", SampleCount: 20, P50LatencyMs: 400, AvgCostUSD: 0.002},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	gotName, gotStats := cache.SelectFrontier()
	if gotName != "zai" {
		t.Errorf("SelectFrontier name = %q, want \"zai\"", gotName)
	}
	if gotStats.P50LatencyMs != 400 {
		t.Errorf("SelectFrontier stats P50 = %d, want 400", gotStats.P50LatencyMs)
	}
}

func TestProviderStatsCache_RefreshErrorDoesNotClearSnapshot(t *testing.T) {
	first := &stubProviderSource{stats: []ProviderStats{
		{Name: "x", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), first, time.Hour, time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	// Swap the source to one that always errors. Refresh must
	// surface the error but keep the prior snapshot intact.
	cache.mu.Lock()
	cache.source = &stubProviderSource{err: errors.New("boom")}
	cache.mu.Unlock()

	if err := cache.Refresh(context.Background()); err == nil {
		t.Error("expected Refresh to return error")
	}
	got := cache.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot len after error = %d, want 1 (previous snapshot preserved)", len(got))
	}
	if got[0].Name != "x" {
		t.Errorf("Snapshot[0].Name = %q, want \"x\"", got[0].Name)
	}
	if cache.LastError() == nil {
		t.Error("LastError nil after erroring Refresh")
	}
}

func TestProviderStatsCache_NilSourceDisabled(t *testing.T) {
	cache := NewProviderStatsCache(NewProviderSelector(), nil, time.Hour, time.Minute)
	if err := cache.Refresh(context.Background()); !errors.Is(err, ErrSelectorDisabled) {
		t.Errorf("Refresh err = %v, want ErrSelectorDisabled", err)
	}
	if got := cache.Snapshot(); got != nil {
		t.Errorf("Snapshot = %+v, want nil", got)
	}
	if _, stats := cache.SelectFrontier(); stats != (ProviderStats{}) {
		t.Errorf("SelectFrontier stats = %+v, want zero", stats)
	}
}

func TestProviderStatsCache_SnapshotIsCopy(t *testing.T) {
	src := &stubProviderSource{stats: []ProviderStats{
		{Name: "x", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	first := cache.Snapshot()
	first[0].P50LatencyMs = 99999 // mutate the copy
	second := cache.Snapshot()
	if second[0].P50LatencyMs != 500 {
		t.Errorf("Snapshot returned shared slice: latency changed to %d", second[0].P50LatencyMs)
	}
}

func TestProviderStatsCache_Defaults(t *testing.T) {
	cache := NewProviderStatsCache(nil, nil, 0, 0)
	if cache.selector.MinSamples != DefaultSelectorMinSamples {
		t.Errorf("MinSamples = %d, want %d", cache.selector.MinSamples, DefaultSelectorMinSamples)
	}
	if cache.selector.MaxErrorRate != DefaultSelectorMaxErrorRate {
		t.Errorf("MaxErrorRate = %v, want %v", cache.selector.MaxErrorRate, DefaultSelectorMaxErrorRate)
	}
	if cache.window != DefaultSelectorWindow {
		t.Errorf("window = %v, want %v", cache.window, DefaultSelectorWindow)
	}
	if cache.refresh != DefaultSelectorRefreshInterval {
		t.Errorf("refresh = %v, want %v", cache.refresh, DefaultSelectorRefreshInterval)
	}
}

// ---------------------------------------------------------------------------
// ProviderStatsCache.Run lifecycle coverage (issue #590)
//
// Run drives Refresh on a fixed cadence from a background goroutine. The
// tests below exercise every branch: the nil-source early return, the
// synchronous prime before the ticker loop, periodic ticker refresh, and
// clean termination on context cancellation. They also guard against
// goroutine leaks using runtime.NumGoroutine.
// ---------------------------------------------------------------------------

// countingSource is a thread-safe ProviderStatsSource for Run lifecycle
// tests. Run calls ProviderStats from a background goroutine while the
// test goroutine reads the call counter, so all access must be
// synchronized (the existing stubProviderSource is not safe for this).
type countingSource struct {
	mu    sync.Mutex
	calls int
	stats []ProviderStats
	err   error
	delay time.Duration // optional: blocks each call for this duration
}

func (s *countingSource) ProviderStats(_ context.Context, _ time.Time) ([]ProviderStats, error) {
	s.mu.Lock()
	s.calls++
	stats := s.stats
	err := s.err
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return stats, nil
}

func (s *countingSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// runWaitFor polls cond until it returns true or d elapses, failing the
// test on timeout so deadlocks surface immediately.
func runWaitFor(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// TestProviderStatsCache_Run_PrimesSynchronouslyBeforeTicker verifies
// that the immediate synchronous Refresh fires before Run enters the
// ticker loop. With a 1h refresh interval no ticker event can fire
// during the test, so any call observed is the synchronous prime.
func TestProviderStatsCache_Run_PrimesSynchronouslyBeforeTicker(t *testing.T) {
	src := &countingSource{stats: []ProviderStats{
		{Name: "frontier", SampleCount: 10, P50LatencyMs: 800, AvgCostUSD: 0.005},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()

	runWaitFor(t, func() bool { return src.Calls() >= 1 }, 2*time.Second)

	if got := src.Calls(); got != 1 {
		t.Fatalf("source calls = %d, want exactly 1 (synchronous prime; ticker interval is 1h)", got)
	}
	// The snapshot must already reflect the primed data before the
	// first ticker event could ever fire.
	if snap := cache.Snapshot(); len(snap) != 1 || snap[0].Name != "frontier" {
		t.Errorf("Snapshot after prime = %+v, want single 'frontier' entry", snap)
	}

	cancel()
	<-done
}

// TestProviderStatsCache_Run_TerminatesWithinTwoSecondsOfCancel
// verifies that Run returns promptly after context cancellation.
func TestProviderStatsCache_Run_TerminatesWithinTwoSecondsOfCancel(t *testing.T) {
	src := &countingSource{stats: []ProviderStats{
		{Name: "x", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()

	// Wait for the goroutine to start and reach the select loop.
	runWaitFor(t, func() bool { return src.Calls() >= 1 }, 2*time.Second)

	cancel()

	select {
	case <-done:
		// goroutine terminated promptly after cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation")
	}
}

// TestProviderStatsCache_Run_NoGoroutineLeak verifies that Run exits
// cleanly on cancellation using a runtime.NumGoroutine delta. Each
// iteration starts Run, lets it prime and tick, cancels, and waits for
// the goroutine to return. A leak (e.g. a ticker or blocking Refresh
// that never observes cancellation) accumulates across iterations.
func TestProviderStatsCache_Run_NoGoroutineLeak(t *testing.T) {
	const iterations = 5

	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < iterations; i++ {
		src := &countingSource{stats: []ProviderStats{
			{Name: "x", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
		}}
		cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, 20*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			cache.Run(ctx)
			close(done)
		}()

		// Wait for the prime plus at least one ticker so the
		// goroutine is well inside the select loop before cancel.
		runWaitFor(t, func() bool { return src.Calls() >= 2 }, 2*time.Second)

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Run did not return within 2s of cancel", i)
		}
	}

	// Allow the runtime to reap exited goroutines.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()

	after := runtime.NumGoroutine()
	// A leak of `iterations` goroutines would push `after` well
	// above before+1. The +1 margin absorbs runtime background
	// variance without masking a real leak.
	if delta := after - before; delta > 1 {
		t.Errorf("goroutine leak: before=%d after=%d (delta=%d, iterations=%d)", before, after, delta, iterations)
	}
}

// TestProviderStatsCache_Run_PeriodicRefreshViaTicker verifies that the
// background ticker drives Refresh after the synchronous prime. With a
// short interval the call count climbs past the initial prime.
func TestProviderStatsCache_Run_PeriodicRefreshViaTicker(t *testing.T) {
	src := &countingSource{stats: []ProviderStats{
		{Name: "x", SampleCount: 10, P50LatencyMs: 500, AvgCostUSD: 0.001},
	}}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()

	// Wait for the synchronous prime plus at least two ticker
	// refreshes. With a 20ms interval this completes well under 1s.
	runWaitFor(t, func() bool { return src.Calls() >= 3 }, 2*time.Second)

	if got := src.Calls(); got < 3 {
		t.Errorf("source calls = %d, want >= 3 (prime + at least 2 ticker refreshes)", got)
	}

	cancel()
	<-done
}

// TestProviderStatsCache_Run_NilSourceReturnsImmediately verifies that
// Run returns without blocking when no source is wired (the cache is
// disabled).
func TestProviderStatsCache_Run_NilSourceReturnsImmediately(t *testing.T) {
	cache := NewProviderStatsCache(NewProviderSelector(), nil, time.Hour, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Run returned immediately without needing cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("Run with nil source did not return within 2s (should return immediately)")
	}
}

// TestProviderStatsCache_Run_RefreshErrorKeepsLoopAlive verifies that a
// source that returns errors does not stall or crash the refresh loop;
// subsequent ticker events continue to call Refresh.
func TestProviderStatsCache_Run_RefreshErrorKeepsLoopAlive(t *testing.T) {
	src := &countingSource{err: errors.New("db down")}
	cache := NewProviderStatsCache(NewProviderSelector(), src, time.Hour, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()

	// The loop must keep ticking despite every Refresh erroring.
	runWaitFor(t, func() bool { return src.Calls() >= 3 }, 2*time.Second)

	if got := src.Calls(); got < 3 {
		t.Errorf("source calls = %d, want >= 3 (loop must continue past Refresh errors)", got)
	}
	if cache.LastError() == nil {
		t.Error("LastError nil after erroring Refresh inside Run")
	}

	cancel()
	<-done
}

// stubHealthChecker is a test double for providers.HealthChecker.
type stubHealthChecker struct {
	unhealthy map[string]bool
}

func (s stubHealthChecker) IsHealthy(name string) bool {
	return !s.unhealthy[name]
}

func (s stubHealthChecker) UnhealthyProviders() []string {
	var out []string
	for name := range s.unhealthy {
		out = append(out, name)
	}
	return out
}

// TestSelectFrontier_SkipsUnhealthy (issue #1158) verifies that the
// selector excludes providers whose health circuit is open.
func TestSelectFrontier_SkipsUnhealthy(t *testing.T) {
	t.Parallel()
	sel := &ProviderSelector{
		MinSamples:   1,
		MaxErrorRate: 1.0,
		Health: stubHealthChecker{
			unhealthy: map[string]bool{"cheap": true},
		},
	}
	stats := []ProviderStats{
		{Name: "cheap", P50LatencyMs: 100, AvgCostUSD: 0.0001, SampleCount: 10},
		{Name: "expensive", P50LatencyMs: 500, AvgCostUSD: 0.01, SampleCount: 10},
	}
	name, _ := sel.SelectFrontier(stats)
	if name != "expensive" {
		t.Fatalf("expected 'expensive' (cheap is unhealthy), got %q", name)
	}
}

// TestSelectFrontier_AllUnhealthy (issue #1158) verifies that when all
// providers are unhealthy the selector returns "" so the caller falls
// back to the first configured provider.
func TestSelectFrontier_AllUnhealthy(t *testing.T) {
	t.Parallel()
	sel := &ProviderSelector{
		MinSamples:   1,
		MaxErrorRate: 1.0,
		Health: stubHealthChecker{
			unhealthy: map[string]bool{"a": true, "b": true},
		},
	}
	stats := []ProviderStats{
		{Name: "a", P50LatencyMs: 100, AvgCostUSD: 0.001, SampleCount: 10},
		{Name: "b", P50LatencyMs: 200, AvgCostUSD: 0.002, SampleCount: 10},
	}
	name, _ := sel.SelectFrontier(stats)
	if name != "" {
		t.Fatalf("expected empty when all providers are unhealthy, got %q", name)
	}
}

// TestSelectFrontier_NilHealthChecker (issue #1158) verifies that a nil
// HealthChecker preserves the legacy error-rate-only exclusion.
func TestSelectFrontier_NilHealthChecker(t *testing.T) {
	t.Parallel()
	sel := &ProviderSelector{
		MinSamples:   1,
		MaxErrorRate: 1.0,
	}
	stats := []ProviderStats{
		{Name: "a", P50LatencyMs: 100, AvgCostUSD: 0.001, SampleCount: 10},
	}
	name, _ := sel.SelectFrontier(stats)
	if name != "a" {
		t.Fatalf("expected 'a', got %q", name)
	}
}
