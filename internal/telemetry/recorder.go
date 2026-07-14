// Package telemetry captures per-request performance metrics for the proxy.
//
// The chat handler emits one Record per request after the upstream response
// completes. Records are pushed onto a buffered channel consumed by a
// background goroutine so the request path never blocks on persistence.
// If the buffer fills (disk stall or fatal write error) the overflow is
// dropped with a log warning rather than stalling user requests.
//
// The Recorder interface is deliberately minimal so the production store
// can be swapped from this v0 JSON-lines append-only file for a SQLite
// backend (tracked in issue #4) without touching callers.
package telemetry

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/tokenizer"
)

// bufferedChannelSize caps the in-flight queue per recorder. Each record
// serialises to ~1 KB of JSON so this is ~1 MB worst-case memory and is
// more than enough to absorb request bursts without ever blocking callers.
const bufferedChannelSize = 1024

// writeBufferSize is the size of the bufio.Writer used inside JSONLRecorder.
// Larger than a single record so most rows flush via the background loop's
// explicit Flush call rather than the bufio auto-flush threshold.
const writeBufferSize = 16 << 10

// Record is the row written for every proxied request.
//
// TTFTMs is integer milliseconds (0 for non-streaming responses — TTFT
// is undefined when the harness requested a single buffered reply).
//
// TotalLatencyMs is FLOAT64 milliseconds (issue #68). Sub-millisecond
// precision matters: on fast hardware the cast to int64 truncates a
// few-microsecond handler run to 0, which trips assertions that the
// latency was recorded and surfaces as a race-detector-dependent
// flake. Storing the value as float64 captures the true elapsed time
// regardless of rounding for display.
//
// FusionArbiterSkipped (issue #48) is true only for route=fusion
// requests that streamed the speculative panel-member answer and
// terminated without invoking the arbiter. False in every other
// case — including the legacy (non-progressive) Panel path, where
// the arbiter is always invoked. The dashboard joins on this flag
// to report "fraction of fusion traffic that achieved agreement".
//
// FusionJaccardSimilarity (issue #200) is the actual Jaccard ratio
// between the two panel members' contents when both returned content.
// 0 when fewer than two members returned content. Enables operators
// to tune NEXUS_FUSION_AGREEMENT_THRESHOLD based on actual distribution.
//
// Route-source fields (issue #74) carry the planner's Decision
// metadata so downstream consumers (JSONL log, SQLite metrics,
// dashboard) can attribute each request to the stage that produced
// the route. RouteSource is one of guardrail / dsl / slm / slm-error
// / escalation; RouteReason is a short machine-readable detail
// (e.g. "vram" for the guardrail path, the error string for the
// SLM-error path); SLMConfidence is the [0,1] confidence value (0.5
// neutral, 0 for non-SLM sources); SLMTaskType is the Categorize()
// bucket (empty for non-SLM sources).
type Record struct {
	Timestamp               time.Time `json:"timestamp"`
	RequestID               string    `json:"request_id"`
	Model                   string    `json:"model"`
	Route                   string    `json:"route"`
	InputTokens             int       `json:"input_tokens"`
	OutputTokens            int       `json:"output_tokens"`
	TTFTMs                  int64     `json:"ttft_ms"`
	TotalLatencyMs          float64   `json:"total_latency_ms"`
	TPS                     float64   `json:"tps"`
	Streaming               bool      `json:"streaming"`
	FusionArbiterSkipped    bool      `json:"fusion_arbiter_skipped,omitempty"`
	FusionJaccardSimilarity float64   `json:"fusion_jaccard_similarity,omitempty"`
	// FusionArbiterCostUSD (issue #239) is the estimated cost of the
	// arbiter call when it ran (route=fusion and agreement threshold
	// not met). 0 when the arbiter was skipped or for non-fusion routes.
	FusionArbiterCostUSD    float64   `json:"fusion_arbiter_cost_usd,omitempty"`
	ToolCallCount           int       `json:"tool_call_count,omitempty"`
	Error                   string    `json:"error,omitempty"`

	// Route-source metadata (issue #74). Omitempty keeps legacy
	// JSONL rows byte-for-byte compatible when these fields are
	// zero-valued.
	RouteSource   string  `json:"route_source,omitempty"`
	RouteReason   string  `json:"route_reason,omitempty"`
	SLMConfidence float64 `json:"slm_confidence,omitempty"`
	SLMTaskType   string  `json:"slm_task_type,omitempty"`
}

// EstimateTokens returns the cheap "4 chars per token" heuristic used
// across the proxy (router VRAM guardrail, telemetry input). Centralising
// the rule here keeps the two call sites consistent.
func EstimateTokens(s string) int {
	return tokenizer.CountTokens(s)
}

// ComputeTPS derives tokens-per-second from output tokens and the generation
// phase (total latency minus time-to-first-token). Returns 0 when the
// generation window is non-positive or no tokens were produced. Units are
// tokens / second.
//
// totalMs is float64 milliseconds (see Record.TotalLatencyMs); ttftMs is
// integer milliseconds. Mixing the two is deliberate — TTFT rounds to a
// coarse integer because the first-byte hook fires on a Write call and
// the slack is well above 1 ms — while totalMs keeps sub-ms precision to
// avoid the issue #68 flake.
func ComputeTPS(outputTokens int, ttftMs int64, totalMs float64) float64 {
	if outputTokens <= 0 || totalMs <= 0 {
		return 0
	}
	genMs := totalMs - float64(ttftMs)
	if genMs <= 0 {
		return 0
	}
	return float64(outputTokens) * 1000.0 / genMs
}

// Recorder persists records. Implementations MUST NOT block Record callers;
// they should drop, buffer, or shed load and never stall the response path.
type Recorder interface {
	Record(r Record)
	Close() error
}

// Compile-time interface compliance checks.
var (
	_ Recorder = Noop{}
	_ Recorder = (*JSONLRecorder)(nil)
)

// Noop discards every record. Useful for tests and when persistence is
// disabled by configuration (NEXUS_TELEMETRY_PATH="").
type Noop struct{}

// Record implements Recorder.
func (Noop) Record(Record) {}

// Close implements Recorder.
func (Noop) Close() error { return nil }

// JSONLRecorder appends one JSON object per line to a file. The file is
// opened in append mode and the parent directory is created on demand.
type JSONLRecorder struct {
	ch      chan Record
	path    string
	file    *os.File
	bw      *bufio.Writer
	wg      sync.WaitGroup
	dropped atomic.Uint64
	closed  atomic.Bool
	done    chan struct{} // closed by run() on exit
}

// NewJSONLRecorder opens path (creating the parent directory if needed)
// and starts the background goroutine that drains the buffer.
func NewJSONLRecorder(path string) (*JSONLRecorder, error) {
	if path == "" {
		return nil, fmt.Errorf("telemetry: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("telemetry: mkdir %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open %q: %w", path, err)
	}
	// Tighten permissions on an existing file so an upgrade from a
	// pre-fix binary locks down the log (issue #108).
	chmodIfWider(path, 0o600)
	r := &JSONLRecorder{
		ch:   make(chan Record, bufferedChannelSize),
		path: path,
		file: f,
		bw:   bufio.NewWriterSize(f, writeBufferSize),
		done: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run()
	return r, nil
}

// Path returns the on-disk path this recorder writes to.
func (r *JSONLRecorder) Path() string { return r.path }

// Dropped returns the number of records dropped because the buffer was
// full. Tests assert on this to verify the non-blocking contract.
func (r *JSONLRecorder) Dropped() uint64 { return r.dropped.Load() }

// run is the background consumer. It exits cleanly when Close signals
// shutdown; queued records are drained before the file is closed.
func (r *JSONLRecorder) run() {
	defer r.wg.Done()
	defer close(r.done)
	for rec := range r.ch {
		if err := writeJSONLine(r.bw, rec); err != nil {
			slog.Error("telemetry write",
				slog.String("path", r.path),
				slog.Any("err", err),
			)
		}
	}
	if err := r.bw.Flush(); err != nil {
		slog.Error("telemetry flush",
			slog.String("path", r.path),
			slog.Any("err", err),
		)
	}
	if err := r.file.Close(); err != nil {
		slog.Error("telemetry close",
			slog.String("path", r.path),
			slog.Any("err", err),
		)
	}
}

// Record enqueues rec for asynchronous persistence. If the buffer is full
// (consumer stalled on I/O) the record is dropped, the dropped counter is
// incremented, and a warning is logged. The call never blocks.
func (r *JSONLRecorder) Record(rec Record) {
	if r.closed.Load() {
		return
	}
	select {
	case r.ch <- rec:
	default:
		r.dropped.Add(1)
		slog.Warn("telemetry buffer full, dropped record",
			slog.String("request_id", rec.RequestID),
			slog.String("route", rec.Route),
		)
	}
}

// Close signals the background goroutine to drain and exit. Safe to call
// once; subsequent calls are no-ops. Blocks until the goroutine returns,
// guaranteeing all queued records reach disk.
func (r *JSONLRecorder) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(r.ch)
	r.wg.Wait()
	return nil
}

// chmodIfWider checks the current mode of path and, if any
// group/other bits are set, tightens the file to the requested mode.
// Errors are logged — chmod failures are non-fatal (the file was
// already created with the restrictive mode by OpenFile).
func chmodIfWider(path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("telemetry: stat for chmod",
			slog.String("path", path),
			slog.Any("err", err),
		)
		return
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return // already owner-only
	}
	slog.Warn("telemetry: tightening file permissions",
		slog.String("path", path),
		slog.Int("was", int(perm)),
		slog.Int("now", int(mode)),
	)
	if err := os.Chmod(path, mode); err != nil {
		slog.Warn("telemetry: chmod failed",
			slog.String("path", path),
			slog.Any("err", err),
		)
	}
}

func writeJSONLine(w *bufio.Writer, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return nil
}

// NewRequestID returns a 16-hex-char identifier unique enough for log
// correlation. Stdlib-only — avoids pulling in a UUID dependency just for
// telemetry tags.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// WriteHook fires once with the wall-clock time of the first Write call
// into the wrapped ResponseWriter. Used by the handler to measure TTFT.
type WriteHook func(time.Time)

// ObservingWriter wraps an http.ResponseWriter, fires hook on the first
// Write, and counts total bytes written. Header() and WriteHeader() pass
// through; Flush() forwards to the inner writer when it implements
// http.Flusher (so Stream's flusher assertion still succeeds on real
// writers but degrades to a no-op on non-flushers like httptest.Recorder).
//
// The wrapper also captures the response status code (issue #33) so the
// debug trace can report it without an extra wrapper. WriteHeader is
// idempotent: Go's net/http panics on a second WriteHeader, so we just
// record the first one we see and ignore the rest.
type ObservingWriter struct {
	http.ResponseWriter
	hook     WriteHook
	wrote    atomic.Bool
	bytesOut atomic.Uint64
	status   atomic.Int64 // -1 until WriteHeader fires; 0 = 200 (Go default)
}

// NewObservingWriter wraps inner. hook may be nil; if so, only byte counts
// are tracked.
func NewObservingWriter(inner http.ResponseWriter, hook WriteHook) *ObservingWriter {
	return &ObservingWriter{ResponseWriter: inner, hook: hook, status: atomic.Int64{}}
}

// Write fires the first-write hook (if not yet fired) and updates the
// byte counter before delegating to the inner writer.
func (o *ObservingWriter) Write(b []byte) (int, error) {
	if o.wrote.CompareAndSwap(false, true) && o.hook != nil {
		o.hook(time.Now())
	}
	o.bytesOut.Add(uint64(len(b)))
	return o.ResponseWriter.Write(b)
}

// WriteHeader records the status code and forwards to the inner writer.
// Idempotent against multiple calls: Go's net/http panics on the second
// WriteHeader, but recording is still racy-free thanks to atomic.Int64.
func (o *ObservingWriter) WriteHeader(s int) {
	o.status.CompareAndSwap(-1, int64(s))
	o.ResponseWriter.WriteHeader(s)
}

// BytesOut returns the cumulative number of bytes written to the underlying
// ResponseWriter.
func (o *ObservingWriter) BytesOut() uint64 { return o.bytesOut.Load() }

// StatusCode returns the status code the wrapped writer committed, or
// 200 when WriteHeader was never called (the Go default). Issue #33:
// the debug trace uses this to surface the upstream HTTP status without
// a second wrapper layer.
func (o *ObservingWriter) StatusCode() int {
	s := o.status.Load()
	if s < 0 {
		return http.StatusOK
	}
	return int(s)
}

// Flush forwards to the inner writer when it implements http.Flusher.
// Defining Flush here (rather than relying on embedding) lets Stream's
// `w.(http.Flusher)` assertion succeed against the wrapper; the inner
// no-op behaviour preserves the existing "non-flusher errors" test for
// direct Stream callers.
func (o *ObservingWriter) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
