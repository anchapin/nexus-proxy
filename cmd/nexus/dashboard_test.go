package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
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
