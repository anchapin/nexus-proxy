// Subcommand: `nexus dashboard`. Reads the per-request metrics written by
// the proxy into a human-readable daily savings summary. The dashboard is
// read-only and safe to run while the proxy is live.
//
// This file is the CLI adapter; the rendering logic lives in
// dashboard_render.go. Keeping the two separate means the rendering is
// testable without importing flag or io.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// defaultCostPer1k is the USD-per-1k-tokens rate used to value TOON
// token savings when neither --cost-per-1k nor NEXUS_COST_PER_1K is
// set. It mirrors internal/judge's default so the dashboard and the
// judge agree on a dollar figure for the same token count.
const defaultCostPer1k = 0.002

// dashboardUsage is shown on -h / bad flags. Kept compact because the
// proxy is an operator tool, not an end-user app.
const dashboardUsage = `nexus dashboard — daily savings summary for the Nexus Proxy.

Usage:
  nexus dashboard [flags]

Per-day mode (default):
  --json              Emit a JSON array instead of a plain-text table.
  --since YYYY-MM-DD  Start date (UTC, inclusive). Default: today.
  --days N            Window size in days. With --since, runs N days from
                      that date; without --since, covers the last N days
                      ending today. Default: 1 (today only).

Range mode (collapses the window into a single aggregate row):
  --range weekly|monthly|quarterly|custom
                      Aggregate the period into one rollup row.
  --from YYYY-MM-DD   Custom-range start (with --range custom).
  --to   YYYY-MM-DD   Custom-range end (with --range custom).
  --compare           Emit the previous equivalent period alongside the
                      current period with a delta row/column. Incompatible
                      with per-day mode (--days).

Common flags:
  --db PATH           Metrics SQLite path. Default: $NEXUS_METRICS_DB,
                      then the XDG cache default
                      (~/.cache/nexus-proxy/metrics.db).
  --cost-per-1k RATE  USD per 1k tokens used to value TOON savings.
                      Default: $0.002 (env: NEXUS_COST_PER_1K).

--range and --days are mutually exclusive. The tool is read-only and
safe to run while the proxy is live.
`

// runDashboard is the testable core of the `nexus dashboard` subcommand.
// args and output are parameters so a future test can drive the full CLI
// end-to-end. The function returns a process exit code (0 or 1) rather
// than calling os.Exit so tests can assert against it without spawning
// a subprocess.
func runDashboard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, dashboardUsage) }

	var (
		asJSON     bool
		sinceRaw   string
		days       int
		dbPath     string
		costPer1kF string
		rangeName  string
		fromRaw    string
		toRaw      string
		compare    bool
	)
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of a plain-text table")
	fs.StringVar(&sinceRaw, "since", "", "start date YYYY-MM-DD (UTC, inclusive)")
	fs.IntVar(&days, "days", 0, "window size in days (default: today only)")
	fs.StringVar(&dbPath, "db", "", "metrics SQLite path")
	fs.StringVar(&costPer1kF, "cost-per-1k", "", "USD per 1k tokens for TOON savings valuation")
	fs.StringVar(&rangeName, "range", "", "aggregate mode: weekly|monthly|quarterly|custom")
	fs.StringVar(&fromRaw, "from", "", "custom-range start YYYY-MM-DD (with --range custom)")
	fs.StringVar(&toRaw, "to", "", "custom-range end YYYY-MM-DD (with --range custom)")
	fs.BoolVar(&compare, "compare", false, "emit previous equivalent period with delta (range mode only)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	costPer1k, err := resolveCostPer1k(costPer1kF)
	if err != nil {
		fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
		return 1
	}

	// --range and --days are mutually exclusive; --compare / --from / --to
	// are only valid in range mode.
	if rangeName != "" && days > 0 {
		fmt.Fprintf(stderr, "nexus dashboard: --range and --days are mutually exclusive\n")
		return 1
	}
	if rangeName == "" && (compare || fromRaw != "" || toRaw != "") {
		fmt.Fprintf(stderr, "nexus dashboard: --compare/--from/--to require --range\n")
		return 1
	}

	rangeMode := rangeName != ""

	path := resolveDBPath(dbPath)
	store, err := metrics.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "nexus dashboard: open metrics store %q: %v\n", path, err)
		return 1
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			slog.Warn("dashboard: close metrics store", "err", cerr)
		}
	}()

	if rangeMode {
		return runRangeDashboard(store, rangeName, fromRaw, toRaw, compare, costPer1k, asJSON, stdout, stderr)
	}

	start, end, err := resolveRange(sinceRaw, days)
	if err != nil {
		fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
		return 1
	}

	summs := make([]metrics.Summary, 0, len(daysRange(start, end)))
	for _, day := range daysRange(start, end) {
		s, err := store.DailySummary(day)
		if err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: daily summary for %s: %v\n", day.Format("2006-01-02"), err)
			return 1
		}
		summs = append(summs, s)
	}

	if asJSON {
		if err := renderDashboardJSON(summs, costPer1k, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
			return 1
		}
	} else {
		if err := renderDashboardTable(summs, costPer1k, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
			return 1
		}
	}
	return 0
}

// runRangeDashboard handles the --range mode: it collapses the window
// into a single aggregate Summary via Store.RangeSummary (one SQL
// round-trip instead of N per-day queries) and renders it through the
// range-specific table / JSON renderers. When --compare is set the
// previous equivalent period is fetched alongside and a delta row is
// emitted.
func runRangeDashboard(store metrics.Store, rangeName, fromRaw, toRaw string, compare bool, costPer1k float64, asJSON bool, stdout, stderr io.Writer) int {
	win, err := resolveRangeWindow(rangeName, fromRaw, toRaw)
	if err != nil {
		fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
		return 1
	}

	cur, err := store.RangeSummary(win.start, win.end.Add(24*time.Hour))
	if err != nil {
		fmt.Fprintf(stderr, "nexus dashboard: range summary: %v\n", err)
		return 1
	}

	var prev metrics.Summary
	hasPrev := false
	if compare {
		prev, err = store.RangeSummary(win.prevStart, win.prevEnd.Add(24*time.Hour))
		if err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: previous range summary: %v\n", err)
			return 1
		}
		hasPrev = true
	}

	view := rangeView{
		label:    win.label,
		current:  cur,
		previous: prev,
		compare:  hasPrev,
	}

	if asJSON {
		if err := renderRangeSummaryJSON(view, costPer1k, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
			return 1
		}
	} else {
		if err := renderRangeSummaryTable(view, costPer1k, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus dashboard: %v\n", err)
			return 1
		}
	}
	return 0
}

// resolveDBPath picks the SQLite path in priority order: --db flag,
// then NEXUS_METRICS_DB, then the same XDG-style default the proxy
// uses (config.DefaultMetricsDBPath). Keeping the default identical to
// the proxy means a zero-flag invocation "just works" against whatever
// the proxy is already writing.
func resolveDBPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if v := os.Getenv("NEXUS_METRICS_DB"); v != "" {
		return v
	}
	return config.DefaultMetricsDBPath()
}

// resolveCostPer1k picks the savings valuation rate: flag, then
// NEXUS_COST_PER1k, then defaultCostPer1k. An invalid rate is a hard
// error rather than a silent fallback — a wrong rate silently
// misreports savings, which is worse than a loud failure.
func resolveCostPer1k(flagVal string) (float64, error) {
	if flagVal != "" {
		v, err := strconv.ParseFloat(flagVal, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("--cost-per-1k: invalid rate %q", flagVal)
		}
		return v, nil
	}
	if v := os.Getenv("NEXUS_COST_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("NEXUS_COST_PER_1K: invalid rate %q", v)
		}
		return f, nil
	}
	return defaultCostPer1k, nil
}
