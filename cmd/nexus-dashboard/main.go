// Command nexus-dashboard renders the per-request metrics written by the
// proxy into a human-readable daily savings summary. See render.go for
// the pure rendering layer; this file wires CLI flags to the metrics
// store and drives the day loop.
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

// usage is shown on -h / bad flags. Kept compact because the proxy is
// an operator tool, not an end-user app.
const usage = `nexus-dashboard — daily savings summary for the Nexus Proxy metrics store.

Usage:
  nexus-dashboard [--json] [--since YYYY-MM-DD] [--days N] [--db PATH] [--cost-per-1k RATE]

Flags:
  --json              Emit a JSON array instead of a plain-text table.
  --since YYYY-MM-DD  Start date (UTC, inclusive). Default: today.
  --days N            Window size in days. With --since, runs N days from
                      that date; without --since, covers the last N days
                      ending today. Default: 1 (today only).
  --db PATH           Metrics SQLite path. Default: $NEXUS_METRICS_DB,
                      then the XDG cache default
                      (~/.cache/nexus-proxy/metrics.db).
  --cost-per-1k RATE  USD per 1k tokens used to value TOON savings.
                      Default: $0.002 (env: NEXUS_COST_PER_1K).

The tool is read-only and safe to run while the proxy is live.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "nexus-dashboard: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable core: args and output are parameters so a future
// test can drive the full CLI (flags + store + render) end to end. The
// day loop calls Store.DailySummary once per queried day; the store is
// opened read-mostly (the proxy is the sole writer).
func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("nexus-dashboard", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var (
		asJSON     bool
		sinceRaw   string
		days       int
		dbPath     string
		costPer1kF string
	)
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of a plain-text table")
	fs.StringVar(&sinceRaw, "since", "", "start date YYYY-MM-DD (UTC, inclusive)")
	fs.IntVar(&days, "days", 0, "window size in days (default: today only)")
	fs.StringVar(&dbPath, "db", "", "metrics SQLite path")
	fs.StringVar(&costPer1kF, "cost-per-1k", "", "USD per 1k tokens for TOON savings valuation")

	if err := fs.Parse(args); err != nil {
		return err
	}

	costPer1k, err := resolveCostPer1k(costPer1kF)
	if err != nil {
		return err
	}

	path := resolveDBPath(dbPath)
	store, err := metrics.Open(path)
	if err != nil {
		return fmt.Errorf("open metrics store %q: %w", path, err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			slog.Warn("dashboard: close metrics store", "err", cerr)
		}
	}()

	start, end, err := resolveRange(sinceRaw, days)
	if err != nil {
		return err
	}

	summs := make([]metrics.Summary, 0, len(daysRange(start, end)))
	for _, day := range daysRange(start, end) {
		s, err := store.DailySummary(day)
		if err != nil {
			return fmt.Errorf("daily summary for %s: %w", day.Format("2006-01-02"), err)
		}
		summs = append(summs, s)
	}

	if asJSON {
		return renderJSON(summs, costPer1k, out)
	}
	return renderTable(summs, costPer1k, out)
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

// resolveRange turns the --since / --days flags into an inclusive
// [start, end] UTC day pair. Resolution rules:
//   - neither flag        → just today (start == end == today)
//   - --days N only       → last N days ending today
//   - --since DATE only   → DATE through today
//   - --since + --days N  → DATE through DATE+N-1
//
// "Today" is fixed once per invocation so a run that straddles
// midnight UTC stays internally consistent.
func resolveRange(sinceRaw string, days int) (start, end time.Time, err error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)

	var since time.Time
	hasSince := sinceRaw != ""
	if hasSince {
		since, err = time.Parse("2006-01-02", sinceRaw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--since: want YYYY-MM-DD, got %q", sinceRaw)
		}
		since = since.UTC()
	}

	switch {
	case hasSince && days > 0:
		start = since
		end = since.Add(time.Duration(days-1) * 24 * time.Hour)
	case hasSince:
		start = since
		end = today
	case days > 0:
		start = today.Add(time.Duration(-(days - 1)) * 24 * time.Hour)
		end = today
	default:
		start = today
		end = today
	}
	return start, end, nil
}
