// Package metrics persists per-request routing and savings metrics into a
// local SQLite database (issue #4). The metrics store replaces the v0
// JSON-lines append-only log (issue #16 / internal/telemetry) as the
// canonical persistence layer for the savings dashboard.
//
// # Design
//
// The chat handler emits one RecordRequest per completed request after
// the upstream response flushes. Writes are dispatched onto a buffered
// channel consumed by a single background goroutine — the request path
// never blocks on SQLite I/O. Records that miss the buffer (disk stall or
// open transaction) are dropped with a counter exposed via Store.Dropped
// so an operator can spot saturation without log-spamming.
//
// SQLiteStore also satisfies the telemetry.Recorder interface, so the
// existing chat handler can adopt it as a drop-in replacement for
// telemetry.JSONLRecorder without any handler-side changes. In that
// configuration the new Request fields (TOON savings, RAG injection, cost)
// are populated from a metrics-aware path in the handler (see
// handlers.MetricsObserver). A telemetry-only request lands with those
// fields defaulting to zero so the schema stays backwards compatible.
//
// # Schema
//
// A single CREATE TABLE IF NOT EXISTS on Open — no migration framework.
// Future schema versions must remain additive (new columns, never drop /
// rename) to keep the "Open, migrate, keep writing" hot path trivial.
package metrics

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// bufferedChannelSize caps the in-flight queue per store. Records
// serialise to one INSERT each, so this caps memory at roughly
// (record-row-bytes * bufferedChannelSize). 1024 records is ~1 MB worst
// case — comfortable and large enough to absorb request bursts without
// blocking the response path.
const bufferedChannelSize = 1024

// RecordRequestErrorTimeout bounds how long a Write op waits for a
// slow disk before the Store reports the failure. Most inserts finish
// in microseconds against tmpfs; the timeout exists only to bound a
// pathological fsync stall.
const recordRequestErrorTimeout = 5 * time.Second

// Request is one row in the `requests` table. The chat handler builds
// this once per request, after the upstream response has been fully
// streamed, then hands it to Store.RecordRequest.
//
// Field choice rationale (issue #4):
//   - Timestamp / Route / Model — primary dashboard dimensions.
//   - InputTokens — what got sent to the upstream.
//   - TOONSavingsTokens — how many tokens proxy-side compression shaved
//     off before the request was sent (sum of character deltas / 4).
//   - RAGInjected / RAGFilename — whether RAG fired and which example
//     was injected (filename is empty when RAG missed).
//   - EstimatedCostUSD — frontier ($/1k token) * input tokens for
//     rough cost accounting. Zero for local-route requests.
//
// Telemetry-mirrored fields (OutputTokens, TTFTMs, TotalLatencyMs, TPS,
// Streaming, Error) carry the same shape as telemetry.Record so the
// SQLiteStore can satisfy both interfaces.
//
// TotalLatencyMs is float64 milliseconds (issue #68) — see
// telemetry.Record.TotalLatencyMs for the rationale.
//
// FusionArbiterSkipped (issue #48) mirrors telemetry.Record: true only
// for route=fusion requests that streamed the speculative panel-member
// answer and terminated without invoking the arbiter. False in every
// other case (legacy Panel path always invokes the arbiter; non-fusion
// routes are always false).
//
// FusionJaccardSimilarity (issue #200) mirrors telemetry.Record: the
// actual Jaccard ratio between the two panel members' contents when
// both returned content. 0 when fewer than two members returned content.
// Enables operators to tune NEXUS_FUSION_AGREEMENT_THRESHOLD based on
// actual distribution data.
//
// Route-source metadata (issue #74) mirrors telemetry.Record so the
// SQLite store and the JSONL recorder carry identical routing metadata
// for every request.
type Request struct {
	Timestamp         time.Time
	RequestID         string
	Route             string
	Model             string
	InputTokens       int
	TOONSavingsTokens int
	// TOONCompressionMethod records which TOON compression pattern was
	// applied to this request's messages (issue #247): "fenced" for
	// ```json [...] ``` blocks, "nested" for {"files": [...]} arrays,
	// or "" when no compression was applied.
	TOONCompressionMethod string
	RAGInjected           bool
	RAGFilename           string
	EstimatedCostUSD      float64

	// BaselineCostUSD is what this request would have cost if sent
	// to the configured baseline (frontier) provider at the baseline
	// rate, regardless of the actual route taken (issue #73).
	// SavingsUSD = max(BaselineCostUSD - EstimatedCostUSD, 0).
	BaselineCostUSD float64
	SavingsUSD      float64

	OutputTokens            int
	TTFTMs                  int64
	TotalLatencyMs          float64
	TPS                     float64
	Streaming               bool
	FusionArbiterSkipped    bool
	FusionJaccardSimilarity float64
	Error                   string

	// Route-source metadata (issue #74).
	RouteSource   string
	RouteReason   string
	SLMConfidence float64
	SLMTaskType   string
}

// Summary is the per-day roll-up returned by Store.DailySummary.
//
// The same struct is reused for arbitrary date ranges (the current
// implementation always collapses to one UTC day) so future monthly /
// weekly endpoints can return a populated list without breaking callers.
type Summary struct {
	Date               time.Time
	RequestCount       int
	LocalCount         int
	FrontierCount      int
	FusionCount        int
	TotalInputTokens   int
	TOONSavingsTokens  int
	RAGInjectedCount   int
	EstimatedCostTotal float64

	// BaselineCostTotal and SavingsTotal roll up the per-request
	// baseline_cost_usd and savings_usd columns (issue #73).
	// BaselineCostTotal is the total "would-have-cost at frontier"
	// figure; SavingsTotal is the sum of max(baseline - actual, 0)
	// across all requests in the period.
	BaselineCostTotal float64
	SavingsTotal      float64

	TotalLatencyMsSum float64
	ErrorCount        int
}

// Store persists per-request metrics. Implementations MUST return
// promptly from RecordRequest; chat-path latency must never depend on
// disk I/O timing. Buffered / async implementations are the expected
// shape — the only synchronous call is DailySummary, which the
// dashboard invokes explicitly.
type Store interface {
	RecordRequest(req Request) error
	DailySummary(date time.Time) (Summary, error)
	Close() error
}

// Logger is the seam Store implementations use to surface internal
// warnings (e.g. dropped writes, decode errors). Defaults to a slog
// bridge that preserves the printf-style ergonomics of the original
// stdlib log target; tests inject a silent / capture target (issue #3).
type Logger func(format string, args ...any)

// stdLogger is the package default — routes internal warnings through
// slog at WARN level. The bracketed prefix is dropped (issue #3) so a
// JSON operator sees plain structured attrs.
var stdLogger Logger = func(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

// Open creates a Store backed by a SQLite database at path. The parent
// directory is created on demand. An empty path is rejected; ":memory:"
// is allowed for tests.
func Open(path string) (Store, error) {
	return OpenWithLogger(path, stdLogger)
}

// OpenWithLogger is Open with a custom logger. Pass a no-op Logger to
// silence the package in tests; pass nil to use the default.
func OpenWithLogger(path string, lg Logger) (Store, error) {
	if path == "" {
		return nil, fmt.Errorf("metrics: empty path")
	}
	if lg == nil {
		lg = stdLogger
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("metrics: mkdir %q: %w", dir, err)
			}
		}
	}
	s, err := newSQLiteStore(path, lg)
	if err != nil {
		return nil, err
	}
	// Tighten permissions on the SQLite DB file so an upgrade from a
	// pre-fix binary locks it down (issue #108). The driver creates the
	// file; we chmod it after the first successful open.
	if path != ":memory:" {
		chmodIfWider(path, 0o600)
	}
	return s, nil
}

// DroppedCounter is implemented by Stores that can shed load under
// pressure. The dashboard uses it to surface buffer-saturation events.
type DroppedCounter interface {
	Dropped() uint64
}

// Compile-time guards: keep the sealed-shape door closed if the SQLite
// implementation grows.
var (
	_ Store          = (*SQLiteStore)(nil)
	_ DroppedCounter = (*SQLiteStore)(nil)
)

// closeOnce guards Close against accidental double-close from a
// mistakenly chained defer in main.go.
type closeOnce struct {
	sync.Once
	err error
}

func (c *closeOnce) Close(do func() error) error {
	c.Once.Do(func() { c.err = do() })
	return c.err
}

// atomicDropped is a tiny helper so tests can assert on drops.
type atomicDropped struct{ n atomic.Uint64 }

func (a *atomicDropped) add()        { a.n.Add(1) }
func (a *atomicDropped) get() uint64 { return a.n.Load() }

// chmodIfWider checks the current mode of path and, if any
// group/other bits are set, tightens the file to the requested mode.
// Errors are logged — chmod failures are non-fatal.
func chmodIfWider(path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("metrics: stat for chmod",
			slog.String("path", path),
			slog.Any("err", err),
		)
		return
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return
	}
	slog.Warn("metrics: tightening file permissions",
		slog.String("path", path),
		slog.Int("was", int(perm)),
		slog.Int("now", int(mode)),
	)
	if err := os.Chmod(path, mode); err != nil {
		slog.Warn("metrics: chmod failed",
			slog.String("path", path),
			slog.Any("err", err),
		)
	}
}
