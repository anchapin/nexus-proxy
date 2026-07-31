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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// defaultBufferSize is the default write buffer before forced flush.
const defaultBufferSize = 64 << 10 // 64 KiB

// defaultFlushInterval is the default time-based flush interval.
const defaultFlushInterval = 5 * time.Second

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
// (e.g. "vram" for the guardrail path, the error String for the
// SLM-error path); SLMConfidence is the [0,1] confidence value (0.5
// neutral, 0 for non-SLM sources); SLMTaskType is the Categorize()
// bucket (empty for non-SLM sources).
//
// Per-stage timing fields (issue #300) track wall-clock milliseconds
// spent in each pipeline stage. Omitempty keeps legacy JSONL rows
// byte-for-byte compatible when these fields are zero-valued.
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
	FusionArbiterCostUSD float64 `json:"fusion_arbiter_cost_usd,omitempty"`
	ToolCallCount        int     `json:"tool_call_count,omitempty"`
	Error                string  `json:"error,omitempty"`

	// Route-source metadata (issue #74). Omitempty keeps legacy
	// JSONL rows byte-for-byte compatible when these fields are
	// zero-valued.
	RouteSource   string  `json:"route_source,omitempty"`
	RouteReason   string  `json:"route_reason,omitempty"`
	SLMConfidence float64 `json:"slm_confidence,omitempty"`
	SLMTaskType   string  `json:"slm_task_type,omitempty"`

	// Per-stage latency breakdown (issue #300). All fields are
	// integer milliseconds; 0 when the stage was skipped or
	// not applicable.
	RAGRetrievalMs      int64 `json:"rag_retrieval_ms,omitempty"`
	PromptEngineeringMs int64 `json:"prompt_engineering_ms,omitempty"`
	TOONCompressionMs   int64 `json:"toon_compression_ms,omitempty"`
	SLMRoutingMs        int64 `json:"slm_routing_ms,omitempty"`
	UpstreamFirstByteMs int64 `json:"upstream_first_byte_ms,omitempty"`
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
	Sync()
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

// Dropped returns 0. Noop never drops records.
func (Noop) Dropped() uint64 { return 0 }

// Rotations returns 0. Noop never rotates.
func (Noop) Rotations() uint64 { return 0 }

// WriteErrors returns 0. Noop never encounters write errors.
func (Noop) WriteErrors() uint64 { return 0 }

// Sync is a no-op for Noop.
func (Noop) Sync() {}

// JSONLRecorder appends one JSON object per line to a file. The file is
// opened in append mode and the parent directory is created on demand.
//
// Size-based rotation (issue #485): when maxBytes > 0 the active file is
// atomically renamed to path.<unix-nanoseconds> the moment the next
// record would push it past the cap, and a fresh file is opened. The
// oldest rotated file is evicted once the rotated-file count exceeds
// maxFiles. Only the background run goroutine touches the file handle,
// so the rename-and-reopen path is race-free against concurrent Record
// callers (which only push onto the buffered channel). maxBytes == 0
// preserves the pre-#485 append-only behaviour.
//
// Batch writes (issue #681): records are accumulated in a memory buffer
// and flushed to disk when the buffer reaches bufferSize bytes or when
// flushInterval has elapsed since the last flush. This amortises disk I/O
// over many records rather than writing each record individually. The
// Sync() method forces an immediate flush; Close() flushes any remaining
// buffered records before exiting.
type JSONLRecorder struct {
	ch            chan Record
	path          string
	file          *os.File
	bw            *bufio.Writer
	wg            sync.WaitGroup
	dropped       atomic.Uint64
	writeErrors   atomic.Uint64
	closed        atomic.Bool
	done          chan struct{} // closed by run() on exit
	maxBytes      int64         // 0 = rotation disabled (append-only)
	maxFiles      int           // rotated-file cap (only when maxBytes > 0)
	written       int64         // bytes committed to the active file via bw
	rotations     atomic.Uint64
	syncCh        chan struct{} // signals immediate flush
	flushInterval time.Duration // time-based flush trigger
	bufferSize    int           // size-based flush threshold
}

// NewJSONLRecorder opens path (creating the parent directory if needed)
// and starts the background goroutine that drains the buffer.
//
// maxBytes enables size-based rotation when > 0: the active file is
// rotated (renamed and replaced) once the next record would exceed the
// cap. maxFiles bounds the number of rotated files retained; it is
// clamped to a minimum of 1 and only consulted when maxBytes > 0. Pass
// maxBytes 0 to select the legacy append-only, never-rotate behaviour.
//
// bufferSize is the maximum bytes to accumulate before flushing to disk.
// Defaults to 64 KiB when 0. A single record larger than bufferSize is
// flushed immediately regardless.
//
// flushInterval is the maximum time between flushes. Defaults to 5s when
// 0. A tick fires at this interval and triggers a flush if there is any
// buffered data.
func NewJSONLRecorder(path string, maxBytes int64, maxFiles int, bufferSize int, flushInterval time.Duration) (*JSONLRecorder, error) {
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
	// Seed the byte counter with the existing file size so a restart
	// against an already-oversized file rotates on the first record
	// rather than silently continuing to append past the cap.
	var initial int64
	if info, statErr := os.Stat(path); statErr == nil {
		initial = info.Size()
	}
	if maxBytes > 0 && maxFiles < 1 {
		maxFiles = 1
	}
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}
	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}
	r := &JSONLRecorder{
		ch:            make(chan Record, bufferedChannelSize),
		path:          path,
		file:          f,
		bw:            bufio.NewWriterSize(f, writeBufferSize),
		done:          make(chan struct{}),
		maxBytes:      maxBytes,
		maxFiles:      maxFiles,
		written:       initial,
		syncCh:        make(chan struct{}, 1),
		flushInterval: flushInterval,
		bufferSize:    bufferSize,
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

// Rotations returns the number of times the active file has been rotated
// because it crossed the size cap (issue #485). Surfaced to operators via
// the nexus_telemetry_rotations_total Prometheus counter. Always 0 when
// rotation is disabled (maxBytes == 0).
func (r *JSONLRecorder) Rotations() uint64 { return r.rotations.Load() }

// WriteErrors returns the number of records dropped because a disk write
// or flush error occurred in the background loop (issue #795). Distinct
// from Dropped(), which counts buffer-full rejections in the request path.
func (r *JSONLRecorder) WriteErrors() uint64 { return r.writeErrors.Load() }

// Sync signals the background goroutine to flush any buffered writes to
// disk immediately. It is called by callers who need guaranteed persistence
// (e.g. during graceful shutdown). It is a no-op when the recorder is
// already closed.
func (r *JSONLRecorder) Sync() {
	if r.closed.Load() {
		return
	}
	select {
	case r.syncCh <- struct{}{}:
	default:
		// sync channel already has a pending signal; the background
		// loop will flush on the next iteration anyway
	}
}

// run is the background consumer. It exits cleanly when Close signals
// shutdown; queued records are drained before the file is closed. All
// file-handle mutation (writes, flush, close, rotation) happens here, so
// the rename-and-reopen path is race-free against concurrent Record
// callers, which only push onto the buffered channel.
func (r *JSONLRecorder) run() {
	defer r.wg.Done()
	defer close(r.done)

	var buf []byte // accumulated serialized records (each line ends with \n)

	// flush writes buf to disk and resets it. Called when buffer is full,
	// on tick, on sync signal, and on channel close.
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if _, err := r.bw.Write(buf); err != nil {
			slog.Error("telemetry write",
				slog.String("path", r.path),
				slog.Any("err", err),
			)
			// On write error, drop all buffered records.
			droppedCount := 0
			for i := 0; i < len(buf); i++ {
				if buf[i] == '\n' {
					droppedCount++
				}
			}
			r.dropped.Add(uint64(droppedCount))
			r.writeErrors.Add(1) // issue #795: distinguish write errors from buffer-full drops
			slog.Warn("telemetry: dropped records due to write failure",
				slog.Int("count", droppedCount),
				slog.String("path", r.path),
			)
			buf = nil
			// Try to recover by rotating the file.
			if r.maxBytes > 0 {
				if rotErr := r.rotate(); rotErr != nil {
					slog.Error("telemetry rotate after write failure",
						slog.String("path", r.path),
						slog.Any("err", rotErr),
					)
				}
			}
			return
		}
		if err := r.bw.Flush(); err != nil {
			slog.Error("telemetry flush",
				slog.String("path", r.path),
				slog.Any("err", err),
			)
			// On flush failure, try to recover via rotation.
			r.writeErrors.Add(1) // issue #795: distinguish write errors from buffer-full drops
			if r.maxBytes > 0 {
				if rotErr := r.rotate(); rotErr != nil {
					slog.Error("telemetry rotate after flush failure",
						slog.String("path", r.path),
						slog.Any("err", rotErr),
					)
				}
			}
			buf = nil
			return
		}
		r.written += int64(len(buf))
		buf = nil
	}

	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			flush()
		case <-r.syncCh:
			flush()
		case rec, ok := <-r.ch:
			if !ok {
				// Channel closed; drain remaining buffered records and exit.
				flush()
				if r.bw != nil {
					if err := r.bw.Flush(); err != nil {
						slog.Error("telemetry final flush",
							slog.String("path", r.path),
							slog.Any("err", err),
						)
					}
				}
				if r.file != nil {
					if err := r.file.Close(); err != nil {
						slog.Error("telemetry close",
							slog.String("path", r.path),
							slog.Any("err", err),
						)
					}
				}
				return
			}
			b, err := json.Marshal(rec)
			if err != nil {
				slog.Error("telemetry marshal",
					slog.String("path", r.path),
					slog.Any("err", err),
				)
				continue
			}
			lineLen := int64(len(b)) + 1 // +1 for trailing newline

			// Rotate before writing when the next line would cross the cap.
			// The written > 0 guard guarantees every file (including one
			// opened against a pre-existing oversized file) receives at
			// least one record, avoiding pathological empty-file rotations
			// when a single record is larger than maxBytes.
			// We must account for buffered bytes (len(buf)) since they
			// will be written together with this record.
			if r.maxBytes > 0 && r.written > 0 && r.written+int64(len(buf))+lineLen > r.maxBytes {
				flush()
				if rotErr := r.rotate(); rotErr != nil {
					slog.Error("telemetry rotate",
						slog.String("path", r.path),
						slog.Any("err", rotErr),
					)
				}
			}

			// If a single record exceeds the buffer size, flush immediately
			// before adding it.
			if int64(len(buf))+lineLen > int64(r.bufferSize) {
				flush()
			}

			// Accumulate the record into buf.
			buf = append(buf, b...)
			buf = append(buf, '\n')

			// Flush when buffer reaches the threshold.
			if len(buf) >= r.bufferSize {
				flush()
			}
		}
	}
}

// rotate atomically moves the active file aside and opens a fresh one.
// It flushes and closes the current writer, renames path to
// path.<unix-nanoseconds>, evicts surplus rotated files, then reopens a
// new append handle. Called only from run(), so no locking is required.
// On rename failure the original file is reopened in place so writes can
// continue (the error is returned for logging); on successful rename but
// reopen failure the recorder is left with a stale (closed) writer whose
// subsequent Writes surface as logged errors rather than panics.
func (r *JSONLRecorder) rotate() error {
	if r.bw != nil {
		if err := r.bw.Flush(); err != nil && !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("flush: %w", err)
		}
	}
	if r.file != nil {
		if err := r.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("close: %w", err)
		}
	}
	rotated := fmt.Sprintf("%s.%d", r.path, time.Now().UnixNano())
	if err := os.Rename(r.path, rotated); err != nil {
		// Best-effort recovery: reopen the original path so the next
		// record has somewhere to go instead of panicking on a nil bw.
		if f, reopenErr := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); reopenErr == nil {
			r.file = f
			r.bw = bufio.NewWriterSize(f, writeBufferSize)
		}
		return fmt.Errorf("rename %q -> %q: %w", r.path, rotated, err)
	}
	r.rotations.Add(1)
	r.evictExcessRotatedFiles()
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen %q: %w", r.path, err)
	}
	chmodIfWider(r.path, 0o600)
	r.file = f
	r.bw = bufio.NewWriterSize(f, writeBufferSize)
	r.written = 0
	return nil
}

// evictExcessRotatedFiles removes the oldest rotated files (by mtime)
// until at most maxFiles remain. Rotated files are identified by the
// path.<suffix> naming produced by rotate. Failures are logged and
// skipped; a Stat error on one file does not abort the sweep.
func (r *JSONLRecorder) evictExcessRotatedFiles() {
	if r.maxFiles < 1 {
		return
	}
	prefix := r.path + "."
	matches, err := filepath.Glob(prefix + "*")
	if err != nil || len(matches) <= r.maxFiles {
		return
	}
	sort.Slice(matches, func(i, j int) bool {
		mi, errI := os.Stat(matches[i])
		mj, errJ := os.Stat(matches[j])
		if errI != nil || errJ != nil {
			return matches[i] < matches[j]
		}
		return mi.ModTime().Before(mj.ModTime())
	})
	for len(matches) > r.maxFiles {
		oldest := matches[0]
		if rmErr := os.Remove(oldest); rmErr != nil {
			slog.Warn("telemetry: evict rotated file",
				slog.String("path", oldest),
				slog.Any("err", rmErr),
			)
		}
		matches = matches[1:]
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
	o := &ObservingWriter{ResponseWriter: inner, hook: hook}
	o.status.Store(-1)
	return o
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
