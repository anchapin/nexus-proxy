// Package quality implements the asynchronous AST/compiler verifier
// described in issue #13.
//
// Design goals:
//
//   - The chat hot path (internal/handlers, internal/upstream) must NOT
//     import this package. Handlers expose a tiny event surface — the
//     QualityObserver hook on handlers.Deps — and the verifier plugs
//     into it from cmd/nexus/main.go. This keeps the hot path lean
//     and matches the dependency rule AGENTS.md states for the judge
//     evaluator (see internal/judge).
//
//   - Detected edits are enqueued, never blocking. The chat handler
//     invokes Submit (a non-blocking channel send) and immediately
//     returns; a bounded worker pool consumes the queue and runs
//     `cargo check` / `npx tsc` against the detected project root.
//
//   - Project detection walks up the edited file's directory looking
//     for Cargo.toml or tsconfig.json. The mapping is cached per
//     directory in a sync.Map so repeated edits in the same tree
//     avoid filesystem stat loops.
//
//   - Stdlib-only. The verifier uses os/exec directly; tests inject
//     deterministic shells via NEXUS_QUALITY_SHELL_OVERRIDE.
package quality

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind identifies which project marker was found during project
// detection. Unknown means the walk yielded no recognised marker; the
// verifier records this and scores 0.
type Kind string

const (
	KindUnknown Kind = ""
	KindRust    Kind = "Cargo.toml"
	KindTS      Kind = "tsconfig.json"
)

// droppedEventArgsCap bounds the size of Args stored in the dropped
// ring so that large tool payloads cannot cause unbounded memory growth.
// EncodeEvent also uses this limit when truncating args for log lines.
const droppedEventArgsCap = 200

// AllKinds is the ordered list of supported project markers. The
// verifier recognises the same set; tests and dashboards use this list
// to enumerate valid options.
var AllKinds = []Kind{KindRust, KindTS}

// ProjectMarker maps a Kind to the manifest filename the verifier
// checks the repo root for. Keep this in sync with the detection loop
// below.
func (k Kind) Marker() string {
	switch k {
	case KindRust:
		return "Cargo.toml"
	case KindTS:
		return "tsconfig.json"
	}
	return ""
}

// Event is the input that triggers one verification attempt. The
// handler emits one of these per detected edit; nothing outside the
// package needs to know how it is consumed.
type Event struct {
	RequestID string // correlates to the chat handler's request id
	Path      string // absolute (or cwd-relative) path of the edited file
	ToolName  string // write_file, edit_file, apply_patch, ...
	Args      []byte // raw arguments JSON (best-effort, may be empty)

	// TraceParent and TraceState carry the W3C trace context from the
	// inbound request so the async worker can create a child span (issue #233).
	TraceParent string
	TraceState  string
}

// Verdict is the structured result produced by one verification
// attempt. Pass is true iff the project check exited 0; anything else
// (non-zero exit, fork failure, timeout) yields Pass=false. Stderr is
// capped (cfg.StderrCap bytes, default 2 KiB) and truncated from the
// tail. Err is non-nil only for infrastructure failures (cannot fork,
// walk cancellation) — exit-code non-zero is reported via Pass without
// an Err.
type Verdict struct {
	Event      Event
	Pass       bool      // true iff shell exit code == 0
	ExitCode   int       // raw exit code; -1 on infrastructure failure
	Stderr     string    // truncated to cfg.StderrCap bytes from the tail
	DurationMs int64     // wall-clock time spent in the shell command
	RepoRoot   string    // detected project root; "" when no manifest found
	Kind       Kind      // detected project kind; "" when no manifest found
	Err        error     // infrastructure failure (fork / cancellation)
	Timestamp  time.Time // time the worker produced the verdict
}

// Observer consumes verdicts as the verifier produces them. The
// observer must be safe to call concurrently from many worker
// goroutines; the verifier does not serialise calls.
type Observer interface {
	Submit(Verdict)
}

// ObserverFunc adapts a plain function to the Observer interface so
// wiring from main.go stays a one-liner.
type ObserverFunc func(Verdict)

// Submit implements Observer.
func (f ObserverFunc) Submit(v Verdict) { f(v) }

// Config tunes a Verifier. Zero values get safe defaults applied by
// NewShellVerifier so callers can construct from a partial config
// without exploding.
type Config struct {
	Concurrency     int           // max parallel workers (default 2)
	QueueDepth      int           // buffered channel size (default 64)
	Timeout         time.Duration // per-check timeout (default 60s)
	StderrCap       int           // stderr bytes retained per verdict (default 2 KiB)
	Observer        Observer      // required at runtime; nil is replaced with a no-op
	DroppedRingSize int           // ring buffer capacity for dropped events (default 256)
	// Now is overridable for tests. Real callers leave it nil and the
	// verifier uses time.Now.
	Now func() time.Time
}

func (c *Config) applyDefaults() {
	// Concurrency is intentionally NOT defaulted: a non-positive
	// value means "dormant", and rewriting 0 to 2 would silently
	// re-enable a verifier the operator just disabled.
	if c.QueueDepth <= 0 {
		c.QueueDepth = 64
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.StderrCap <= 0 {
		c.StderrCap = 2 * 1024
	}
	if c.Observer == nil {
		c.Observer = ObserverFunc(func(Verdict) {})
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.DroppedRingSize <= 0 {
		c.DroppedRingSize = 256
	}
}

// Verifier detects project-bearing edits and runs the project's check
// command asynchronously with bounded concurrency. The interface is
// the seam the handler depends on; implementations may swap
// ShellVerifier for an in-process stub during tests.
type Verifier interface {
	// Enabled reports whether the verifier will actually run checks.
	Enabled() bool
	// Submit enqueues e for asynchronous verification. Non-blocking;
	// returns false on overflow / when disabled / after Close.
	Submit(e Event) bool
	// Concurrency returns the configured worker count.
	Concurrency() int
	// QueueDepth returns the current buffered count.
	QueueDepth() int
	// Dropped returns the number of events that overflowed the queue.
	Dropped() uint64
	// DroppedEvents returns the most recently dropped events.
	DroppedEvents() []Event
	// DroppedRingCapacity returns the maximum number of dropped events
	// retained in the ring buffer for inspection.
	DroppedRingCapacity() int
	// Close drains the queue and stops the worker pool.
	Close() error
}

// droppedRing is a lock-free ring buffer for recently dropped events.
// It stores up to c events in a fixed-size circular buffer.
type droppedRing struct {
	events []Event
	idx    atomic.Int64
	size   int
}

func newDroppedRing(size int) *droppedRing {
	return &droppedRing{
		events: make([]Event, size),
		size:   size,
	}
}

// Push records one dropped event into the ring buffer.
func (r *droppedRing) Push(e Event) {
	pos := r.idx.Add(1) - 1
	if len(e.Args) > droppedEventArgsCap {
		e.Args = e.Args[:droppedEventArgsCap]
	}
	r.events[pos%int64(r.size)] = e
}

// Events returns the recorded dropped events in chronological order,
// newest first. The slice is a copy and safe to retain.
func (r *droppedRing) Events() []Event {
	count := r.idx.Load()
	if count == 0 {
		return nil
	}
	size := int(count)
	if size > r.size {
		size = r.size
	}
	// start is the oldest index we need to return
	start := count - int64(size)
	result := make([]Event, size)
	for i := 0; i < size; i++ {
		result[i] = r.events[(start+int64(i))%int64(r.size)]
	}
	return result
}

// ShellVerifier is the production Verifier: it runs `cargo check`
// (for Cargo.toml repos) or `npx tsc --noEmit` (for tsconfig.json
// repos) inside the detected repo root, asynchronously, with bounded
// concurrency. NEXUS_QUALITY_SHELL_OVERRIDE replaces the command for
// unit-test injection.
type ShellVerifier struct {
	cfg   Config
	cache sync.Map // map[string]projectHint (key = absDir of source file)

	queue       chan Event
	wg          sync.WaitGroup
	closed      chan struct{}
	closeOnce   sync.Once
	dropped     atomic.Uint64 // queue overflow counter
	droppedRing *droppedRing
}

// projectHint caches the result of a directory walk: where the project
// root landed and which Kind of project it is. The (root == "") case
// is cached too — the same falsey answer avoids re-stat'ing a deep
// non-project tree for every edit.
//
// mtime records the modification time of the discovered manifest file
// at cache-prime time so we can detect when a manifest is created or
// deleted after the entry was cached (issue #1067).
type projectHint struct {
	root  string
	kind  Kind
	mtime int64 // UnixNano of the manifest at cache-prime; 0 for negative entries
}

// Compile-time assertion: ShellVerifier satisfies the Verifier interface.
var _ Verifier = (*ShellVerifier)(nil)

// NewShellVerifier wires a ShellVerifier and starts its worker pool.
// The workers live until Close is called.
//
// If cfg.Concurrency is non-positive the verifier is dormant: Submit
// always returns false and Close is a no-op. This lets callers wire
// the verifier unconditionally without checking config.
func NewShellVerifier(cfg Config) *ShellVerifier {
	cfg.applyDefaults()
	v := &ShellVerifier{
		cfg:         cfg,
		queue:       make(chan Event, cfg.QueueDepth),
		closed:      make(chan struct{}),
		droppedRing: newDroppedRing(cfg.DroppedRingSize),
	}
	if cfg.Concurrency <= 0 {
		close(v.closed)
		return v
	}
	for i := 0; i < cfg.Concurrency; i++ {
		v.wg.Add(1)
		go v.worker()
	}
	return v
}

// Enabled reports whether the verifier will actually run checks.
// Useful for logging in main and for tests.
func (v *ShellVerifier) Enabled() bool {
	if v == nil {
		return false
	}
	return v.cfg.Concurrency > 0
}

// Concurrency returns the configured worker count.
func (v *ShellVerifier) Concurrency() int { return v.cfg.Concurrency }

// QueueDepth returns the current number of buffered, unverified
// edits. Mostly useful for tests and operational dashboards.
func (v *ShellVerifier) QueueDepth() int {
	if v == nil {
		return 0
	}
	return len(v.queue)
}

// Dropped returns the number of events dropped because the queue was
// full. Tests assert on this to verify the non-blocking contract.
func (v *ShellVerifier) Dropped() uint64 {
	if v == nil {
		return 0
	}
	return v.dropped.Load()
}

// DroppedEvents returns the most recently dropped events up to the
// configured ring size, newest first.
func (v *ShellVerifier) DroppedEvents() []Event {
	if v == nil {
		return nil
	}
	return v.droppedRing.Events()
}

// DroppedRingCapacity returns the maximum number of dropped events
// retained in the ring buffer for inspection.
func (v *ShellVerifier) DroppedRingCapacity() int {
	if v == nil {
		return 0
	}
	return v.droppedRing.size
}

// Submit enqueues e for asynchronous verification. It is the non-
// blocking dispatch entry point: callers (the chat handler) invoke it
// and return immediately, never waiting on the worker pool.
//
// Returns false if the queue is full or the verifier has been
// closed. Callers may log this; we deliberately do not block the
// request goroutine.
func (v *ShellVerifier) Submit(e Event) bool {
	if !v.Enabled() {
		return false
	}
	select {
	case <-v.closed:
		return false
	default:
	}
	select {
	case v.queue <- e:
		return true
	default:
		v.dropped.Add(1)
		v.droppedRing.Push(e)
		slog.Warn("quality queue full, dropped edit",
			slog.String("request_id", e.RequestID),
			slog.String("path", e.Path),
		)
		return false
	}
}

// Close drains the queue and stops the worker pool. Safe to call
// multiple times. Nil-safe so main.go's deferred Close survives a
// configuration that wires nil verifier (e.g. dormant operator).
func (v *ShellVerifier) Close() error {
	if v == nil {
		return nil
	}
	v.closeOnce.Do(func() {
		close(v.queue)
	})
	v.wg.Wait()
	return nil
}

// worker pulls from the queue and runs Verify + observer.Submit until
// the queue is closed and drained.
func (v *ShellVerifier) worker() {
	defer v.wg.Done()
	for e := range v.queue {
		v.cfg.Observer.Submit(v.Verify(e))
	}
}

// Verify runs one check synchronously and returns the resulting
// Verdict (the observer hook is the caller's job). Exported so tests
// and future CLI tools can drive one check without spinning up the
// worker pool.
func (v *ShellVerifier) Verify(e Event) Verdict {
	started := v.cfg.Now()
	vv := Verdict{
		Event:     e,
		Timestamp: started.UTC(),
	}

	root, kind, err := v.lookupProject(e.Path)
	if err != nil {
		vv.Err = err
		vv.DurationMs = v.cfg.Now().Sub(started).Milliseconds()
		return vv
	}
	vv.RepoRoot = root
	vv.Kind = kind
	if root == "" {
		// Walked all the way up without finding a manifest — record
		// a "no project" verdict with Pass=false so dashboards
		// still see the request id. Err is left nil because this is
		// not an infrastructure failure; it is a detection outcome.
		vv.DurationMs = v.cfg.Now().Sub(started).Milliseconds()
		return vv
	}

	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.Timeout)
	defer cancel()

	exitCode, stderr, err := runCheck(ctx, root, kind, e.TraceParent, e.TraceState)
	vv.ExitCode = exitCode
	vv.Stderr = capStderr(stderr, v.cfg.StderrCap)
	vv.DurationMs = v.cfg.Now().Sub(started).Milliseconds()
	if err != nil {
		vv.Err = err
		return vv
	}
	vv.Pass = exitCode == 0
	return vv
}

// lookupProject walks up from filePath's directory looking for a
// recognised manifest. Returns (root, kind, nil); returns ("", "", err)
// only on a context-like cancellation — currently always nil.
//
// Cache entries store the mtime of the discovered manifest so that
// future calls detect when the manifest file is created or deleted
// after cache prime (issue #1067). Negative entries (no manifest) have
// mtime=0 and are invalidated by checking whether any manifest now exists.
func (v *ShellVerifier) lookupProject(filePath string) (string, Kind, error) {
	if filePath == "" {
		return "", KindUnknown, nil
	}
	dir, err := filepath.Abs(filePath)
	if err != nil {
		// Fall back to using the path verbatim — detecting project
		// roots tolerates relative paths in tests.
		dir = filePath
	} else {
		dir = filepath.Dir(dir)
	}
	// Walk up to a small bounded depth. We could walk until filepath.Dir
	// stabilises but a tight cap keeps the worst-case test from
	// spending time scanning a deep /usr/include tree.
	const maxDepth = 32
	for i := 0; i < maxDepth; i++ {
		// Cache key: the source directory we started from. Repeat
		// hits on the same dir skip the walk entirely.
		cacheKey := dir
		if cached, ok := v.cache.Load(cacheKey); ok {
			h := cached.(projectHint)
			// mtime-based invalidation: rewalk if manifest changed.
			if h.root != "" && h.kind != KindUnknown {
				marker := h.kind.Marker()
				if marker == "" {
					return h.root, h.kind, nil
				}
				candidate := filepath.Join(h.root, marker)
				fi, err := os.Stat(candidate)
				if err != nil || fi.ModTime().UnixNano() != h.mtime {
					// Manifest missing or mtime changed — invalidate and rewalk.
					v.cache.Delete(cacheKey)
				} else {
					return h.root, h.kind, nil
				}
			} else {
				// Negative entry: rewalk only if a manifest now exists.
				if hasAnyManifest(dir) {
					v.cache.Delete(cacheKey)
				} else {
					return h.root, h.kind, nil
				}
			}
		}
		for _, k := range AllKinds {
			marker := k.Marker()
			if marker == "" {
				continue
			}
			candidate := filepath.Join(dir, marker)
			fi, err := os.Stat(candidate)
			if err == nil {
				v.cache.Store(cacheKey, projectHint{root: dir, kind: k, mtime: fi.ModTime().UnixNano()})
				return dir, k, nil
			}
		}
		// Cache the negative result for this dir too; further edits
		// in the same subtree skip the stat loop.
		parent := filepath.Dir(dir)
		if parent == dir {
			v.cache.Store(cacheKey, projectHint{root: "", kind: KindUnknown})
			return "", KindUnknown, nil
		}
		dir = parent
	}
	return "", KindUnknown, nil
}

// hasAnyManifest returns true if any recognised project manifest
// exists in dir. Used to invalidate negative cache entries when a
// manifest is created after cache prime.
func hasAnyManifest(dir string) bool {
	for _, k := range AllKinds {
		marker := k.Marker()
		if marker == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// runCheck executes the project's check command inside repoRoot. The
// NEXUS_QUALITY_SHELL_OVERRIDE env var, when set, replaces the real
// command with `/bin/sh -c <value>` — tests use this for deterministic
// exit-code control.
//
// Returns (exitCode, stderr, err). When the process exits non-zero
// but cleanly, err is nil and exitCode is the OS-reported code (cast
// from *exec.ExitError). err is non-nil only for fork / I/O
// failures.
func runCheck(ctx context.Context, repoRoot string, kind Kind, traceParent, traceState string) (int, []byte, error) {
	var cmd *exec.Cmd
	if override := os.Getenv("NEXUS_QUALITY_SHELL_OVERRIDE"); override != "" {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", override)
	} else {
		switch kind {
		case KindRust:
			manifest := filepath.Join(repoRoot, "Cargo.toml")
			cmd = exec.CommandContext(ctx, "cargo", "check",
				"--manifest-path", manifest, "--quiet")
		case KindTS:
			cfg := filepath.Join(repoRoot, "tsconfig.json")
			cmd = exec.CommandContext(ctx, "npx", "--no", "tsc",
				"--noEmit", "-p", cfg)
		default:
			return -1, nil, fmt.Errorf("quality: unsupported project kind %q", kind)
		}
	}
	cmd.Dir = repoRoot

	// Propagate W3C trace context to subprocess via environment variables (issue #233).
	if traceParent != "" {
		cmd.Env = append(cmd.Env, "TRACEPARENT="+traceParent)
	}
	if traceState != "" {
		cmd.Env = append(cmd.Env, "TRACESTATE="+traceState)
	}

	// WaitDelay bounds the time Wait() will spend draining I/O
	// after the process has already exited. Without it, an orphan
	// grand-child that inherited the cmd's pipes (e.g. `sleep 30`
	// forked from `/bin/sh -c`) can hold the write end open after
	// /bin/sh is killed, freezing Wait() until the orphan exits.
	// 5s is well above any real cargo / tsc I/O drain and well
	// below our worker timeout budget.
	cmd.WaitDelay = 5 * time.Second

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Discard stdout — `cargo check` and `tsc --noEmit` are both
	// quiet enough on success and noisy enough on failure that
	// stderr alone is sufficient for diagnostics. Capture both
	// anyway and prefer stderr in the verdict.
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err == nil {
		return 0, stderr.Bytes(), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// Prefer stderr — but fall back to stdout when the tool
		// only wrote to stdout (some tsc invocations do this).
		if stderr.Len() == 0 {
			return ee.ExitCode(), stdout.Bytes(), nil
		}
		return ee.ExitCode(), stderr.Bytes(), nil
	}
	// Fork / context-cancelled / unknown failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, nil, fmt.Errorf("quality: %w", ctxErr)
	}
	return -1, stderr.Bytes(), fmt.Errorf("quality: run: %w", err)
}

// capStderr returns at most max bytes of src, preferring the tail so
// the most recent diagnostic lines (which usually name the failing
// symbol) survive. Returns "" when max <= 0 or src is empty.
func capStderr(src []byte, max int) string {
	if max <= 0 || len(src) == 0 {
		return ""
	}
	if len(src) <= max {
		return string(src)
	}
	// Drop from the head, keep the tail. Prefix with an ellipsis so
	// consumers can tell the truncation happened.
	elided := len(src) - max
	return "[... " + itoa(elided) + " bytes elided ...]\n" + string(src[elided:])
}

// itoa is a small dependency-free integer formatter to keep capStderr
// alloc-light in the hot path. Negative values yield "-N".
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// EncodeEvent returns a stable, debug-friendly representation of e.
// Useful for log lines and test assertions; never used as a wire
// format (verdicts use Verdict's own fields).
func EncodeEvent(e Event) string {
	var b strings.Builder
	b.WriteString("Event{request=")
	b.WriteString(e.RequestID)
	b.WriteString(" path=")
	b.WriteString(e.Path)
	b.WriteString(" tool=")
	b.WriteString(e.ToolName)
	if len(e.Args) > 0 {
		b.WriteString(" args=")
		if len(e.Args) <= droppedEventArgsCap {
			b.Write(e.Args)
		} else {
			b.Write(e.Args[:droppedEventArgsCap])
			b.WriteString("...[truncated]")
		}
	}
	b.WriteByte('}')
	return b.String()
}
