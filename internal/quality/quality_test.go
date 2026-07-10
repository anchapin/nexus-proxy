package quality

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingObserver captures every verdict on a buffered channel so
// tests can assert the worker pool's dispatch behaviour without
// sleeping. Safe for concurrent Submit calls.
type recordingObserver struct {
	mu  sync.Mutex
	out []Verdict
}

func (r *recordingObserver) Submit(v Verdict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = append(r.out, v)
}

func (r *recordingObserver) Snapshot() []Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Verdict, len(r.out))
	copy(out, r.out)
	return out
}

// waitForVerdicts polls r until at least n verdicts have been recorded
// or the deadline elapses. Bounded to keep tests fast and surface
// deadlocks quickly.
func waitForVerdicts(t *testing.T, r *recordingObserver, n int, d time.Duration) []Verdict {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		got := r.Snapshot()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r.Snapshot()
}

// withShellOverride sets and clears NEXUS_QUALITY_SHELL_OVERRIDE for a
// single test, ensuring the env doesn't leak into siblings.
func withShellOverride(t *testing.T, cmd string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("NEXUS_QUALITY_SHELL_OVERRIDE")
	if err := os.Setenv("NEXUS_QUALITY_SHELL_OVERRIDE", cmd); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("NEXUS_QUALITY_SHELL_OVERRIDE", prev)
		} else {
			_ = os.Unsetenv("NEXUS_QUALITY_SHELL_OVERRIDE")
		}
	})
}

// makeRustRepo creates a temp directory containing Cargo.toml and
// returns its absolute path. The Cargo.toml is syntactically valid
// enough for `os.Stat` to find it.
func makeRustRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname=\"x\"\nversion=\"0.0.0\"\nedition=\"2021\"\n"), 0o644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}
	return dir
}

// makeTSRepo creates a temp directory containing tsconfig.json.
func makeTSRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte("{\"compilerOptions\":{\"strict\":true}}\n"), 0o644); err != nil {
		t.Fatalf("write tsconfig.json: %v", err)
	}
	return dir
}

// TestVerifierShellOverrideExitZero verifies the happy path: exit 0
// from the override command yields Pass=true and is forwarded to the
// observer.
func TestVerifierShellOverrideExitZero(t *testing.T) {
	repo := makeRustRepo(t)
	withShellOverride(t, "exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  1,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	if !v.Enabled() {
		t.Fatal("verifier should be enabled")
	}
	if !v.Submit(Event{RequestID: "req-zero", Path: filepath.Join(repo, "src", "lib.rs"), ToolName: "write_file"}) {
		t.Fatal("Submit should enqueue (queue depth 1, worker idle)")
	}
	got := waitForVerdicts(t, obs, 1, 2*time.Second)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	vv := got[0]
	if !vv.Pass {
		t.Errorf("Pass = false, want true; verdict=%+v", vv)
	}
	if vv.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", vv.ExitCode)
	}
	if vv.RepoRoot != repo {
		t.Errorf("RepoRoot = %q, want %q", vv.RepoRoot, repo)
	}
	if vv.Kind != KindRust {
		t.Errorf("Kind = %q, want %q", vv.Kind, KindRust)
	}
	if vv.Err != nil {
		t.Errorf("Err = %v, want nil", vv.Err)
	}
	if vv.Event.RequestID != "req-zero" {
		t.Errorf("RequestID propagated incorrectly: %q", vv.Event.RequestID)
	}
}

// TestVerifierShellOverrideExitOne verifies the failure path: a
// non-zero exit (without an Err) still yields Pass=false and the exit
// code is surfaced.
func TestVerifierShellOverrideExitOne(t *testing.T) {
	repo := makeTSRepo(t)
	withShellOverride(t, "echo 'type error: X is not assignable to Y' 1>&2; exit 1")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  1,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	if !v.Submit(Event{RequestID: "req-one", Path: filepath.Join(repo, "src", "index.ts"), ToolName: "edit_file"}) {
		t.Fatal("Submit should enqueue")
	}
	got := waitForVerdicts(t, obs, 1, 2*time.Second)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	vv := got[0]
	if vv.Pass {
		t.Errorf("Pass = true, want false; verdict=%+v", vv)
	}
	if vv.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", vv.ExitCode)
	}
	if vv.Err != nil {
		t.Errorf("Err = %v, want nil (non-zero exit is data, not infrastructure failure)", vv.Err)
	}
	if !contains(vv.Stderr, "type error") {
		t.Errorf("stderr did not contain diagnostic; got %q", vv.Stderr)
	}
	if vv.Kind != KindTS {
		t.Errorf("Kind = %q, want %q", vv.Kind, KindTS)
	}
}

// TestVerifierTimeoutTriggersFailure verifies that a check that runs
// longer than cfg.Timeout surfaces Err and Pass=false with the right
// exit-code sentinel.
//
// The wait deadline includes the configured I/O WaitDelay (5s)
// because the orphan sleep 30 child holds the cmd pipe write end
// after /bin/sh is killed; cmd.Run only returns once the I/O drain
// completes or WaitDelay elapses. Both paths are correct.
func TestVerifierTimeoutTriggersFailure(t *testing.T) {
	repo := makeRustRepo(t)
	withShellOverride(t, "sleep 30; exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  1,
		Timeout:     50 * time.Millisecond,
		Observer:    obs,
	})
	if !v.Submit(Event{RequestID: "req-timeout", Path: filepath.Join(repo, "src", "lib.rs")}) {
		t.Fatal("Submit should enqueue")
	}
	// Cover cfg.Timeout (50ms) + WaitDelay (5s) + slack.
	got := waitForVerdicts(t, obs, 1, 7*time.Second)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	vv := got[0]
	if vv.Pass {
		t.Errorf("Pass = true, want false on timeout")
	}
	if vv.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (signal-kill sentinel)", vv.ExitCode)
	}
	// Err is intentionally nil: the process exited (it was killed
	// by signal); runCheck's err-as-data rule keeps the verdict
	// well-formed (Pass=false, ExitCode=-1) without mapping the
	// signal into Err. Err is reserved for fork / setup failures.
	if vv.Err != nil {
		t.Errorf("Err = %v, want nil (signal kill is data, not error)", vv.Err)
	}
	if vv.DurationMs > 6500 {
		t.Errorf("DurationMs = %d, suspiciously large for 50ms timeout", vv.DurationMs)
	}
}

// TestVerifierBoundedConcurrency verifies that the worker pool honours
// cfg.Concurrency: with Concurrency=2 and 6 jobs of fixed duration,
// the average throughput should be ~2x what a single worker would
// achieve. We approximate by measuring wall-clock time for 4 jobs
// that each "sleep 0.1".
func TestVerifierBoundedConcurrency(t *testing.T) {
	repo := makeRustRepo(t)
	// /bin/sh sleep 0.1 sleeps 100ms; the timeout (5s) is plenty.
	withShellOverride(t, "sleep 0.1; exit 0")

	const jobs = 4
	const conc = 2
	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: conc,
		QueueDepth:  jobs,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	started := time.Now()
	for i := 0; i < jobs; i++ {
		v.Submit(Event{RequestID: "r", Path: filepath.Join(repo, "src", "lib.rs")})
	}
	got := waitForVerdicts(t, obs, jobs, 10*time.Second)
	elapsed := time.Since(started)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != jobs {
		t.Fatalf("got %d verdicts, want %d", len(got), jobs)
	}
	// With conc=2 and 4x100ms jobs, ideal floor is ~200ms. Add a
	// generous slack to absorb CI flakes.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed = %v, want < 1.5s for %d jobs at conc=%d", elapsed, jobs, conc)
	}
}

// TestVerifierNonBlockingDropOnOverflow verifies that Submit returns
// false instead of blocking when the queue is full and no worker can
// drain it yet.
func TestVerifierNonBlockingDropOnOverflow(t *testing.T) {
	repo := makeRustRepo(t)
	// Each job sleeps longer than the test's wait window so the
	// single worker stays busy.
	withShellOverride(t, "sleep 0.5; exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  2,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	// Fill the queue (conc=1, depth=2 -> 3 buffered before drop).
	ok := true
	for i := 0; i < 8; i++ {
		if !v.Submit(Event{RequestID: "r", Path: filepath.Join(repo, "lib.rs")}) {
			ok = false
		}
	}
	// At least one of the 8 calls must have been dropped.
	if v.Dropped() == 0 {
		t.Errorf("Dropped = 0; want at least one drop in an 8-submit burst at conc=1, depth=2")
	}
	_ = ok // success codes are advisory
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestVerifierDormantDoesNothing verifies that a verifier with
// Concurrency <= 0 is dormant: Submit returns false, no workers run,
// Close is a no-op.
func TestVerifierDormantDoesNothing(t *testing.T) {
	repo := makeTSRepo(t)
	withShellOverride(t, "exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 0,
		Observer:    obs,
	})
	if v.Enabled() {
		t.Fatal("dormant verifier should be disabled")
	}
	if v.Submit(Event{RequestID: "x", Path: filepath.Join(repo, "i.ts")}) {
		t.Error("Submit should return false on dormant verifier")
	}
	if err := v.Close(); err != nil {
		t.Errorf("Close on dormant verifier: %v", err)
	}
	if v.QueueDepth() != 0 {
		t.Errorf("QueueDepth = %d, want 0", v.QueueDepth())
	}
	if v.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0", v.Dropped())
	}
}

// TestVerifierNoProjectScoresZero verifies that when no manifest is
// found by walking up, Pass=false and RepoRoot="" — but no Err is set
// (this is detection outcome, not infrastructure failure).
func TestVerifierNoProjectScoresZero(t *testing.T) {
	withShellOverride(t, "exit 0")

	dir := t.TempDir()
	// No manifest under dir or any parent we created; t.TempDir is
	// inside the test's working area, which has neither Cargo.toml
	// nor tsconfig.json at the project root.
	file := filepath.Join(dir, "nope.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  1,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	if !v.Submit(Event{RequestID: "no-proj", Path: file}) {
		t.Fatal("Submit should enqueue")
	}
	got := waitForVerdicts(t, obs, 1, 2*time.Second)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	vv := got[0]
	if vv.Pass {
		t.Errorf("Pass = true, want false when no project detected")
	}
	if vv.RepoRoot != "" {
		t.Errorf("RepoRoot = %q, want empty", vv.RepoRoot)
	}
	if vv.Kind != KindUnknown {
		t.Errorf("Kind = %q, want empty", vv.Kind)
	}
	if vv.Err != nil {
		t.Errorf("Err = %v, want nil (no project is an outcome, not a failure)", vv.Err)
	}
}

// TestVerifierProjectCache verifies that repeated edits in the same
// directory tree don't re-stat the filesystem: the second Verify
// returns the same (root, kind) without doing IO. We can't observe
// io behaviour directly here; instead we confirm both calls return
// the same RepoRoot.
func TestVerifierProjectCache(t *testing.T) {
	repo := makeRustRepo(t)
	withShellOverride(t, "exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{
		Concurrency: 1,
		QueueDepth:  4,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	v.Submit(Event{RequestID: "a", Path: filepath.Join(repo, "src", "a.rs")})
	v.Submit(Event{RequestID: "b", Path: filepath.Join(repo, "src", "sub", "b.rs")})
	got := waitForVerdicts(t, obs, 2, 2*time.Second)
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d verdicts, want 2", len(got))
	}
	for _, vv := range got {
		if vv.RepoRoot != repo {
			t.Errorf("RepoRoot = %q, want %q", vv.RepoRoot, repo)
		}
		if vv.Kind != KindRust {
			t.Errorf("Kind = %q, want %q", vv.Kind, KindRust)
		}
	}
}

// TestVerifySynchronous confirms Verify() produces correct verdicts
// without going through the worker pool. Useful for downstream tests
// and one-shot CLI tools.
func TestVerifySynchronous(t *testing.T) {
	repo := makeRustRepo(t)
	withShellOverride(t, "exit 0")

	obs := &recordingObserver{}
	v := NewShellVerifier(Config{Observer: obs})
	vv := v.Verify(Event{RequestID: "sync", Path: filepath.Join(repo, "x.rs")})
	if !vv.Pass {
		t.Errorf("Pass = false, want true")
	}
	if vv.RepoRoot != repo {
		t.Errorf("RepoRoot = %q, want %q", vv.RepoRoot, repo)
	}
	if len(obs.Snapshot()) != 0 {
		t.Errorf("synchronous Verify should not invoke Observer")
	}
}

// TestCapStderrLargerKeepsAll verifies capStderr is identity when src
// fits inside the cap.
func TestCapStderrLargerKeepsAll(t *testing.T) {
	got := capStderr([]byte("hello"), 100)
	if got != "hello" {
		t.Errorf("capStderr(5-byte, cap=100) = %q, want %q", got, "hello")
	}
}

// TestCapStderrSmallKeepsTail verifies capStderr truncates from the
// head and prefixes an ellipsis when src exceeds the cap.
func TestCapStderrSmallKeepsTail(t *testing.T) {
	src := make([]byte, 100)
	for i := range src {
		src[i] = 'a' + byte(i%26)
	}
	got := capStderr(src, 20)
	if !contains(got, "elided") {
		t.Errorf("expected elision marker in %q", got)
	}
	if !contains(got, string(src[len(src)-20:])) {
		t.Errorf("expected tail bytes in %q", got)
	}
}

// TestCapStderrEmptyReturnsEmpty verifies the no-allocation branch.
func TestCapStderrEmptyReturnsEmpty(t *testing.T) {
	if got := capStderr(nil, 100); got != "" {
		t.Errorf("capStderr(nil) = %q, want \"\"", got)
	}
	if got := capStderr([]byte("x"), 0); got != "" {
		t.Errorf("capStderr with cap=0 = %q, want \"\"", got)
	}
}

// TestObserverConcurrency checks the Observer interface contract:
// many concurrent Submit calls all land on the observer without data
// races. The test itself catches races under `go test -race`.
func TestObserverConcurrency(t *testing.T) {
	repo := makeRustRepo(t)
	withShellOverride(t, "exit 0")

	// Custom observer with a counter (race-detector friendly
	// because all mutations go through atomic.Add).
	var seen atomic.Uint64
	obs := ObserverFunc(func(_ Verdict) { seen.Add(1) })

	v := NewShellVerifier(Config{
		Concurrency: 4,
		QueueDepth:  32,
		Timeout:     5 * time.Second,
		Observer:    obs,
	})
	for i := 0; i < 20; i++ {
		v.Submit(Event{RequestID: "race", Path: filepath.Join(repo, "src", "lib.rs")})
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := seen.Load(); got != 20 {
		t.Errorf("observer saw %d verdicts, want 20", got)
	}
}

// TestDefaultsApplied verifies the applyDefaults helper covers all
// zero-value fields with safe operational defaults. Concurrency is
// intentionally NOT defaulted because zero means "dormant"; rewriting
// it to 2 would silently re-enable a verifier the operator
// disabled.
func TestDefaultsApplied(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.Concurrency != 0 {
		t.Errorf("Concurrency default = %d, want 0 (dormant by design)", cfg.Concurrency)
	}
	if cfg.QueueDepth != 64 {
		t.Errorf("QueueDepth default = %d, want 64", cfg.QueueDepth)
	}
	if cfg.Timeout != 60*time.Second {
		t.Errorf("Timeout default = %v, want 60s", cfg.Timeout)
	}
	if cfg.StderrCap != 2*1024 {
		t.Errorf("StderrCap default = %d, want %d", cfg.StderrCap, 2*1024)
	}
	if cfg.Observer == nil {
		t.Error("Observer default = nil, want no-op adapter")
	}
	if cfg.Now == nil {
		t.Error("Now default = nil, want time.Now")
	}
}

// TestNilVerifierSafe confirms the package treats a nil *ShellVerifier
// as gracefully disabled (defensive against main.go's deferred Close).
func TestNilVerifierSafe(t *testing.T) {
	var v *ShellVerifier
	if v.Enabled() {
		t.Error("nil ver.Enabled() = true, want false")
	}
	if v.Dropped() != 0 {
		t.Error("nil ver.Dropped() != 0")
	}
	if v.QueueDepth() != 0 {
		t.Error("nil ver.QueueDepth() != 0")
	}
	if err := v.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

// TestRunCheckExitsClean confirms the underlying exec helper
// tolerates an absent cargo binary. runCheck is package-private; the
// test lives in the same package to avoid exporting internals.
//
// We use the shell override so the helper doesn't need `cargo` on
// PATH — the test verifies runCheck's bookkeeping (exit code
// propagation, stderr capture, err classification), not the real
// cargo integration. Coverage of the real `cargo check` path is the
// issue #13 acceptance criteria's "manual end-to-end" step.
func TestRunCheckExitsClean(t *testing.T) {
	dir := makeRustRepo(t)
	withShellOverride(t, "exit 0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, stderr, err := runCheck(ctx, dir, KindRust)
	if err != nil {
		t.Fatalf("runCheck returned err=%v on clean exit", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// contains is a tiny stdlib-free substring check used by tests that
// need to assert stderr lines without importing strings for a one-off.
func contains(haystack, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
