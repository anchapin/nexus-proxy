// dashboard_render.go contains the pure rendering functions for the `nexus
// dashboard` subcommand. It is kept separate from dashboard.go so the
// rendering logic is testable without importing flag or io.
//
// The rendering layer reads from the same SQLite metrics store the proxy
// writes to (issue #4) and never writes to it, so it is safe to run
// concurrently with a live proxy.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// costDivisor converts a raw token count into the per-1k billing unit
// used by frontier pricing. Kept as a named constant so the savings
// formula reads identically to its documentation.
const costDivisor = 1000.0

// savingsUSD estimates the dollars saved by TOON prompt compression for
// a single day's Summary. TOON compression shaves tokens off the prompt
// before it reaches the upstream; those tokens would otherwise have been
// billed at the frontier rate (costPer1k, USD per 1k tokens).
//
// This is a deliberately conservative figure: the metrics schema does
// not expose per-route input-token sums, so the larger savings from
// routing to a free local model are NOT counted here — only the tokens
// TOON physically removed are valued. The dashboard column header makes
// this explicit ("$$ SAVED (TOON)").
func savingsUSD(s metrics.Summary, costPer1k float64) float64 {
	return float64(s.TOONSavingsTokens) / costDivisor * costPer1k
}

// allEmpty reports whether every day in the slice has zero requests.
// Used to decide whether to append a "no data" hint after the table.
func allEmpty(summs []metrics.Summary) bool {
	for _, s := range summs {
		if s.RequestCount > 0 {
			return false
		}
	}
	return true
}

// renderDashboardTable writes a plain-text, tab-aligned table to w. The header
// row is ALWAYS emitted — including when the store is empty — so an
// operator piped into less/awk still sees a recognisable table. When
// every queried day has zero requests, a short "no requests recorded"
// hint is appended below the table.
func renderDashboardTable(summs []metrics.Summary, costPer1k float64, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Header. The route distribution is split into three columns
	// (LOCAL / FRONTIER / FUSION) per the issue spec.
	fmt.Fprintln(tw, "DATE\tTOTAL\tLOCAL\tFRONTIER\tFUSION\tTOON SAVED\t$$ SAVED (TOON)")
	for _, s := range summs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			s.Date.Format("2006-01-02"),
			s.RequestCount,
			s.LocalCount,
			s.FrontierCount,
			s.FusionCount,
			comma(s.TOONSavingsTokens),
			formatUSD(savingsUSD(s, costPer1k)),
		)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("dashboard: flush table: %w", err)
	}
	if allEmpty(summs) {
		// Printed after the table (not as a row) so it never
		// collides with tabwriter column alignment.
		if _, err := fmt.Fprintln(w, "(no requests recorded for this period)"); err != nil {
			return fmt.Errorf("dashboard: write hint: %w", err)
		}
	}
	return nil
}

// dayJSON is the per-day object emitted by renderDashboardJSON. Field names are
// snake_case to match the conventions of the surrounding OpenAI-style
// API surface; the shape is stable and additive-only.
type dayJSON struct {
	Date                string  `json:"date"`
	TotalRequests       int     `json:"total_requests"`
	Local               int     `json:"local"`
	Frontier            int     `json:"frontier"`
	Fusion              int     `json:"fusion"`
	TOONSavedTokens     int     `json:"toon_saved_tokens"`
	EstimatedSavingsUSD float64 `json:"estimated_savings_usd"`
}

// renderDashboardJSON writes the summaries as a JSON array. The output is a top
// level array (not an object) so it pipes cleanly into jq / other CLIs.
// Empty stores yield an array of zero-valued day objects — one per
// queried day — rather than an empty array, so consumers can tell a
// missing day from an unqueried one.
func renderDashboardJSON(summs []metrics.Summary, costPer1k float64, w io.Writer) error {
	out := make([]dayJSON, 0, len(summs))
	for _, s := range summs {
		out = append(out, dayJSON{
			Date:                s.Date.Format("2006-01-02"),
			TotalRequests:       s.RequestCount,
			Local:               s.LocalCount,
			Frontier:            s.FrontierCount,
			Fusion:              s.FusionCount,
			TOONSavedTokens:     s.TOONSavingsTokens,
			EstimatedSavingsUSD: roundUSD(savingsUSD(s, costPer1k)),
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("dashboard: encode json: %w", err)
	}
	return nil
}

// formatUSD renders a dollar amount with four decimal places. Four
// decimals are required because TOON savings on a single day are
// frequently sub-cent ($0.0008 is typical); two decimals would round
// most real days to "$0.00" and hide the signal.
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.4f", v)
}

// roundUSD truncates to four decimal places so JSON output is stable
// across float representations (avoids trailing-digit noise from the
// multiply/divide). Mirrors the precision of formatUSD.
func roundUSD(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// comma inserts thousands separators into a non-negative integer. The
// stdlib fmt verb set has no thousands-grouping flag, so this is the
// minimal hand-rolled equivalent. Negatives are handled by preserving
// the leading sign.
func comma(n int) string {
	s := strconv.Itoa(n)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// daysRange returns the inclusive list of UTC days from start through
// end, ascending. Both endpoints are truncated to 24h first so callers
// can pass arbitrary instants. A start after end yields a single-element
// slice containing the (truncated) start.
func daysRange(start, end time.Time) []time.Time {
	s := start.UTC().Truncate(24 * time.Hour)
	e := end.UTC().Truncate(24 * time.Hour)
	if e.Before(s) {
		e = s
	}
	var days []time.Time
	for d := s; !d.After(e); d = d.Add(24 * time.Hour) {
		days = append(days, d)
	}
	return days
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

// --- Range mode (issue #1170) ---------------------------------------------
//
// Range mode collapses a weekly / monthly / quarterly / custom window
// into a single aggregate Summary via Store.RangeSummary (one SQL
// round-trip). --compare fetches the previous equivalent period and
// emits a delta row. These types and the resolver are pure and tested
// without a real store; the renderers below mirror the per-day table /
// JSON shapes so an operator switching between modes sees a consistent
// column layout.

// rangeWindow describes a resolved range-mode period. start and end are
// inclusive UTC-day truncations; prevStart / prevEnd are the same-length
// window immediately preceding start (used by --compare). label is the
// human / JSON period identifier (e.g. "2026-07", "2026-Q3").
type rangeWindow struct {
	label     string
	start     time.Time
	end       time.Time
	prevStart time.Time
	prevEnd   time.Time
}

// rangeView is the data the range renderers consume: the current-period
// Summary, an optional previous-period Summary, and the period label.
type rangeView struct {
	label    string
	current  metrics.Summary
	previous metrics.Summary
	compare  bool
}

// validRangeNames is the set of accepted --range values. Kept as a map
// for O(1) membership; the switch in resolveRangeWindow guarantees
// exhaustiveness.
var validRangeNames = map[string]bool{
	"weekly":    true,
	"monthly":   true,
	"quarterly": true,
	"custom":    true,
}

// resolveRangeWindow maps the --range flag (plus --from / --to for
// custom) into a rangeWindow anchored on "today" (UTC). Period
// semantics:
//
//   - weekly    → last 7 days ending today (inclusive)
//   - monthly   → first day of current month through today (month-to-date)
//   - quarterly → first day of current calendar quarter through today
//   - custom    → --from through --to (both required)
//
// The previous equivalent period (prevStart..prevEnd) has the same
// length as the current window and immediately precedes it. This keeps
// the compare delta meaningful regardless of where in the month we are.
func resolveRangeWindow(name, fromRaw, toRaw string) (rangeWindow, error) {
	if !validRangeNames[name] {
		return rangeWindow{}, fmt.Errorf("--range: want weekly|monthly|quarterly|custom, got %q", name)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	var win rangeWindow

	switch name {
	case "weekly":
		win.start = today.AddDate(0, 0, -6) // last 7 days inclusive
		win.end = today
		win.label = win.start.Format("2006-01-02")
	case "monthly":
		win.start = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		win.end = today
		win.label = today.Format("2006-01")
	case "quarterly":
		qStartMonth := ((int(today.Month())-1)/3)*3 + 1 // 1, 4, 7, or 10
		win.start = time.Date(today.Year(), time.Month(qStartMonth), 1, 0, 0, 0, 0, time.UTC)
		win.end = today
		win.label = fmt.Sprintf("%d-Q%d", today.Year(), (int(today.Month())-1)/3+1)
	case "custom":
		if fromRaw == "" || toRaw == "" {
			return rangeWindow{}, fmt.Errorf("--range custom requires --from and --to")
		}
		from, err := time.Parse("2006-01-02", fromRaw)
		if err != nil {
			return rangeWindow{}, fmt.Errorf("--from: want YYYY-MM-DD, got %q", fromRaw)
		}
		to, err := time.Parse("2006-01-02", toRaw)
		if err != nil {
			return rangeWindow{}, fmt.Errorf("--to: want YYYY-MM-DD, got %q", toRaw)
		}
		from = from.UTC()
		to = to.UTC()
		if to.Before(from) {
			return rangeWindow{}, fmt.Errorf("--to %s is before --from %s", toRaw, fromRaw)
		}
		win.start = from
		win.end = to
		win.label = from.Format("2006-01-02")
	}

	// Previous equivalent period: same length, immediately before start.
	span := win.end.Sub(win.start)
	win.prevEnd = win.start.AddDate(0, 0, -1)
	win.prevStart = win.prevEnd.Add(-span)
	return win, nil
}

// rangeSummaryJSON is the per-period object emitted by
// renderRangeSummaryJSON. The "previous" field is omitted (via omitempty
// on the pointer) when --compare is not set.
type rangeSummaryJSON struct {
	Period   string            `json:"period"`
	Summary  rangeMetricsJSON  `json:"summary"`
	Previous *rangeMetricsJSON `json:"previous,omitempty"`
}

// rangeMetricsJSON carries the same metric set as the per-day dayJSON
// but uses "period" instead of "date" and omits the date field so the
// range output is self-describing.
type rangeMetricsJSON struct {
	TotalRequests       int     `json:"total_requests"`
	Local               int     `json:"local"`
	Frontier            int     `json:"frontier"`
	Fusion              int     `json:"fusion"`
	TOONSavedTokens     int     `json:"toon_saved_tokens"`
	EstimatedSavingsUSD float64 `json:"estimated_savings_usd"`
}

// toRangeMetricsJSON converts a Summary into the JSON metric subset.
func toRangeMetricsJSON(s metrics.Summary, costPer1k float64) rangeMetricsJSON {
	return rangeMetricsJSON{
		TotalRequests:       s.RequestCount,
		Local:               s.LocalCount,
		Frontier:            s.FrontierCount,
		Fusion:              s.FusionCount,
		TOONSavedTokens:     s.TOONSavingsTokens,
		EstimatedSavingsUSD: roundUSD(savingsUSD(s, costPer1k)),
	}
}

// renderRangeSummaryJSON writes a single-element JSON array containing
// the current period (and, with --compare, the previous period). The
// array wrapper keeps it a drop-in for tooling that already consumes
// the per-day JSON array shape.
func renderRangeSummaryJSON(view rangeView, costPer1k float64, w io.Writer) error {
	out := rangeSummaryJSON{
		Period:  view.label,
		Summary: toRangeMetricsJSON(view.current, costPer1k),
	}
	if view.compare {
		prev := toRangeMetricsJSON(view.previous, costPer1k)
		out.Previous = &prev
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	arr := []rangeSummaryJSON{out}
	if err := enc.Encode(arr); err != nil {
		return fmt.Errorf("dashboard: encode range json: %w", err)
	}
	return nil
}

// renderRangeSummaryTable writes a tab-aligned table for range mode.
// Without --compare it prints one data row; with --compare it prints
// current, previous, and delta rows. The delta row uses a leading "Δ"
// label and signed integer formatting.
func renderRangeSummaryTable(view rangeView, costPer1k float64, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PERIOD\tTOTAL\tLOCAL\tFRONTIER\tFUSION\tTOON SAVED\t$$ SAVED (TOON)")

	writeRangeRow(tw, view.label, view.current, costPer1k)

	if view.compare {
		writeRangeRow(tw, "previous", view.previous, costPer1k)
		writeRangeDeltaRow(tw, "Δ", view.current, view.previous, costPer1k)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("dashboard: flush range table: %w", err)
	}
	if view.current.RequestCount == 0 && (!view.compare || view.previous.RequestCount == 0) {
		if _, err := fmt.Fprintln(w, "(no requests recorded for this period)"); err != nil {
			return fmt.Errorf("dashboard: write hint: %w", err)
		}
	}
	return nil
}

// writeRangeRow emits one data line into the tabwriter.
func writeRangeRow(tw *tabwriter.Writer, label string, s metrics.Summary, costPer1k float64) {
	fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
		label,
		s.RequestCount,
		s.LocalCount,
		s.FrontierCount,
		s.FusionCount,
		comma(s.TOONSavingsTokens),
		formatUSD(savingsUSD(s, costPer1k)),
	)
}

// writeRangeDeltaRow emits the signed difference between current and
// previous for each numeric column.
func writeRangeDeltaRow(tw *tabwriter.Writer, label string, cur, prev metrics.Summary, costPer1k float64) {
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		label,
		signedInt(cur.RequestCount-prev.RequestCount),
		signedInt(cur.LocalCount-prev.LocalCount),
		signedInt(cur.FrontierCount-prev.FrontierCount),
		signedInt(cur.FusionCount-prev.FusionCount),
		signedInt(cur.TOONSavingsTokens-prev.TOONSavingsTokens),
		signedUSD(savingsUSD(cur, costPer1k)-savingsUSD(prev, costPer1k)),
	)
}

// signedInt renders an integer with an explicit leading "+" when
// non-negative, so delta columns are unambiguous.
func signedInt(n int) string {
	if n >= 0 {
		return "+" + comma(n)
	}
	return comma(n)
}

// signedUSD renders a dollar delta with an explicit leading "+" when
// non-negative, mirroring signedInt.
func signedUSD(v float64) string {
	if v >= 0 {
		return "+" + formatUSD(v)
	}
	return formatUSD(v)
}
