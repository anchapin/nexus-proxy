// Command nexus-dashboard renders the per-request metrics written by the
// proxy into a human-readable daily savings summary. It reads the same
// SQLite store the proxy writes to (issue #4) and never writes to it, so
// it is safe to run concurrently with a live proxy.
//
// The binary is self-contained: it depends only on internal/metrics and
// the standard library. Rendering is split into pure functions (this
// file, render.go) so tests can golden-file the output without spinning
// up a database, while a separate integration test exercises the real
// Store.DailySummary read path.
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

// renderTable writes a plain-text, tab-aligned table to w. The header
// row is ALWAYS emitted — including when the store is empty — so an
// operator piped into less/awk still sees a recognisable table. When
// every queried day has zero requests, a short "no requests recorded"
// hint is appended below the table.
func renderTable(summs []metrics.Summary, costPer1k float64, w io.Writer) error {
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

// dayJSON is the per-day object emitted by renderJSON. Field names are
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

// renderJSON writes the summaries as a JSON array. The output is a top
// level array (not an object) so it pipes cleanly into jq / other CLIs.
// Empty stores yield an array of zero-valued day objects — one per
// queried day — rather than an empty array, so consumers can tell a
// missing day from an unqueried one.
func renderJSON(summs []metrics.Summary, costPer1k float64, w io.Writer) error {
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
