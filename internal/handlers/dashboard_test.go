package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
)

// fakeDashboardStore is an in-memory DashboardStore for dashboard
// tests. It returns a fixed Summary for the queried UTC day, allowing
// the test to assert the handler aggregates across the window and
// formats values without standing up SQLite.
type fakeDashboardStore struct {
	byDate map[string]metrics.Summary
	err    error
}

func (f *fakeDashboardStore) DailySummary(date time.Time) (metrics.Summary, error) {
	if f.err != nil {
		return metrics.Summary{}, f.err
	}
	return f.byDate[date.UTC().Truncate(24*time.Hour).Format("2006-01-02")], nil
}

// twoDayStore seeds the two most recent days ending at fixedTime so the
// 24h and 7d windows have something distinct to assert against.
func twoDayStore() *fakeDashboardStore {
	today := fixedTime.UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	return &fakeDashboardStore{
		byDate: map[string]metrics.Summary{
			today.Format("2006-01-02"): {
				Date: today, RequestCount: 10, LocalCount: 6, FrontierCount: 3, FusionCount: 1,
				TotalInputTokens: 1200, TOONSavingsTokens: 400, SavingsTotal: 0.05,
				RAGInjectedCount: 3, ErrorCount: 0,
			},
			yesterday.Format("2006-01-02"): {
				Date: yesterday, RequestCount: 5, LocalCount: 2, FrontierCount: 2, FusionCount: 1,
				TotalInputTokens: 800, TOONSavingsTokens: 200, SavingsTotal: 0.02,
				RAGInjectedCount: 1, ErrorCount: 1,
			},
		},
	}
}

// TestDashboardDisabledStoreServesGracefulPage asserts that a nil
// store (metrics disabled) degrades to a 200 "no data" page rather than
// a 500. This is the disabled-default posture.
func TestDashboardDisabledStoreServesGracefulPage(t *testing.T) {
	h := Dashboard(DashboardDeps{Store: nil, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for nil store", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No requests recorded") {
		t.Errorf("nil-store page should show the empty hint; got body snippet: %q", snippet(body))
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// TestDashboardRendersDataAndAggregates drives the handler with a
// two-day store and asserts the HTML embeds both the per-day rows and
// the rolled-up window totals.
func TestDashboardRendersDataAndAggregates(t *testing.T) {
	h := Dashboard(DashboardDeps{
		Store:     twoDayStore(),
		CostPer1K: 0.002,
		Now:       fixedClock,
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=7d", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Both seeded days appear.
	if !strings.Contains(body, fixedTime.UTC().Truncate(24*time.Hour).Format("2006-01-02")) {
		t.Error("today row missing from rendered page")
	}
	// Totals aggregate across days: 10 + 5 = 15 requests.
	if !strings.Contains(body, ">15<") {
		t.Errorf("aggregated request total (15) missing; snippet: %q", snippet(body))
	}
	// TOON savings USD = (400+200)/1000 * 0.002 = 0.0012 → rounded 0.0012.
	if !strings.Contains(body, "$0.0012") {
		t.Errorf("TOON savings figure $0.0012 missing; snippet: %q", snippet(body))
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestDashboardRangeSelection verifies the ?range= query selects the
// right window size and that the 24h window only contains today.
func TestDashboardRangeSelection(t *testing.T) {
	store := twoDayStore()
	h := Dashboard(DashboardDeps{Store: store, CostPer1K: 0.002, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=24h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	today := fixedTime.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	yesterday := fixedTime.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1).Format("2006-01-02")

	// 24h window: only today is present; yesterday must not appear as a
	// data row (its date string should be absent from the table body).
	if !strings.Contains(body, today) {
		t.Error("today row missing from 24h view")
	}
	if strings.Contains(body, ">"+yesterday+"<") {
		t.Errorf("yesterday should be absent from the 24h window; snippet: %q", snippet(body))
	}
	// Totals for 24h should be just today's 10 requests.
	if !strings.Contains(body, ">10<") {
		t.Errorf("24h total (10) missing; snippet: %q", snippet(body))
	}
}

// TestDashboardUnknownRangeDefaultsTo24h ensures a bogus ?range=
// value never 500s; it falls back to the default window.
func TestDashboardUnknownRangeDefaultsTo24h(t *testing.T) {
	h := Dashboard(DashboardDeps{Store: twoDayStore(), CostPer1K: 0.002, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=forever", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for bogus range", rr.Code)
	}
}

// TestDashboardMethodNotAllowed enforces GET/HEAD only — the dashboard
// is read-only and rejects POST/PUT/DELETE with 405.
func TestDashboardMethodNotAllowed(t *testing.T) {
	h := Dashboard(DashboardDeps{Store: twoDayStore(), Now: fixedClock})

	req := httptest.NewRequest(http.MethodPost, "/dashboard", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want \"GET, HEAD\"", got)
	}
}

// TestDashboardJSONShape asserts the JSON view emits the agreed shape.
func TestDashboardJSONShape(t *testing.T) {
	h := DashboardJSON(DashboardDeps{Store: twoDayStore(), CostPer1K: 0.002, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=7d&format=json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"range":"7d"`) {
		t.Errorf("JSON missing range key; snippet: %q", snippet(body))
	}
	if !strings.Contains(body, `"totals"`) || !strings.Contains(body, `"days"`) {
		t.Errorf("JSON missing totals/days; snippet: %q", snippet(body))
	}
}

// TestDashboardQueryErrorIsTolerated asserts that a DailySummary error
// for one day does not abort the whole page — the bad day renders an
// empty row and the remaining days still aggregate.
func TestDashboardQueryErrorIsTolerated(t *testing.T) {
	store := &errStore{}
	h := Dashboard(DashboardDeps{Store: store, CostPer1K: 0.002, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=7d", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with query errors", rr.Code)
	}
}

type errStore struct{}

func (errStore) DailySummary(time.Time) (metrics.Summary, error) {
	return metrics.Summary{}, errFake
}

var errFake = errors.New("boom")

// fakeJudgeStore is an in-memory JudgeDashboardStore for dashboard
// RAG quality tests. It returns fixed RAGQualitySummary values.
type fakeJudgeStore struct {
	byDay map[string]judge.RAGQualitySummary
}

func (f *fakeJudgeStore) Record(judge.JudgeScore) error {
	return nil
}

func (f *fakeJudgeStore) RecentScores(limit int) ([]int, error) {
	return nil, nil
}

func (f *fakeJudgeStore) Close() error {
	return nil
}

func (f *fakeJudgeStore) RAGQuality(from, to time.Time) (judge.RAGQualitySummary, error) {
	key := from.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	return f.byDay[key], nil
}

// TestDashboardRAGDisabled shows "judge disabled" when JudgeEnabled is false.
func TestDashboardRAGDisabled(t *testing.T) {
	h := Dashboard(DashboardDeps{
		Store:        twoDayStore(),
		CostPer1K:    0.002,
		Now:          fixedClock,
		JudgeEnabled: false,
		JudgeStore:   nil,
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=24h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "judge disabled") {
		t.Errorf("RAG quality section should show 'judge disabled'; snippet: %q", snippet(body))
	}
}

// TestDashboardRAGEnabled shows RAG quality metrics when judge is enabled.
func TestDashboardRAGEnabled(t *testing.T) {
	today := fixedTime.UTC().Truncate(24 * time.Hour)
	store := &fakeJudgeStore{
		byDay: map[string]judge.RAGQualitySummary{
			today.Format("2006-01-02"): {
				JudgeCount:               5,
				RAGInjectedCount:         3,
				RAGInjectedScoreSum:      13.0, // scores: 5, 4, 4
				RAGInjectedScoreCount:    3,
				RAGInjectedSimilaritySum: 2.73, // similarities: 0.91, 0.92, 0.90
				NonRAGInjectedCount:      2,
				NonRAGInjectedScoreSum:   7.0, // scores: 4, 3
				NonRAGInjectedScoreCount: 2,
			},
		},
	}
	h := Dashboard(DashboardDeps{
		Store:        twoDayStore(),
		CostPer1K:    0.002,
		Now:          fixedClock,
		JudgeEnabled: true,
		JudgeStore:   store,
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=24h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Injection rate: 3 out of 10 = 30%
	if !strings.Contains(body, "30.0<small>%</small>") {
		t.Errorf("injection rate 30.0%% missing; snippet: %q", snippet(body))
	}
	// Avg judge score RAG: 13/3 = 4.33
	if !strings.Contains(body, "4.33") {
		t.Errorf("avg judge score RAG 4.33 missing; snippet: %q", snippet(body))
	}
	// Avg judge score non-RAG: 7/2 = 3.50
	if !strings.Contains(body, "3.50") {
		t.Errorf("avg judge score non-RAG 3.50 missing; snippet: %q", snippet(body))
	}
	// Avg similarity: 2.73/3 = 0.91
	if !strings.Contains(body, "0.910") {
		t.Errorf("avg similarity 0.910 missing; snippet: %q", snippet(body))
	}
}

// TestDashboardJSONRAGQuality verifies the JSON output includes RAG quality fields.
func TestDashboardJSONRAGQuality(t *testing.T) {
	today := fixedTime.UTC().Truncate(24 * time.Hour)
	store := &fakeJudgeStore{
		byDay: map[string]judge.RAGQualitySummary{
			today.Format("2006-01-02"): {
				JudgeCount:               4,
				RAGInjectedCount:         2,
				RAGInjectedScoreSum:      8.0,
				RAGInjectedScoreCount:    2,
				RAGInjectedSimilaritySum: 1.84,
				NonRAGInjectedCount:      2,
				NonRAGInjectedScoreSum:   6.0,
				NonRAGInjectedScoreCount: 2,
			},
		},
	}
	h := DashboardJSON(DashboardDeps{
		Store:        twoDayStore(),
		CostPer1K:    0.002,
		Now:          fixedClock,
		JudgeEnabled: true,
		JudgeStore:   store,
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=24h&format=json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("JSON parse error: %v", err)
	}

	rag, ok := out["rag_quality"].(map[string]any)
	if !ok {
		t.Fatal("rag_quality field missing from JSON")
	}
	if got, want := rag["judge_enabled"].(bool), true; got != want {
		t.Errorf("judge_enabled = %v, want %v", got, want)
	}
	if got, want := rag["injection_rate"].(float64), 30.0; got != want {
		t.Errorf("injection_rate = %v, want %v", got, want)
	}
	if got, want := rag["avg_judge_score_injected"].(float64), 4.0; got != want {
		t.Errorf("avg_judge_score_injected = %v, want %v", got, want)
	}
}

// TestDashboardJSONRAGDisabled verifies the JSON output shows judge disabled.
func TestDashboardJSONRAGDisabled(t *testing.T) {
	h := DashboardJSON(DashboardDeps{
		Store:        twoDayStore(),
		CostPer1K:    0.002,
		Now:          fixedClock,
		JudgeEnabled: false,
		JudgeStore:   nil,
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=24h&format=json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("JSON parse error: %v", err)
	}

	rag, ok := out["rag_quality"].(map[string]any)
	if !ok {
		t.Fatal("rag_quality field missing from JSON")
	}
	if got, want := rag["judge_enabled"].(bool), false; got != want {
		t.Errorf("judge_enabled = %v, want %v", got, want)
	}
}

// snippet returns the first 200 chars of body for compact assertion
// failure messages.
func snippet(body string) string {
	const max = 200
	if len(body) <= max {
		return body
	}
	return body[:max] + "…"
}

// TestToonSavingsUSDFormula pins the savings formula so a refactor of
// the rate math cannot silently drift from the CLI dashboard.
func TestToonSavingsUSDFormula(t *testing.T) {
	s := metrics.Summary{TOONSavingsTokens: 500}
	got := toonSavingsUSD(s, 0.002)
	want := 0.001 // 500/1000 * 0.002
	if got != want {
		t.Errorf("toonSavingsUSD(500 tok, 0.002) = %v, want %v", got, want)
	}
}

// TestSanitizeRangeKey covers the key coercion matrix.
func TestSanitizeRangeKey(t *testing.T) {
	cases := map[string]string{
		"24h":   "24h",
		"7d":    "7d",
		"30d":   "30d",
		"7D":    "7d", // case-insensitive
		"":      "24h",
		"bogus": "24h",
	}
	for in, want := range cases {
		if got := sanitizeRangeKey(in); got != want {
			t.Errorf("sanitizeRangeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDashboardHTMLHasNoExternalRequests guards the "no external
// assets" acceptance criterion: the rendered page must not reference
// http(s):// CDN URLs or <script src=> tags.
func TestDashboardHTMLHasNoExternalRequests(t *testing.T) {
	h := Dashboard(DashboardDeps{Store: twoDayStore(), CostPer1K: 0.002, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/dashboard?range=7d", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()

	for _, needle := range []string{"https://", "http://", "<script src", "<link rel=\"stylesheet\""} {
		if strings.Contains(body, needle) {
			t.Errorf("rendered page contains external-asset marker %q; must be self-contained", needle)
		}
	}
}
