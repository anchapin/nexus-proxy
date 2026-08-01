package metrics

import (
	"testing"
	"time"
)

// TestWritable_LiveStoreReturnsTrue verifies that Writable reports
// true on a freshly opened store whose underlying connection is
// healthy. This is the happy path the /status handler exercises
// (cmd/nexus/main.go MetricsDBWritable) to advertise DB liveness.
func TestWritable_LiveStoreReturnsTrue(t *testing.T) {
	s := newTestStore(t)
	if !s.Writable() {
		t.Fatal("Writable() = false on live store, want true")
	}
}

// TestWritable_AfterCloseReturnsFalse verifies that Writable flips
// to false once the store has been closed: db.Ping() must surface
// the closed-handle error so /status stops advertising a dead
// connection. The store is opened directly (not via newTestStore)
// because this test owns the Close lifecycle itself.
func TestWritable_AfterCloseReturnsFalse(t *testing.T) {
	s, err := OpenWithLogger(t.TempDir()+"/metrics.db", silentLogger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*SQLiteStore)
	if !ss.Writable() {
		t.Fatal("Writable() = false before close, want true")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ss.Writable() {
		t.Fatal("Writable() = true after Close, want false")
	}
}

// TestWritable_NilReceiverReturnsFalse exercises the nil-safe guard
// at the top of Writable. It mirrors cmd/nexus/main.go, which holds
// the store behind the metrics.Store interface and type-asserts to
// *SQLiteStore before calling Writable(). When the store is a
// typed-nil pointer boxed in an interface the assertion succeeds,
// yielding a nil concrete receiver — Writable must return false and
// must not panic.
func TestWritable_NilReceiverReturnsFalse(t *testing.T) {
	var concrete *SQLiteStore
	// Box the typed nil in the Store interface exactly as main.go
	// does (metricsStore is declared as metrics.Store).
	var store Store = concrete
	recovered, ok := store.(*SQLiteStore)
	if !ok {
		t.Fatal("type assertion (*SQLiteStore) failed for boxed nil pointer")
	}
	if recovered.Writable() {
		t.Error("nil receiver Writable() = true, want false")
	}
}

// TestRangeSummaryEqualsSumOfDaily verifies the acceptance criterion
// "Store.RangeSummary(start, end) returns a single Summary whose totals
// equal the sum of DailySummary over the range" (issue #1170). It seeds
// rows across three consecutive UTC days, then compares the single
// RangeSummary against the manual sum of the three per-day summaries.
func TestRangeSummaryEqualsSumOfDaily(t *testing.T) {
	s := newTestStore(t)

	day1 := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	day3 := day2.AddDate(0, 0, 1)
	end := day3.AddDate(0, 0, 1) // exclusive upper bound

	rows := []Request{
		{Timestamp: day1, RequestID: "a1", Route: "local", Model: "m", InputTokens: 100, TOONSavingsTokens: 10},
		{Timestamp: day1, RequestID: "a2", Route: "frontier", Model: "m", InputTokens: 50, TOONSavingsTokens: 5, EstimatedCostUSD: 0.01, SavingsUSD: 0.02},
		{Timestamp: day2, RequestID: "b1", Route: "fusion", Model: "m", InputTokens: 200, TOONSavingsTokens: 20},
		{Timestamp: day3, RequestID: "c1", Route: "local", Model: "m", InputTokens: 300, TOONSavingsTokens: 30},
		{Timestamp: day3, RequestID: "c2", Route: "local", Model: "m", InputTokens: 150, TOONSavingsTokens: 15, BaselineCostUSD: 0.5, SavingsUSD: 0.1},
	}
	for _, r := range rows {
		if err := s.RecordRequest(r); err != nil {
			t.Fatalf("RecordRequest %s: %v", r.RequestID, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen on disk so every drained write is visible.
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// Single round-trip range summary.
	rangeSum, err := s2.RangeSummary(day1, end)
	if err != nil {
		t.Fatalf("RangeSummary: %v", err)
	}

	// Manual sum of per-day summaries.
	var manual Summary
	for _, d := range []time.Time{day1, day2, day3} {
		ds, err := s2.DailySummary(d)
		if err != nil {
			t.Fatalf("DailySummary %s: %v", d, err)
		}
		manual.RequestCount += ds.RequestCount
		manual.LocalCount += ds.LocalCount
		manual.FrontierCount += ds.FrontierCount
		manual.FusionCount += ds.FusionCount
		manual.TotalInputTokens += ds.TotalInputTokens
		manual.TOONSavingsTokens += ds.TOONSavingsTokens
		manual.EstimatedCostTotal += ds.EstimatedCostTotal
		manual.BaselineCostTotal += ds.BaselineCostTotal
		manual.SavingsTotal += ds.SavingsTotal
		manual.TotalLatencyMsSum += ds.TotalLatencyMsSum
		manual.ErrorCount += ds.ErrorCount
	}

	if rangeSum.RequestCount != manual.RequestCount ||
		rangeSum.LocalCount != manual.LocalCount ||
		rangeSum.FrontierCount != manual.FrontierCount ||
		rangeSum.FusionCount != manual.FusionCount ||
		rangeSum.TotalInputTokens != manual.TotalInputTokens ||
		rangeSum.TOONSavingsTokens != manual.TOONSavingsTokens ||
		rangeSum.EstimatedCostTotal != manual.EstimatedCostTotal ||
		rangeSum.BaselineCostTotal != manual.BaselineCostTotal ||
		rangeSum.SavingsTotal != manual.SavingsTotal ||
		rangeSum.TotalLatencyMsSum != manual.TotalLatencyMsSum ||
		rangeSum.ErrorCount != manual.ErrorCount {
		t.Errorf("RangeSummary does not equal sum of DailySummary\nrange:  %+v\nmanual: %+v", rangeSum, manual)
	}
}

// TestRangeSummaryEmptyRange verifies that an empty range (start not
// before end) returns an error rather than a misleading zero-row
// aggregate.
func TestRangeSummaryEmptyRange(t *testing.T) {
	s := newTestStore(t)
	day := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	_, err := s.RangeSummary(day, day)
	if err == nil {
		t.Fatal("RangeSummary with empty range returned nil error")
	}
}

// TestRangeSummaryNoRows verifies the zero-data path returns a valid
// (all-zero) Summary rather than an error.
func TestRangeSummaryNoRows(t *testing.T) {
	s := newTestStore(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	sum, err := s.RangeSummary(start, end)
	if err != nil {
		t.Fatalf("RangeSummary: %v", err)
	}
	if sum.RequestCount != 0 {
		t.Errorf("RequestCount = %d, want 0 for empty store", sum.RequestCount)
	}
}

// TestRangeSummaryBoundaryExclusive verifies the upper bound is
// exclusive: a row whose timestamp equals the end instant must NOT be
// counted.
func TestRangeSummaryBoundaryExclusive(t *testing.T) {
	s := newTestStore(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	midDay := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	// Row at exactly start — included.
	if err := s.RecordRequest(Request{Timestamp: start, RequestID: "in", Route: "local", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	// Row at exactly midDay (the exclusive end) — must be excluded.
	if err := s.RecordRequest(Request{Timestamp: midDay, RequestID: "out", Route: "frontier", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.RangeSummary(start, midDay)
	if err != nil {
		t.Fatalf("RangeSummary: %v", err)
	}
	if sum.RequestCount != 1 {
		t.Errorf("RequestCount = %d, want 1 (upper bound should be exclusive)", sum.RequestCount)
	}
	if sum.FrontierCount != 0 {
		t.Errorf("FrontierCount = %d, want 0 (boundary row excluded)", sum.FrontierCount)
	}
}
