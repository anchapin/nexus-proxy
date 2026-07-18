package router

import (
	"context"
	"errors"
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
