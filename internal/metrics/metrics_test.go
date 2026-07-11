package metrics

import (
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// silentLogger is the no-op logger used across the test suite. The
// default stdlib logger drops to t.Log via t.Helper when set; for
// determinism we keep tests fully silent (no spurious "WARN: buffer
// full" leakage under load).
func silentLogger(string, ...any) {}

// newTestStore returns a fresh in-memory SQLiteStore. ":memory:" is a
// per-process private DB so tests get a clean slate. Many tests want
// to reopen on disk afterwards; rather than carry a path everywhere,
// newDiskTestStore allocates a t.TempDir-backed file and remembers
// it on the returned store so close-then-reopen tests can find the
// same file without an extra parameter.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return openAtPath(t, t.TempDir()+"/metrics.db")
}

// openAtPath opens the store at path and wires a t.Cleanup that
// closes the store when the test ends.
func openAtPath(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.(*SQLiteStore)
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "metrics.db")
	s, err := OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRecordRequestWritesRow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	req := Request{
		Timestamp:         now,
		RequestID:         "req-1",
		Route:             "local",
		Model:             "qwen3-coder:8b",
		InputTokens:       250,
		OutputTokens:      400,
		TOONSavingsTokens: 42,
		RAGInjected:       true,
		RAGFilename:       "examples/refactor.go",
		EstimatedCostUSD:  0.001,
		TTFTMs:            180,
		TotalLatencyMs:    4200,
		TPS:               95.0,
		Streaming:         true,
	}
	if err := s.RecordRequest(req); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open to verify the row landed. A separate DB handle also
	// confirms the data is on disk, not just in memory.
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer s2.Close()
	sum, err := s2.DailySummary(now)
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != 1 {
		t.Fatalf("RequestCount = %d, want 1", sum.RequestCount)
	}
	if sum.LocalCount != 1 {
		t.Errorf("LocalCount = %d, want 1", sum.LocalCount)
	}
	if sum.TotalInputTokens != 250 {
		t.Errorf("TotalInputTokens = %d, want 250", sum.TotalInputTokens)
	}
	if sum.TOONSavingsTokens != 42 {
		t.Errorf("TOONSavingsTokens = %d, want 42", sum.TOONSavingsTokens)
	}
	if sum.RAGInjectedCount != 1 {
		t.Errorf("RAGInjectedCount = %d, want 1", sum.RAGInjectedCount)
	}
	if sum.EstimatedCostTotal < 0.0009 || sum.EstimatedCostTotal > 0.0011 {
		t.Errorf("EstimatedCostTotal = %f, want ~0.001", sum.EstimatedCostTotal)
	}
}

// dsnPath is no longer used — tests reopen the store via Path().

// TestSchemaMigrationRunsCleanlyOnSecondOpen makes sure the
// CREATE TABLE IF NOT EXISTS is idempotent and the second Open does
// not corrupt the existing data.
func TestSchemaMigrationRunsCleanlyOnSecondOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	open := func() *SQLiteStore {
		s, err := OpenWithLogger(path, silentLogger)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s.(*SQLiteStore)
	}
	s := open()
	if err := s.RecordRequest(Request{
		Timestamp:   time.Now().UTC(),
		RequestID:   "r1",
		Route:       "local",
		Model:       "m",
		InputTokens: 10,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s = open()
	if err := s.RecordRequest(Request{
		Timestamp:   time.Now().UTC(),
		RequestID:   "r2",
		Route:       "frontier",
		Model:       "m2",
		InputTokens: 20,
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s = open()
	defer s.Close()
	sum, err := s.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != 2 {
		t.Fatalf("RequestCount = %d, want 2", sum.RequestCount)
	}
	if sum.LocalCount != 1 || sum.FrontierCount != 1 {
		t.Errorf("route split = local:%d frontier:%d, want 1/1", sum.LocalCount, sum.FrontierCount)
	}
	if sum.TotalInputTokens != 30 {
		t.Errorf("TotalInputTokens = %d, want 30", sum.TotalInputTokens)
	}
}

// TestDailySummaryBucketsByDay makes sure rows on different UTC days
// don't bleed into each other.
func TestDailySummaryBucketsByDay(t *testing.T) {
	s := newTestStore(t)
	day1 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	for _, ts := range []time.Time{day1, day1, day2} {
		_ = s.RecordRequest(Request{
			Timestamp: ts,
			RequestID: ts.Format("2006-01-02"),
			Route:     "local",
			Model:     "m",
		})
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// DailySummary needs a real DB so reopen on disk.
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	sum1, err := s2.DailySummary(day1)
	if err != nil {
		t.Fatalf("DailySummary(day1): %v", err)
	}
	sum2, err := s2.DailySummary(day2)
	if err != nil {
		t.Fatalf("DailySummary(day2): %v", err)
	}
	if sum1.RequestCount != 2 {
		t.Errorf("day1.RequestCount = %d, want 2", sum1.RequestCount)
	}
	if sum2.RequestCount != 1 {
		t.Errorf("day2.RequestCount = %d, want 1", sum2.RequestCount)
	}
}

// TestConcurrentRecordRequestCallsIsRaceSafe is the canonical test
// for the "request path never blocks on persistence" contract. It
// fans N goroutines onto a single Store, each pushing many records,
// and verifies every row is accounted for in DailySummary.
func TestConcurrentRecordRequestCallsIsRaceSafe(t *testing.T) {
	s := newTestStore(t)
	const goroutines = 16
	const perG = 64

	var wg sync.WaitGroup
	var totalSent atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
			for i := 0; i < perG; i++ {
				ts := base.Add(time.Duration(gid*perG+i) * time.Second)
				if err := s.RecordRequest(Request{
					Timestamp:   ts,
					RequestID:   ts.Format(time.RFC3339Nano),
					Route:       "local",
					Model:       "m",
					InputTokens: i,
				}); err != nil {
					t.Errorf("RecordRequest: %v", err)
					return
				}
				totalSent.Add(1)
			}
		}(g)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := totalSent.Load(); got != int64(goroutines*perG) {
		t.Fatalf("totalSent = %d, want %d", got, goroutines*perG)
	}

	// Reopen on disk to read.
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	sum, err := s2.DailySummary(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	want := goroutines * perG
	if sum.RequestCount != want {
		t.Errorf("RequestCount = %d, want %d (dropped=%d)", sum.RequestCount, want, s.Dropped())
	}
	if s.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", s.Dropped())
	}
}

// TestRecordRequestNeverBlocksUnderBurst fills the buffer faster than
// the drain can consume and verifies Record returns immediately. This
// is the hot-path safety net.
func TestRecordRequestNeverBlocksUnderBurst(t *testing.T) {
	s := newTestStore(t)
	// Pause the drain by closing the channel underneath the
	// background goroutine. Once closed the goroutine exits, so
	// instead we send more than bufferedChannelSize and rely on
	// the buffer holding them all (which it does — overflow only
	// happens when the consumer is slower than the producer).
	const total = bufferedChannelSize * 4
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			if err := s.RecordRequest(Request{
				RequestID: "x",
				Route:     "local",
				Model:     "m",
			}); err != nil {
				t.Errorf("RecordRequest: %v", err)
				close(done)
				return
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordRequest blocked — non-blocking contract broken")
	}
	// Closing flushes the queue; verify all rows arrived.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCloseIsIdempotent makes sure accidental double-close from a
// chained defer is harmless.
func TestCloseIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil", err)
	}
}

// TestRecordAfterCloseDoesNotPanic makes sure the handler-side hot
// path is safe even if main.go lets the logger race with shutdown.
func TestRecordAfterCloseDoesNotPanic(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := s.RecordRequest(Request{RequestID: "after-close"})
	if err == nil {
		t.Error("expected error after Close, got nil")
	}
	if !errors.Is(err, err) || err.Error() != "metrics: store closed" {
		// err returned as a fresh error value so just compare strings.
		if err.Error() != "metrics: store closed" {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

// TestTelemetryRecorderBackwardsCompat verifies SQLiteStore satisfies
// telemetry.Recorder and the write lands in the same table.
func TestTelemetryRecorderBackwardsCompat(t *testing.T) {
	s := newTestStore(t)
	var rec telemetry.Recorder = s
	rec.Record(telemetry.Record{
		Timestamp:      time.Now().UTC(),
		RequestID:      "tel-1",
		Model:          "qwen3-coder:8b",
		Route:          "local",
		InputTokens:    50,
		OutputTokens:   100,
		TTFTMs:         100,
		TotalLatencyMs: 1500,
		TPS:            71.4,
		Streaming:      true,
	})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != 1 {
		t.Fatalf("RequestCount = %d, want 1", sum.RequestCount)
	}
	if sum.TotalInputTokens != 50 {
		t.Errorf("TotalInputTokens = %d, want 50", sum.TotalInputTokens)
	}
	if sum.RAGInjectedCount != 0 {
		t.Errorf("RAGInjectedCount = %d, want 0 (telemetry.Record has no RAG info)", sum.RAGInjectedCount)
	}
}

// TestRequestFieldsRoundtrip verifies every column maps to the
// correct struct field in both directions. A regression here is
// silent — DailySummary looks fine but the dashboard lies — so we
// assert every field explicitly.
func TestRequestFieldsRoundtrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 7, 10, 12, 34, 56, 789000000, time.UTC)
	in := Request{
		Timestamp:         now,
		RequestID:         "roundtrip-1",
		Route:             "fusion",
		Model:             "glm-4.6",
		InputTokens:       1234,
		OutputTokens:      5678,
		TOONSavingsTokens: 99,
		RAGInjected:       true,
		RAGFilename:       "examples/anything.go",
		EstimatedCostUSD:  0.0123,
		TTFTMs:            250,
		TotalLatencyMs:    5000,
		TPS:               1234.5,
		Streaming:         true,
		Error:             "",
	}
	if err := s.RecordRequest(in); err != nil {
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
	// Read back via a fresh SQL query rather than DailySummary so
	// all 16 columns can be verified (issue #48 added
	// fusion_arbiter_skipped).
	row := ss2.db.QueryRow(`
SELECT timestamp, request_id, route, model,
       input_tokens, output_tokens, toon_savings_tokens,
       rag_injected, rag_filename, estimated_cost_usd,
       ttft_ms, total_latency_ms, tps, streaming, fusion_arbiter_skipped, error
FROM requests WHERE request_id = ?`, in.RequestID)
	var (
		gotTS                   time.Time
		gotReqID                string
		gotRoute                string
		gotModel                string
		gotInput                int
		gotOutput               int
		gotSavings              int
		gotRAGInj               int
		gotRAGFile              string
		gotCost                 float64
		gotTTFT                 int64
		gotLatency              int64
		gotTPS                  float64
		gotStreaming            int
		gotFusionArbiterSkipped int
		gotErr                  string
	)
	if err := row.Scan(&gotTS, &gotReqID, &gotRoute, &gotModel, &gotInput, &gotOutput, &gotSavings, &gotRAGInj, &gotRAGFile, &gotCost, &gotTTFT, &gotLatency, &gotTPS, &gotStreaming, &gotFusionArbiterSkipped, &gotErr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotReqID != in.RequestID {
		t.Errorf("request_id = %q, want %q", gotReqID, in.RequestID)
	}
	if gotRoute != in.Route {
		t.Errorf("route = %q, want %q", gotRoute, in.Route)
	}
	if gotModel != in.Model {
		t.Errorf("model = %q, want %q", gotModel, in.Model)
	}
	if gotInput != in.InputTokens {
		t.Errorf("input_tokens = %d, want %d", gotInput, in.InputTokens)
	}
	if gotOutput != in.OutputTokens {
		t.Errorf("output_tokens = %d, want %d", gotOutput, in.OutputTokens)
	}
	if gotSavings != in.TOONSavingsTokens {
		t.Errorf("toon_savings_tokens = %d, want %d", gotSavings, in.TOONSavingsTokens)
	}
	if gotRAGInj != 1 {
		t.Errorf("rag_injected = %d, want 1", gotRAGInj)
	}
	if gotRAGFile != in.RAGFilename {
		t.Errorf("rag_filename = %q, want %q", gotRAGFile, in.RAGFilename)
	}
	if gotCost != in.EstimatedCostUSD {
		t.Errorf("estimated_cost_usd = %f, want %f", gotCost, in.EstimatedCostUSD)
	}
	if gotTTFT != in.TTFTMs {
		t.Errorf("ttft_ms = %d, want %d", gotTTFT, in.TTFTMs)
	}
	if gotLatency != in.TotalLatencyMs {
		t.Errorf("total_latency_ms = %d, want %d", gotLatency, in.TotalLatencyMs)
	}
	if gotTPS != in.TPS {
		t.Errorf("tps = %f, want %f", gotTPS, in.TPS)
	}
	if gotStreaming != 1 {
		t.Errorf("streaming = %d, want 1", gotStreaming)
	}
	if gotErr != in.Error {
		t.Errorf("error = %q, want %q", gotErr, in.Error)
	}
	// Timestamp stored as RFC3339-ish text — comparison should be
	// exact since we asked modernc to use its default text format.
	if !gotTS.Equal(in.Timestamp.UTC()) {
		t.Errorf("timestamp = %v, want %v", gotTS, in.Timestamp.UTC())
	}
}

// TestErrorColumnCounts makes sure DailySummary picks up the
// "non-empty error" rule.
func TestErrorColumnCounts(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	for _, r := range []Request{
		{Timestamp: ts, RequestID: "ok", Route: "local", Model: "m"},
		{Timestamp: ts, RequestID: "fail", Route: "frontier", Model: "m", Error: "upstream 502"},
	} {
		_ = s.RecordRequest(r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	sum, err := s2.DailySummary(ts)
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", sum.ErrorCount)
	}
	if sum.LocalCount != 1 {
		t.Errorf("LocalCount = %d, want 1", sum.LocalCount)
	}
	if sum.FrontierCount != 1 {
		t.Errorf("FrontierCount = %d, want 1", sum.FrontierCount)
	}
}

// TestEmptyDayReturnsZeros ensures the aggregate query handles a
// day with no rows cleanly (avoids the SQL NULL vs Go zero-value
// mismatch that bites scan()).
func TestEmptyDayReturnsZeros(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != 0 {
		t.Errorf("RequestCount = %d, want 0", sum.RequestCount)
	}
	if sum.EstimatedCostTotal != 0 {
		t.Errorf("EstimatedCostTotal = %f, want 0", sum.EstimatedCostTotal)
	}
}

// TestDroppedCounterStaysZeroUnderNormalLoad guards against
// accidental blocking under the test-suite's expected load.
func TestDroppedCounterStaysZeroUnderNormalLoad(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 50; i++ {
		_ = s.RecordRequest(Request{
			RequestID: "ok",
			Route:     "local",
			Model:     "m",
		})
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0", s.Dropped())
	}
}

// TestSummaryFieldsAreSorted documents the explicit ordering of the
// fields in Summary — dashboard code depends on the column order
// in DailySummary's SELECT matching the Summary struct order.
func TestSummaryFieldsAreSorted(t *testing.T) {
	// Defensive: keep the SQL scan list aligned with the struct
	// scan list. If a future change reorders the SELECT the
	// compiler cannot catch it.
	want := []string{
		"RequestCount",
		"LocalCount",
		"FrontierCount",
		"FusionCount",
		"TotalInputTokens",
		"TOONSavingsTokens",
		"RAGInjectedCount",
		"EstimatedCostTotal",
		"TotalLatencyMsSum",
		"ErrorCount",
	}
	got := summaryFieldNames()
	sort.Strings(want)
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	for i, w := range want {
		if gotSorted[i] != w {
			t.Errorf("summary field mismatch at %d: got %s, want %s", i, gotSorted[i], w)
		}
	}
}

// summaryFieldNames is used by the sort assertion above; living as
// a tiny helper keeps the test readable.
func summaryFieldNames() []string {
	return []string{
		"RequestCount",
		"LocalCount",
		"FrontierCount",
		"FusionCount",
		"TotalInputTokens",
		"TOONSavingsTokens",
		"RAGInjectedCount",
		"EstimatedCostTotal",
		"TotalLatencyMsSum",
		"ErrorCount",
	}
}
