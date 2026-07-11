package metrics

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/router"
)

// seedProviderStats inserts a fixed set of requests into the store so
// the ProviderStats aggregation can be asserted. rows is keyed by
// model name; each row's index is appended to the request id so
// duplicates are easy to spot in failure messages. Negative offsetSec
// values let tests seed rows that fall before `base`.
func seedProviderStats(t *testing.T, s *SQLiteStore, base time.Time, rows map[string][]seedRow) {
	t.Helper()
	for model, list := range rows {
		for i, r := range list {
			ts := base.Add(time.Duration(i)*time.Second + time.Duration(r.offsetSec)*time.Second)
			if err := s.RecordRequest(Request{
				Timestamp:        ts,
				RequestID:        model + "-row-" + intToStr(i),
				Route:            "frontier",
				Model:            model,
				TotalLatencyMs:   r.latency,
				EstimatedCostUSD: r.cost,
				Error:            r.err,
			}); err != nil {
				t.Fatalf("RecordRequest(%s/%d): %v", model, i, err)
			}
		}
	}
}

type seedRow struct {
	latency   float64
	cost      float64
	err       string
	offsetSec int // extra time offset (used for "old" rows in the window test)
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

func TestProviderStats_AggregatesPerModel(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// 5 frontier requests: latencies 100, 200, 300, 400, 500; cost 0.005 each; 1 error
	// 4 zai requests: latencies 150, 250, 350, 450; cost 0.002 each; 0 errors
	seedProviderStats(t, s, base, map[string][]seedRow{
		"gpt-4o": {
			{latency: 100, cost: 0.005},
			{latency: 200, cost: 0.005},
			{latency: 300, cost: 0.005},
			{latency: 400, cost: 0.005, err: "upstream 502"},
			{latency: 500, cost: 0.005},
		},
		"glm-4.6": {
			{latency: 150, cost: 0.002},
			{latency: 250, cost: 0.002},
			{latency: 350, cost: 0.002},
			{latency: 450, cost: 0.002},
		},
	})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)

	stats, err := ss2.ProviderStats(base.Add(-time.Second))
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("len(stats) = %d, want 2", len(stats))
	}

	byName := map[string]router.ProviderStats{}
	for _, st := range stats {
		byName[st.Name] = st
	}

	gpt, ok := byName["gpt-4o"]
	if !ok {
		t.Fatal("missing gpt-4o")
	}
	if gpt.SampleCount != 5 {
		t.Errorf("gpt-4o sample_count = %d, want 5", gpt.SampleCount)
	}
	if gpt.AvgCostUSD < 0.00499 || gpt.AvgCostUSD > 0.00501 {
		t.Errorf("gpt-4o avg_cost = %f, want ~0.005", gpt.AvgCostUSD)
	}
	if gpt.ErrorRate < 0.19 || gpt.ErrorRate > 0.21 {
		t.Errorf("gpt-4o error_rate = %f, want ~0.2", gpt.ErrorRate)
	}
	if gpt.P50LatencyMs != 300 {
		t.Errorf("gpt-4o p50 = %d, want 300 (median of 100,200,300,400,500)", gpt.P50LatencyMs)
	}
	// p95 of [100,200,300,400,500]: row at ceil(5*0.95) = 5th row = 500
	if gpt.P95LatencyMs != 500 {
		t.Errorf("gpt-4o p95 = %d, want 500", gpt.P95LatencyMs)
	}

	glm, ok := byName["glm-4.6"]
	if !ok {
		t.Fatal("missing glm-4.6")
	}
	if glm.SampleCount != 4 {
		t.Errorf("glm-4.6 sample_count = %d, want 4", glm.SampleCount)
	}
	if glm.ErrorRate != 0 {
		t.Errorf("glm-4.6 error_rate = %f, want 0", glm.ErrorRate)
	}
	// p50 of [150,250,350,450]: row at ceil(4*0.5) = 2nd row = 250
	if glm.P50LatencyMs != 250 {
		t.Errorf("glm-4.6 p50 = %d, want 250", glm.P50LatencyMs)
	}
	// p95 of [150,250,350,450]: ceil(4*0.95) = 4th row = 450
	if glm.P95LatencyMs != 450 {
		t.Errorf("glm-4.6 p95 = %d, want 450", glm.P95LatencyMs)
	}
}

func TestProviderStats_RespectsWindowCutoff(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// Two rows in the window (absolute timestamps base+0, base+60s),
	// two rows well outside (base-2h, base-3h). The seed helper
	// adds offsetSec to (base + i*second), so negative offsets
	// land at base - 7200s and base - 10800s.
	seedProviderStats(t, s, base, map[string][]seedRow{
		"gpt-4o": {
			{latency: 100, cost: 0.005},
			{latency: 200, cost: 0.005, offsetSec: 60},
			{latency: 9999, cost: 0.999, offsetSec: -7200},
			{latency: 9999, cost: 0.999, offsetSec: -10800},
		},
	})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)

	// Window: everything from base-1s onwards. The two old rows
	// at base-2h and base-3h are outside the window.
	since := base.Add(-time.Second)
	stats, err := ss2.ProviderStats(since)
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("len(stats) = %d, want 1 (old rows filtered)", len(stats))
	}
	gpt := stats[0]
	if gpt.Name != "gpt-4o" {
		t.Errorf("name = %q, want gpt-4o", gpt.Name)
	}
	if gpt.SampleCount != 2 {
		t.Errorf("sample_count = %d, want 2 (old rows excluded)", gpt.SampleCount)
	}
	if gpt.P50LatencyMs != 100 {
		t.Errorf("p50 = %d, want 100 (median of [100,200])", gpt.P50LatencyMs)
	}
}

func TestProviderStats_ExcludesUnknownModel(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// Insert one row with a known model and one row whose model
	// falls back to "unknown" (the RecordRequest sentinel).
	if err := s.RecordRequest(Request{
		Timestamp: base, RequestID: "ok", Route: "frontier",
		Model: "gpt-4o", TotalLatencyMs: 100,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.RecordRequest(Request{
		Timestamp: base, RequestID: "anon", Route: "frontier",
		// empty Model -> "unknown" sentinel in writeOne
		TotalLatencyMs: 50,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)

	stats, err := ss2.ProviderStats(base.Add(-time.Second))
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("len(stats) = %d, want 1 (unknown model excluded)", len(stats))
	}
	if stats[0].Name != "gpt-4o" {
		t.Errorf("name = %q, want gpt-4o", stats[0].Name)
	}
}

func TestProviderStats_EmptyReturnsNil(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)
	stats, err := ss2.ProviderStats(time.Now().UTC())
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("len(stats) = %d, want 0 (empty store)", len(stats))
	}
}

func TestProviderStats_AfterReopenUsesPersistedRows(t *testing.T) {
	// Writes are async, so we Close the writer, reopen, and verify
	// the aggregation sees the rows after restart.
	s := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := s.RecordRequest(Request{
			Timestamp:        base.Add(time.Duration(i) * time.Second),
			RequestID:        "persist-" + intToStr(i),
			Route:            "frontier",
			Model:            "gpt-4o",
			TotalLatencyMs:   float64(100 * (i + 1)),
			EstimatedCostUSD: 0.005,
		}); err != nil {
			t.Fatalf("RecordRequest: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopen on the same path.
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)
	stats, err := ss2.ProviderStats(base.Add(-time.Second))
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("len(stats) = %d, want 1", len(stats))
	}
	if stats[0].SampleCount != 5 {
		t.Errorf("sample_count = %d, want 5", stats[0].SampleCount)
	}
	if stats[0].P50LatencyMs != 300 {
		t.Errorf("p50 = %d, want 300 (median of [100,200,300,400,500])", stats[0].P50LatencyMs)
	}
}

// TestProviderStats_NilStoreDoesNotPanic makes sure callers that
// pass a nil store (typical of the legacy Recorder fallback path) do
// not crash.
func TestProviderStats_NilStoreDoesNotPanic(t *testing.T) {
	var s *SQLiteStore
	stats, err := s.ProviderStats(time.Now())
	if err != nil {
		t.Errorf("nil store err = %v, want nil", err)
	}
	if len(stats) != 0 {
		t.Errorf("nil store stats = %+v, want nil", stats)
	}
}

func TestProviderStats_DiskBacked(t *testing.T) {
	// Smoke test against a real on-disk file (rather than the
	// shared cache mode ":memory:") to make sure the WAL
	// configuration doesn't break the percentile query.
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := s.RecordRequest(Request{
		Timestamp: base, RequestID: "warm",
		Route: "frontier", Model: "gpt-4o",
		TotalLatencyMs: 100, EstimatedCostUSD: 0.005,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)
	stats, err := ss2.ProviderStats(base.Add(-time.Second))
	if err != nil {
		t.Fatalf("ProviderStats: %v", err)
	}
	if len(stats) != 1 || stats[0].Name != "gpt-4o" || stats[0].SampleCount != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}
