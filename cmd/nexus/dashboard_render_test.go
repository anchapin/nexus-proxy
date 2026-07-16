package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// update, when set via -update-golden, regenerates every testdata/
// golden file from the current renderer output. Run:
//
//	go test ./cmd/nexus -run TestGolden -update-golden
//
// then review the diff before committing.
var update = flag.Bool("update-golden", false, "regenerate testdata/ golden files")

// silentLogger is the no-op metrics logger (mirrors the metrics test
// suite) so seeding a store does not spam test output.
func silentLogger(string, ...any) {}

// goldenDay is the fixed UTC date every seed uses. Pinning the date
// keeps golden output deterministic regardless of when the test runs.
var goldenDay = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// seedStore returns a freshly opened, populated store whose only data
// lands on goldenDay. The store is fully flushed and closed before
// returning so DailySummary reads see every row; the caller reopens for
// reads. This mirrors the close-then-reopen pattern in the metrics
// package's own tests.
func seedStore(t *testing.T) (path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "metrics.db")
	w, err := metrics.OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	rows := []metrics.Request{
		{Timestamp: goldenDay, RequestID: "l1", Route: "local", Model: "qwen3-coder:8b", InputTokens: 200, TOONSavingsTokens: 50},
		{Timestamp: goldenDay, RequestID: "l2", Route: "local", Model: "qwen3-coder:8b", InputTokens: 300, TOONSavingsTokens: 60},
		{Timestamp: goldenDay, RequestID: "l3", Route: "local", Model: "qwen3-coder:8b", InputTokens: 150, TOONSavingsTokens: 40},
		{Timestamp: goldenDay, RequestID: "f1", Route: "frontier", Model: "gpt-4o", InputTokens: 1200, TOONSavingsTokens: 100, EstimatedCostUSD: 0.0024},
		{Timestamp: goldenDay, RequestID: "f2", Route: "frontier", Model: "gpt-4o", InputTokens: 800, TOONSavingsTokens: 80, EstimatedCostUSD: 0.0016},
		{Timestamp: goldenDay, RequestID: "u1", Route: "fusion", Model: "glm-4.6", InputTokens: 500, TOONSavingsTokens: 70},
	}
	for _, r := range rows {
		if err := w.RecordRequest(r); err != nil {
			t.Fatalf("RecordRequest %s: %v", r.RequestID, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flush seed store: %v", err)
	}
	return path
}

// readDay reopens the store read-only and returns the DailySummary for
// day. A separate handle guarantees the drained writes are visible.
func readDay(t *testing.T, path string, day time.Time) metrics.Summary {
	t.Helper()
	s, err := metrics.OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	sum, err := s.DailySummary(day)
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	return sum
}

// checkGolden compares got against testdata/name, regenerating the file
// when -update-golden is set. The comparison is exact so a single
// stray space is caught.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v\n(re-run with -update-golden to create it)", name, err)
	}
	if string(want) != got {
		t.Errorf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// TestGoldenSeededTable runs the full read path (seed → DailySummary →
// renderDashboardTable) and golden-files the rendered table. A regression in
// the schema scan order, the renderer, or the cost formula all surface
// here as a diff.
func TestGoldenSeededTable(t *testing.T) {
	path := seedStore(t)
	sum := readDay(t, path, goldenDay)
	var b strings.Builder
	if err := renderDashboardTable([]metrics.Summary{sum}, 0.002, &b); err != nil {
		t.Fatalf("renderDashboardTable: %v", err)
	}
	checkGolden(t, "seeded.golden", b.String())
}

// TestGoldenEmptyTable asserts the no-data case: header + a zero row
// for the queried day + the human-readable "no requests recorded"
// hint. This is the acceptance criterion "handles the no-data-yet case
// gracefully".
func TestGoldenEmptyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	// Open then immediately close on an empty DB so the table exists
	// but holds zero rows — exactly what a fresh proxy install looks
	// like on first run.
	w, err := metrics.OpenWithLogger(path, silentLogger)
	if err != nil {
		t.Fatalf("open empty store: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close empty store: %v", err)
	}
	sum := readDay(t, path, goldenDay)
	var b strings.Builder
	if err := renderDashboardTable([]metrics.Summary{sum}, 0.002, &b); err != nil {
		t.Fatalf("renderDashboardTable: %v", err)
	}
	checkGolden(t, "empty.golden", b.String())
}

// TestGoldenSeededJSON locks the --json output shape (field names,
// nesting, precision) so external consumers piping through jq don't
// break silently on a renderer change.
func TestGoldenSeededJSON(t *testing.T) {
	path := seedStore(t)
	sum := readDay(t, path, goldenDay)
	var b strings.Builder
	if err := renderDashboardJSON([]metrics.Summary{sum}, 0.002, &b); err != nil {
		t.Fatalf("renderDashboardJSON: %v", err)
	}
	checkGolden(t, "seeded.json", b.String())
}

// TestSavingsUSDFormula pins the dollar conversion so a future change
// to costDivisor or the multiply order is caught by a unit test, not
// only by a golden diff.
func TestSavingsUSDFormula(t *testing.T) {
	s := metrics.Summary{TOONSavingsTokens: 400}
	if got := savingsUSD(s, 0.002); got != 0.0008 {
		t.Errorf("savingsUSD(400, 0.002) = %v, want 0.0008", got)
	}
	// Zero tokens → zero dollars.
	if got := savingsUSD(metrics.Summary{}, 0.002); got != 0 {
		t.Errorf("savingsUSD({}, 0.002) = %v, want 0", got)
	}
}

// TestComma covers the three boundary cases the helper was written
// for: sub-1000 (no separator), exact multiples, and negatives.
func TestComma(t *testing.T) {
	for in, want := range map[int]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		1234:    "1,234",
		1234567: "1,234,567",
		-1234:   "-1,234",
	} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDaysRange covers the inclusive-ascending contract and the
// start-after-end clamp.
func TestDaysRange(t *testing.T) {
	jul9 := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	jul11 := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	days := daysRange(jul9, jul11)
	if len(days) != 3 {
		t.Fatalf("len(days) = %d, want 3", len(days))
	}
	if !days[0].Equal(jul9) || !days[2].Equal(jul11) {
		t.Errorf("range edges = %v..%v, want %v..%v", days[0], days[2], jul9, jul11)
	}
	// start after end collapses to a single day (the start).
	rev := daysRange(jul11, jul9)
	if len(rev) != 1 || !rev[0].Equal(jul11) {
		t.Errorf("reversed range = %v, want single %v", rev, jul11)
	}
}
