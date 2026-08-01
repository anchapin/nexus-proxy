// Package handlers — dashboard.go serves the built-in web dashboard
// (issue #1182). The dashboard is a single self-contained HTML page
// (inline CSS/JS, no external assets) rendering savings and routing
// metrics from the SQLite metrics store. It is read-only and safe to
// serve while the proxy is live — it issues the same DailySummary
// aggregate the `nexus dashboard` CLI uses.
//
// The endpoint is disabled by default (NEXUS_DASHBOARD_ENDPOINT=false)
// and, when enabled, follows the same auth posture as /status: gated
// by NEXUS_PROXY_API_KEY unless NEXUS_DASHBOARD_PUBLIC=true. The route
// is registered inside the mux that SecurityHeaders wraps, so it
// inherits the proxy's response-hardening headers automatically.

package handlers

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// defaultDashboardCostPer1k is the USD-per-1k-tokens rate used to value
// TOON token savings when the caller does not supply one. It mirrors
// cmd/nexus's defaultCostPer1k so the web dashboard and the `nexus
// dashboard` CLI agree on a dollar figure for the same token count.
const defaultDashboardCostPer1k = 0.002

// dashboardRanges are the selectable windows exposed via ?range=. The
// order is the iteration order for the range-picker UI; keys are the
// query-string values an operator types.
var dashboardRanges = []struct {
	Label string
	Key   string
	Days  int
}{
	{"Last 24 hours", "24h", 1},
	{"Last 7 days", "7d", 7},
	{"Last 30 days", "30d", 30},
}

// DashboardStore is the subset of metrics.Store the dashboard needs.
// Declared locally so the handler is testable with a fake store and so
// a nil store can short-circuit to a "metrics disabled" page. The
// concrete metrics.SQLiteStore satisfies this interface.
type DashboardStore interface {
	DailySummary(date time.Time) (metrics.Summary, error)
}

// DashboardDeps carries the collaborators the dashboard handler needs.
type DashboardDeps struct {
	// Store supplies per-day metric aggregates. When nil the handler
	// serves a static "metrics disabled" page (HTTP 200) so the
	// endpoint degrades gracefully instead of 500-ing.
	Store DashboardStore
	// CostPer1K values TOON token savings (USD per 1k tokens). Zero
	// falls back to defaultDashboardCostPer1k.
	CostPer1K float64
	// Now is the injectable clock; nil → time.Now. Used so tests can
	// pin "today" without touching the system clock.
	Now func() time.Time
}

// dashboardDayRow is one row of the per-day table in the rendered page.
type dashboardDayRow struct {
	Date            string  `json:"date"`
	Requests        int     `json:"requests"`
	Local           int     `json:"local"`
	Frontier        int     `json:"frontier"`
	Fusion          int     `json:"fusion"`
	InputTokens     int     `json:"input_tokens"`
	TOONSavedTokens int     `json:"toon_saved_tokens"`
	SavingsUSD      float64 `json:"savings_usd"`
	Errors          int     `json:"errors"`
}

// dashboardTotals is the rolled-up window aggregate shown at the top.
type dashboardTotals struct {
	Range             string  `json:"range"`
	Requests          int     `json:"requests"`
	Local             int     `json:"local"`
	Frontier          int     `json:"frontier"`
	Fusion            int     `json:"fusion"`
	InputTokens       int     `json:"input_tokens"`
	TOONSavedTokens   int     `json:"toon_saved_tokens"`
	TOONSavingsUSD    float64 `json:"toon_savings_usd"`
	RoutingSavingsUSD float64 `json:"routing_savings_usd"`
	Errors            int     `json:"errors"`
}

// dashboardModel holds the data injected into the HTML template.
type dashboardModel struct {
	RangeKey   string
	RangeLabel string
	RangeLinks []dashboardRangeLink
	Totals     dashboardTotals
	Days       []dashboardDayRow
	Generated  string
	HasData    bool
}

// dashboardRangeLink is one entry in the range-picker nav.
type dashboardRangeLink struct {
	Key    string
	Label  string
	Active bool
}

// Dashboard returns an http.HandlerFunc that serves the built-in web
// dashboard. The handler is read-only and safe to invoke concurrently
// with live proxy traffic.
func Dashboard(deps DashboardDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	costPer1k := deps.CostPer1K
	if costPer1k <= 0 {
		costPer1k = defaultDashboardCostPer1k
	}
	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if deps.Store == nil {
			renderDashboardDisabled(w, tmpl)
			return
		}

		rangeKey := sanitizeRangeKey(r.URL.Query().Get("range"))
		days := daysForKey(rangeKey)
		today := now().UTC().Truncate(24 * time.Hour)

		rows := make([]dashboardDayRow, 0, days)
		var tot dashboardTotals
		tot.Range = rangeKey
		hasData := false
		for i := days - 1; i >= 0; i-- {
			day := today.AddDate(0, 0, -i)
			s, err := deps.Store.DailySummary(day)
			if err != nil {
				slog.Warn("dashboard: daily summary query failed",
					slog.String("date", day.Format("2006-01-02")),
					slog.Any("err", err),
				)
				rows = append(rows, dashboardDayRow{Date: day.Format("2006-01-02")})
				continue
			}
			if s.RequestCount > 0 {
				hasData = true
			}
			toonUSD := toonSavingsUSD(s, costPer1k)
			rows = append(rows, dashboardDayRow{
				Date:            day.Format("2006-01-02"),
				Requests:        s.RequestCount,
				Local:           s.LocalCount,
				Frontier:        s.FrontierCount,
				Fusion:          s.FusionCount,
				InputTokens:     s.TotalInputTokens,
				TOONSavedTokens: s.TOONSavingsTokens,
				SavingsUSD:      toonUSD,
				Errors:          s.ErrorCount,
			})
			tot.Requests += s.RequestCount
			tot.Local += s.LocalCount
			tot.Frontier += s.FrontierCount
			tot.Fusion += s.FusionCount
			tot.InputTokens += s.TotalInputTokens
			tot.TOONSavedTokens += s.TOONSavingsTokens
			tot.TOONSavingsUSD += toonUSD
			tot.RoutingSavingsUSD += s.SavingsTotal
			tot.Errors += s.ErrorCount
		}
		tot.TOONSavingsUSD = roundTo(tot.TOONSavingsUSD, 4)
		tot.RoutingSavingsUSD = roundTo(tot.RoutingSavingsUSD, 4)

		model := dashboardModel{
			RangeKey:   rangeKey,
			RangeLabel: labelForKey(rangeKey),
			RangeLinks: buildRangeLinks(rangeKey),
			Totals:     tot,
			Days:       rows,
			Generated:  now().UTC().Format(time.RFC3339),
			HasData:    hasData,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.Execute(w, model); err != nil {
			// Headers are already committed by Execute, so we can
			// only log — writing a 500 would corrupt the partial
			// response. This mirrors the SecurityHeaders contract.
			slog.Warn("dashboard: render template", slog.Any("err", err))
		}
	}
}

// renderDashboardDisabled emits the static "metrics disabled" page. The
// store is optional, so this is a graceful degradation (HTTP 200), not
// an error.
func renderDashboardDisabled(w http.ResponseWriter, tmpl *template.Template) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	model := dashboardModel{
		RangeKey:   "24h",
		RangeLabel: labelForKey("24h"),
		RangeLinks: buildRangeLinks("24h"),
		HasData:    false,
	}
	if err := tmpl.Execute(w, model); err != nil {
		slog.Warn("dashboard: render disabled template", slog.Any("err", err))
	}
}

// toonSavingsUSD values TOON prompt-compression savings at the given
// per-1k-token rate. Mirrors cmd/nexus's savingsUSD so the web and CLI
// dashboards report identical figures for the same day.
func toonSavingsUSD(s metrics.Summary, costPer1k float64) float64 {
	return roundTo(float64(s.TOONSavingsTokens)/1000.0*costPer1k, 4)
}

// roundTo truncates v to n decimal places to keep JSON/HTML output
// stable across float representations (avoids trailing-digit noise).
func roundTo(v float64, n int) float64 {
	scale := 1.0
	for i := 0; i < n; i++ {
		scale *= 10
	}
	return float64(int64(v*scale+0.5)) / scale
}

// sanitizeRangeKey coerces the ?range= query value to a known key.
// Unknown / empty values default to "24h" so a bad link never 500s.
func sanitizeRangeKey(raw string) string {
	for _, r := range dashboardRanges {
		if strings.EqualFold(raw, r.Key) {
			return r.Key
		}
	}
	return "24h"
}

// daysForKey returns the day count for a sanitized range key.
func daysForKey(key string) int {
	for _, r := range dashboardRanges {
		if r.Key == key {
			return r.Days
		}
	}
	return 1
}

// labelForKey returns the human-readable label for a range key.
func labelForKey(key string) string {
	for _, r := range dashboardRanges {
		if r.Key == key {
			return r.Label
		}
	}
	return dashboardRanges[0].Label
}

// buildRangeLinks builds the range-picker nav, marking the active key.
func buildRangeLinks(active string) []dashboardRangeLink {
	links := make([]dashboardRangeLink, 0, len(dashboardRanges))
	for _, r := range dashboardRanges {
		links = append(links, dashboardRangeLink{
			Key:    r.Key,
			Label:  r.Label,
			Active: r.Key == active,
		})
	}
	return links
}

// DashboardJSON is an optional JSON view of the same aggregates the
// HTML dashboard renders. It is wired at the same path when the caller
// passes ?format=json, letting operators script against the dashboard
// without parsing HTML. Output shape is additive-only.
func DashboardJSON(deps DashboardDeps) http.HandlerFunc {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	costPer1k := deps.CostPer1K
	if costPer1k <= 0 {
		costPer1k = defaultDashboardCostPer1k
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Store == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
			return
		}
		rangeKey := sanitizeRangeKey(r.URL.Query().Get("range"))
		days := daysForKey(rangeKey)
		today := now().UTC().Truncate(24 * time.Hour)
		rows := make([]dashboardDayRow, 0, days)
		var tot dashboardTotals
		tot.Range = rangeKey
		for i := days - 1; i >= 0; i-- {
			day := today.AddDate(0, 0, -i)
			s, err := deps.Store.DailySummary(day)
			if err != nil {
				rows = append(rows, dashboardDayRow{Date: day.Format("2006-01-02")})
				continue
			}
			toonUSD := toonSavingsUSD(s, costPer1k)
			rows = append(rows, dashboardDayRow{
				Date:            day.Format("2006-01-02"),
				Requests:        s.RequestCount,
				Local:           s.LocalCount,
				Frontier:        s.FrontierCount,
				Fusion:          s.FusionCount,
				InputTokens:     s.TotalInputTokens,
				TOONSavedTokens: s.TOONSavingsTokens,
				SavingsUSD:      toonUSD,
				Errors:          s.ErrorCount,
			})
			tot.Requests += s.RequestCount
			tot.Local += s.LocalCount
			tot.Frontier += s.FrontierCount
			tot.Fusion += s.FusionCount
			tot.InputTokens += s.TotalInputTokens
			tot.TOONSavedTokens += s.TOONSavingsTokens
			tot.TOONSavingsUSD += toonUSD
			tot.RoutingSavingsUSD += s.SavingsTotal
			tot.Errors += s.ErrorCount
		}
		tot.TOONSavingsUSD = roundTo(tot.TOONSavingsUSD, 4)
		tot.RoutingSavingsUSD = roundTo(tot.RoutingSavingsUSD, 4)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"range":     rangeKey,
			"totals":    tot,
			"days":      rows,
			"generated": now().UTC().Format(time.RFC3339),
		})
	}
}

// dashboardHTML is the single self-contained page template. Inline CSS
// and JS — no external requests — so it works on an air-gapped host and
// never leaks the deployment's existence to a third-party CDN.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Nexus Proxy — Dashboard</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    background: #0f1115; color: #e6e6e6; line-height: 1.5;
  }
  header {
    padding: 1.25rem 1.5rem; border-bottom: 1px solid #232733;
    display: flex; flex-wrap: wrap; gap: .75rem; align-items: baseline;
  }
  header h1 { font-size: 1.15rem; margin: 0; font-weight: 600; }
  header .sub { color: #8b93a7; font-size: .85rem; }
  main { max-width: 1000px; margin: 0 auto; padding: 1.5rem; }
  nav.ranges { display: flex; gap: .5rem; margin-bottom: 1.25rem; flex-wrap: wrap; }
  nav.ranges a {
    text-decoration: none; padding: .35rem .75rem; border-radius: 6px;
    border: 1px solid #2c3140; color: #c7ccd9; font-size: .85rem;
  }
  nav.ranges a.active { background: #2563eb; border-color: #2563eb; color: #fff; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: .75rem; margin-bottom: 1.5rem; }
  .card { background: #171a22; border: 1px solid #232733; border-radius: 8px; padding: .9rem 1rem; }
  .card .k { font-size: .72rem; text-transform: uppercase; letter-spacing: .04em; color: #8b93a7; }
  .card .v { font-size: 1.4rem; font-weight: 600; margin-top: .25rem; }
  .card .v small { font-size: .8rem; color: #8b93a7; font-weight: 400; }
  table { width: 100%; border-collapse: collapse; font-size: .85rem; }
  th, td { text-align: right; padding: .5rem .6rem; border-bottom: 1px solid #232733; }
  th:first-child, td:first-child { text-align: left; }
  th { color: #8b93a7; font-weight: 600; font-size: .72rem; text-transform: uppercase; letter-spacing: .03em; }
  td.l, th.l { color: #4ade80; }
  td.f, th.f { color: #60a5fa; }
  td.s, th.s { color: #c084fc; }
  .pill { display: inline-block; padding: .05rem .45rem; border-radius: 999px; font-size: .72rem; }
  .ok { background: #052e16; color: #4ade80; }
  .err { background: #3b0d0d; color: #f87171; }
  .empty { color: #8b93a7; padding: 2rem 0; text-align: center; }
  footer { color: #5b6173; font-size: .75rem; padding: 1rem 1.5rem; border-top: 1px solid #232733; }
  @media (prefers-color-scheme: light) {
    body { background: #f6f7f9; color: #1a1d24; }
    header { border-color: #e2e5ea; }
    .card { background: #fff; border-color: #e2e5ea; }
    th, td { border-color: #e2e5ea; }
    footer { border-color: #e2e5ea; color: #6b7280; }
    .card .k, th, header .sub, .empty { color: #6b7280; }
    nav.ranges a { border-color: #d1d5db; color: #374151; }
    td.l, th.l { color: #15803d; }
    td.f, th.f { color: #1d4ed8; }
    td.s, th.s { color: #7e22ce; }
  }
</style>
</head>
<body>
<header>
  <h1>Nexus Proxy Dashboard</h1>
  <span class="sub">savings &amp; routing metrics · {{.RangeLabel}}</span>
</header>
<main>
  {{if not .HasData}}
  <div class="empty">No requests recorded for this period. Metrics begin populating once the proxy serves traffic.</div>
  {{else}}
  <nav class="ranges">
    {{range .RangeLinks}}<a href="?range={{.Key}}"{{if .Active}} class="active"{{end}}>{{.Label}}</a>{{end}}
  </nav>
  <section class="cards">
    <div class="card"><div class="k">Requests</div><div class="v">{{.Totals.Requests}}</div></div>
    <div class="card"><div class="k">Input tokens</div><div class="v">{{.Totals.InputTokens}}</div></div>
    <div class="card"><div class="k">TOON saved</div><div class="v">{{.Totals.TOONSavedTokens}}<small> tok</small></div></div>
    <div class="card"><div class="k">TOON savings</div><div class="v">${{printf "%.4f" .Totals.TOONSavingsUSD}}</div></div>
    <div class="card"><div class="k">Routing savings</div><div class="v">${{printf "%.4f" .Totals.RoutingSavingsUSD}}</div></div>
    <div class="card"><div class="k">Errors</div><div class="v">{{.Totals.Errors}}</div></div>
  </section>
  <table>
    <thead><tr><th>Date</th><th>Total</th><th class="l">Local</th><th class="f">Frontier</th><th class="s">Fusion</th><th>Tokens</th><th>TOON</th><th>$$ Saved</th><th>Status</th></tr></thead>
    <tbody>
    {{range .Days}}
      <tr>
        <td>{{.Date}}</td>
        <td>{{.Requests}}</td>
        <td class="l">{{.Local}}</td>
        <td class="f">{{.Frontier}}</td>
        <td class="s">{{.Fusion}}</td>
        <td>{{.InputTokens}}</td>
        <td>{{.TOONSavedTokens}}</td>
        <td>${{printf "%.4f" .SavingsUSD}}</td>
        <td>{{if gt .Errors 0}}<span class="pill err">{{.Errors}} err</span>{{else if gt .Requests 0}}<span class="pill ok">ok</span>{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</main>
<footer>Generated {{.Generated}} UTC · read-only · no external assets</footer>
</body>
</html>
`
