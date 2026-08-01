package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// TestRunDashboardHelp verifies the -h flag prints usage and returns 0.
func TestRunDashboardHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDashboard([]string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runDashboard(-h) = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "nexus dashboard") {
		t.Errorf("stderr does not contain 'nexus dashboard' usage")
	}
}

// TestRunDashboardInvalidFlag verifies bad flags return 1.
func TestRunDashboardInvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDashboard([]string{"--not-a-flag"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runDashboard(--not-a-flag) = %d, want 1", code)
	}
}

// TestResolveCostPer1kFlag validates the flag parsing path.
func TestResolveCostPer1kFlag(t *testing.T) {
	v, err := resolveCostPer1k("0.001")
	if err != nil {
		t.Fatalf("resolveCostPer1k(0.001) err = %v", err)
	}
	if v != 0.001 {
		t.Errorf("resolveCostPer1k(0.001) = %v, want 0.001", v)
	}
}

// TestResolveCostPer1kInvalid validates invalid rate is rejected.
func TestResolveCostPer1kInvalid(t *testing.T) {
	_, err := resolveCostPer1k("not-a-number")
	if err == nil {
		t.Error("resolveCostPer1k(not-a-number) err = nil, want error")
	}
	_, err = resolveCostPer1k("-1")
	if err == nil {
		t.Error("resolveCostPer1k(-1) err = nil, want error")
	}
}

// TestResolveRange verifies date range parsing.
func TestResolveRange(t *testing.T) {
	start, end, err := resolveRange("2026-07-10", 3)
	if err != nil {
		t.Fatalf("resolveRange(2026-07-10, 3) err = %v", err)
	}
	if start.Day() != 10 || end.Day() != 12 {
		t.Errorf("range = %v..%v, want 10..12", start.Day(), end.Day())
	}
}

// TestResolveRangeBadDate verifies bad date format is rejected.
func TestResolveRangeBadDate(t *testing.T) {
	_, _, err := resolveRange("not-a-date", 0)
	if err == nil {
		t.Error("resolveRange(not-a-date, 0) err = nil, want error")
	}
}

// TestResolveDBPath verifies the three-tier DB path priority chain used by
// `nexus dashboard`: --db flag > NEXUS_METRICS_DB env > XDG default
// (config.DefaultMetricsDBPath). A reordered condition would silently make the
// dashboard read the wrong database and report stale or empty savings. Each
// subtest scopes its own NEXUS_METRICS_DB via t.Setenv, which is restored on
// exit, so the suite leaves the process env untouched.
func TestResolveDBPath(t *testing.T) {
	const (
		flagVal = "/explicit/flag/metrics.db"
		envVal  = "/from/env/metrics.db"
	)

	tests := []struct {
		name     string
		flagPath string
		envSet   bool
		envVal   string
		want     string
	}{
		{
			name:     "flag wins over env and default",
			flagPath: flagVal,
			envSet:   true,
			envVal:   envVal,
			want:     flagVal,
		},
		{
			name:     "flag wins when env unset",
			flagPath: flagVal,
			envSet:   false,
			want:     flagVal,
		},
		{
			name:     "env wins over default when flag empty",
			flagPath: "",
			envSet:   true,
			envVal:   envVal,
			want:     envVal,
		},
		{
			name:     "default when flag and env both empty",
			flagPath: "",
			envSet:   false,
			want:     config.DefaultMetricsDBPath(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv("NEXUS_METRICS_DB", tc.envVal)
			} else {
				t.Setenv("NEXUS_METRICS_DB", "")
			}
			got := resolveDBPath(tc.flagPath)
			if got != tc.want {
				t.Errorf("resolveDBPath(%q) = %q, want %q", tc.flagPath, got, tc.want)
			}
		})
	}
}

// TestRunDashboardEndToEnd drives the full runDashboard happy path
// (args → resolveDBPath → metrics.Open → DailySummary → render) against a
// seeded temp SQLite store produced by the shared seedStore helper. The store
// lands every row on goldenDay (2026-07-10), so the query pins --since to that
// day with a one-day window. Both the plain-text table and --json render modes
// are exercised; each must exit 0 and emit output referencing the seeded date.
// No real config dir or network bind is touched.
func TestRunDashboardEndToEnd(t *testing.T) {
	path := seedStore(t)
	// seedStore writes exclusively on goldenDay; restrict the window to that
	// single day so output is deterministic regardless of "today".
	baseArgs := []string{"--db", path, "--since", "2026-07-10", "--days", "1"}

	t.Run("table", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := append([]string{}, baseArgs...)
		code := runDashboard(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runDashboard(table) code = %d, stderr = %q", code, stderr.String())
		}
		out := stdout.String()
		if out == "" {
			t.Fatal("runDashboard(table) produced empty stdout")
		}
		if !strings.Contains(out, "DATE") {
			t.Errorf("table output missing header row\n%s", out)
		}
		if !strings.Contains(out, "2026-07-10") {
			t.Errorf("table output missing seeded date\n%s", out)
		}
		if strings.Contains(out, "no requests recorded") {
			t.Errorf("table output reports no data for a seeded store\n%s", out)
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--json"}, baseArgs...)
		code := runDashboard(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runDashboard(json) code = %d, stderr = %q", code, stderr.String())
		}
		out := stdout.String()
		if out == "" {
			t.Fatal("runDashboard(json) produced empty stdout")
		}
		if !strings.Contains(out, `"date": "2026-07-10"`) {
			t.Errorf("json output missing seeded date\n%s", out)
		}
		if !strings.Contains(out, "total_requests") {
			t.Errorf("json output missing total_requests field\n%s", out)
		}
	})
}

// TestRunDashboardOpenError confirms runDashboard returns exit code 1 and
// writes a diagnostic to stderr when the resolved DB path cannot be opened.
// metrics.Open auto-creates the parent directory, so to force an open failure
// we make the parent "directory" be an existing regular file (MkdirAll then
// fails). This covers the store-open error branch and gives runDashboard
// coverage a comfortable margin above the 70% acceptance floor.
func TestRunDashboardOpenError(t *testing.T) {
	// blocker is a regular file; treating it as a directory fails MkdirAll.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"--db", filepath.Join(blocker, "metrics.db")}
	code := runDashboard(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runDashboard(open error) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "nexus dashboard") {
		t.Errorf("stderr missing diagnostic prefix\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout on open error, got %q", stdout.String())
	}
}

// --- Range mode tests (issue #1170) ---------------------------------------

// TestRunDashboardRangeDaysConflict verifies the acceptance criterion:
// `nexus dashboard --range monthly --days 7` exits non-zero with a
// clear error message.
func TestRunDashboardRangeDaysConflict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := seedStore(t)
	args := []string{"--db", path, "--range", "monthly", "--days", "7"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runDashboard(--range --days) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr missing mutual-exclusivity message\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout on flag conflict, got %q", stdout.String())
	}
}

// TestRunDashboardCompareWithoutRange verifies --compare is rejected
// when --range is not set.
func TestRunDashboardCompareWithoutRange(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := seedStore(t)
	args := []string{"--db", path, "--compare"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runDashboard(--compare without --range) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "require --range") {
		t.Errorf("stderr missing hint\n%s", stderr.String())
	}
}

// TestRunDashboardRangeCustom drives the full range-mode happy path
// using --range custom with explicit --from / --to anchored on
// goldenDay so the test is deterministic regardless of "today". Both
// table and JSON modes are exercised.
func TestRunDashboardRangeCustom(t *testing.T) {
	path := seedStore(t)
	baseArgs := []string{"--db", path, "--range", "custom", "--from", "2026-07-10", "--to", "2026-07-10"}

	t.Run("table", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := append([]string{}, baseArgs...)
		code := runDashboard(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runDashboard(range table) code = %d, stderr = %q", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "PERIOD") {
			t.Errorf("table output missing header row\n%s", out)
		}
		if !strings.Contains(out, "2026-07-10") {
			t.Errorf("table output missing period label\n%s", out)
		}
		// Seeded store has 6 requests on goldenDay.
		if !strings.Contains(out, "6") {
			t.Errorf("table output missing request count\n%s", out)
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--json"}, baseArgs...)
		code := runDashboard(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runDashboard(range json) code = %d, stderr = %q", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, `"period": "2026-07-10"`) {
			t.Errorf("json output missing period\n%s", out)
		}
		if !strings.Contains(out, `"total_requests": 6`) {
			t.Errorf("json output missing total_requests\n%s", out)
		}
		// Without --compare, the "previous" field should be absent.
		if strings.Contains(out, `"previous"`) {
			t.Errorf("json output should not contain previous without --compare\n%s", out)
		}
	})
}

// TestRunDashboardRangeCompare verifies --compare mode emits the
// previous period and delta. Uses custom range so the test is
// deterministic; the seed has data only on goldenDay (the current
// period), so the previous period is all-zero.
func TestRunDashboardRangeCompare(t *testing.T) {
	path := seedStore(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--db", path, "--range", "custom", "--from", "2026-07-10", "--to", "2026-07-10", "--compare"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDashboard(range compare) code = %d, stderr = %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "previous") {
		t.Errorf("table output missing 'previous' row\n%s", out)
	}
	if !strings.Contains(out, "Δ") {
		t.Errorf("table output missing delta row\n%s", out)
	}
}

// TestRunDashboardRangeCompareJSON verifies the JSON output shape for
// --compare includes the "previous" field.
func TestRunDashboardRangeCompareJSON(t *testing.T) {
	path := seedStore(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--db", path, "--json", "--range", "custom", "--from", "2026-07-10", "--to", "2026-07-10", "--compare"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDashboard(range compare json) code = %d, stderr = %q", code, stderr.String())
	}
	var arr []rangeSummaryJSON
	if err := json.Unmarshal(stdout.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, stdout.String())
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
	if arr[0].Previous == nil {
		t.Error("expected non-nil previous with --compare")
	}
	if arr[0].Previous.TotalRequests != 0 {
		t.Errorf("previous total_requests = %d, want 0 (no data in previous period)", arr[0].Previous.TotalRequests)
	}
}

// TestRunDashboardRangeCustomMissingFromTo verifies --range custom
// without both --from and --to exits with an error.
func TestRunDashboardRangeCustomMissingFromTo(t *testing.T) {
	path := seedStore(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--db", path, "--range", "custom"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runDashboard(--range custom without from/to) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--from") {
		t.Errorf("stderr missing hint about --from\n%s", stderr.String())
	}
}

// TestRunDashboardRangeBadName verifies an invalid --range value is
// rejected.
func TestRunDashboardRangeBadName(t *testing.T) {
	path := seedStore(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--db", path, "--range", "yearly"}
	code := runDashboard(args, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runDashboard(--range yearly) code = %d, want 1", code)
	}
}

// TestResolveRangeWindow exercises each range preset to verify the
// start/end/prev bounds and label format are computed correctly.
func TestResolveRangeWindow(t *testing.T) {
	// All presets anchor on "today", so compute expected values from
	// the same time.Now the resolver uses.
	now := time.Now().UTC().Truncate(24 * time.Hour)

	t.Run("weekly", func(t *testing.T) {
		win, err := resolveRangeWindow("weekly", "", "")
		if err != nil {
			t.Fatal(err)
		}
		wantStart := now.AddDate(0, 0, -6)
		if !win.start.Equal(wantStart) {
			t.Errorf("start = %v, want %v", win.start, wantStart)
		}
		if !win.end.Equal(now) {
			t.Errorf("end = %v, want %v", win.end, now)
		}
		if win.label != wantStart.Format("2006-01-02") {
			t.Errorf("label = %q", win.label)
		}
	})

	t.Run("monthly", func(t *testing.T) {
		win, err := resolveRangeWindow("monthly", "", "")
		if err != nil {
			t.Fatal(err)
		}
		wantStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if !win.start.Equal(wantStart) {
			t.Errorf("start = %v, want %v", win.start, wantStart)
		}
		if win.label != now.Format("2006-01") {
			t.Errorf("label = %q", win.label)
		}
	})

	t.Run("quarterly", func(t *testing.T) {
		win, err := resolveRangeWindow("quarterly", "", "")
		if err != nil {
			t.Fatal(err)
		}
		qStartMonth := ((int(now.Month())-1)/3)*3 + 1
		wantStart := time.Date(now.Year(), time.Month(qStartMonth), 1, 0, 0, 0, 0, time.UTC)
		if !win.start.Equal(wantStart) {
			t.Errorf("start = %v, want %v", win.start, wantStart)
		}
		wantQ := (int(now.Month())-1)/3 + 1
		wantLabel := fmt.Sprintf("%d-Q%d", now.Year(), wantQ)
		if win.label != wantLabel {
			t.Errorf("label = %q, want %q", win.label, wantLabel)
		}
	})

	t.Run("custom", func(t *testing.T) {
		win, err := resolveRangeWindow("custom", "2026-01-05", "2026-01-20")
		if err != nil {
			t.Fatal(err)
		}
		wantStart := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
		wantEnd := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
		if !win.start.Equal(wantStart) {
			t.Errorf("start = %v, want %v", win.start, wantStart)
		}
		if !win.end.Equal(wantEnd) {
			t.Errorf("end = %v, want %v", win.end, wantEnd)
		}
		// Previous period: 15 days (span of Jan 5..Jan 20) before Jan 5.
		// span = 15 days; prevEnd = Jan 4; prevStart = Dec 20.
		wantPrevEnd := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
		wantPrevStart := wantPrevEnd.AddDate(0, 0, -15)
		if !win.prevEnd.Equal(wantPrevEnd) {
			t.Errorf("prevEnd = %v, want %v", win.prevEnd, wantPrevEnd)
		}
		if !win.prevStart.Equal(wantPrevStart) {
			t.Errorf("prevStart = %v, want %v", win.prevStart, wantPrevStart)
		}
	})

	t.Run("custom_to_before_from", func(t *testing.T) {
		_, err := resolveRangeWindow("custom", "2026-01-20", "2026-01-05")
		if err == nil {
			t.Fatal("expected error for to-before-from")
		}
	})
}

// TestRenderRangeSummaryTableCompare verifies the two-row + delta
// rendering path directly against known data.
func TestRenderRangeSummaryTableCompare(t *testing.T) {
	view := rangeView{
		label: "2026-07",
		current: metrics.Summary{
			RequestCount:      100,
			LocalCount:        50,
			FrontierCount:     40,
			FusionCount:       10,
			TOONSavingsTokens: 5000,
		},
		previous: metrics.Summary{
			RequestCount:      80,
			LocalCount:        30,
			FrontierCount:     45,
			FusionCount:       5,
			TOONSavingsTokens: 3000,
		},
		compare: true,
	}
	var b bytes.Buffer
	if err := renderRangeSummaryTable(view, 0.002, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"PERIOD", "2026-07", "previous", "Δ", "+20", "+2,000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestRenderRangeSummaryTableNoCompare verifies single-row output
// (no --compare).
func TestRenderRangeSummaryTableNoCompare(t *testing.T) {
	view := rangeView{
		label: "2026-07",
		current: metrics.Summary{
			RequestCount:      42,
			TOONSavingsTokens: 1000,
		},
		compare: false,
	}
	var b bytes.Buffer
	if err := renderRangeSummaryTable(view, 0.002, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "Δ") {
		t.Errorf("single-row output should not contain delta\n%s", out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("output missing request count\n%s", out)
	}
}

// TestRenderRangeSummaryJSONCompare verifies the JSON shape with
// --compare includes the previous object.
func TestRenderRangeSummaryJSONCompare(t *testing.T) {
	view := rangeView{
		label:    "2026-07",
		current:  metrics.Summary{RequestCount: 10, TOONSavingsTokens: 100},
		previous: metrics.Summary{RequestCount: 8, TOONSavingsTokens: 80},
		compare:  true,
	}
	var b bytes.Buffer
	if err := renderRangeSummaryJSON(view, 0.002, &b); err != nil {
		t.Fatal(err)
	}
	var arr []rangeSummaryJSON
	if err := json.Unmarshal(b.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
	if arr[0].Period != "2026-07" {
		t.Errorf("period = %q", arr[0].Period)
	}
	if arr[0].Previous == nil {
		t.Fatal("expected non-nil previous")
	}
	if arr[0].Previous.TotalRequests != 8 {
		t.Errorf("previous requests = %d, want 8", arr[0].Previous.TotalRequests)
	}
}

// TestSignedInt verifies the delta formatting helper.
func TestSignedInt(t *testing.T) {
	for in, want := range map[int]string{
		0:    "+0",
		5:    "+5",
		-5:   "-5",
		1000: "+1,000",
		-100: "-100",
	} {
		if got := signedInt(in); got != want {
			t.Errorf("signedInt(%d) = %q, want %q", in, got, want)
		}
	}
}
