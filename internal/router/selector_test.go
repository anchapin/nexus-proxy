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
