package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoopRecorderSafe(t *testing.T) {
	var r Recorder = Noop{}
	r.Record(Record{RequestID: "x"}) // must not panic
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := map[string]struct{ min, max int }{
		"":         {0, 0},
		"a":        {1, 1}, // tiktoken: 1 token
		"abcd":     {1, 1}, // tiktoken: 1 token (short strings compress to 1 in cl100k_base)
		"abcdefgh": {1, 1}, // tiktoken: 1 token (same)
		"long text this is a sentence with many many words here please": {11, 13}, // tiktoken: 12 tokens
	}
	for in, tc := range cases {
		got := EstimateTokens(in)
		if got < tc.min || got > tc.max {
			t.Errorf("EstimateTokens(%q) = %d, want [%d, %d]", in, got, tc.min, tc.max)
		}
	}
}

func TestComputeTPS(t *testing.T) {
	cases := []struct {
		name         string
		outputTokens int
		ttftMs       int64
		totalMs      float64
		want         float64
	}{
		{"no tokens", 0, 100, 200, 0},
		{"total zero", 50, 0, 0, 0},
		{"gen window zero", 50, 200, 200, 0},
		{"ttft > total (rounding)", 50, 300, 200, 0},
		{"happy 100 tok in 1s", 100, 200, 1200, 100.0},
		{"happy 250 tok in 0.5s gen", 250, 500, 1000, 500.0},
		{"sub-ms total (issue #68)", 100, 0, 0.5, 200000.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeTPS(tc.outputTokens, tc.ttftMs, tc.totalMs)
			if !approxEqual(got, tc.want, 0.001) {
				t.Errorf("ComputeTPS(%d,%d,%f) = %f, want %f",
					tc.outputTokens, tc.ttftMs, tc.totalMs, got, tc.want)
			}
		})
	}
}

func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func TestNewJSONLRecorderCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	defer r.Close()
	if r.Path() != path {
		t.Errorf("Path = %q, want %q", r.Path(), path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestJSONLRecorderFilePerms0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	defer r.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file perm = %o, want 0600", got)
	}
}

func TestJSONLRecorderTightensExistingFilePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tel.jsonl")
	// Create a file with permissive mode (simulating a pre-fix file).
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	defer r.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("existing file perm = %o, want 0600", got)
	}
}

func TestJSONLRecorderParentDirPerms0700(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	defer r.Close()
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("parent dir perm = %o, want 0700", got)
	}
}

func TestNewJSONLRecorderEmptyPathErrors(t *testing.T) {
	if _, err := NewJSONLRecorder("", 0, 0); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestJSONLRecorderWritesRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	rec := Record{
		Timestamp:      time.Date(2026, 7, 10, 12, 34, 56, 0, time.UTC),
		RequestID:      "abc123",
		Model:          "qwen3-coder:8b",
		Route:          "local",
		InputTokens:    42,
		OutputTokens:   100,
		TTFTMs:         150,
		TotalLatencyMs: 1230,
		TPS:            92.59,
		Streaming:      true,
	}
	r.Record(rec)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1: %q", len(lines), data)
	}
	var got Record
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%q", err, lines[0])
	}
	if got.RequestID != rec.RequestID || got.Model != rec.Model || got.Route != rec.Route {
		t.Errorf("got %+v, want request_id=%s model=%s route=%s",
			got, rec.RequestID, rec.Model, rec.Route)
	}
	if got.InputTokens != rec.InputTokens || got.OutputTokens != rec.OutputTokens {
		t.Errorf("tokens mismatch: got %+v", got)
	}
	if got.TTFTMs != rec.TTFTMs || got.TotalLatencyMs != rec.TotalLatencyMs {
		t.Errorf("latency mismatch: got %+v", got)
	}
	if !approxEqual(got.TPS, rec.TPS, 0.01) {
		t.Errorf("tps = %f, want %f", got.TPS, rec.TPS)
	}
	if !got.Streaming {
		t.Errorf("streaming flag lost")
	}
}

func TestJSONLRecorderAppendsAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	for i := 0; i < 5; i++ {
		r.Record(Record{RequestID: "r", Route: "local", OutputTokens: i})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want 5: %q", len(lines), data)
	}
}

func TestJSONLRecorderDoesNotBlockOnFullBuffer(t *testing.T) {
	// Use a path inside a directory we control; the goroutine is healthy so
	// the channel drains quickly. To force overflow we Record from many
	// goroutines while closing the file handle out from under the writer is
	// too brittle — instead, fill the buffer synchronously by stopping the
	// consumer. We achieve this by closing the recorder channel under the
	// recorder's feet via a small, dedicated stress test.
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	// Close immediately so the next non-blocking sends see a closed-but-
	// draining channel; the buffer still accepts up to its capacity without
	// blocking. Record must NEVER block regardless of channel state.
	done := make(chan struct{})
	go func() {
		for i := 0; i < bufferedChannelSize*4; i++ {
			r.Record(Record{RequestID: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked under burst — non-blocking contract broken")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJSONLRecorderDropsWhenChannelFull(t *testing.T) {
	// Hold the consumer busy so the buffer fills, then verify Record drops
	// (rather than blocking) when the channel is saturated.
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	// We cannot stop the goroutine, but we can fill the channel: produce
	// more records than the buffer holds, see if any are dropped. Disk is
	// fast enough on tmpfs that drops may be 0; we only assert no panic
	// and non-blocking. The previous test covers the hard guarantee.
	for i := 0; i < bufferedChannelSize*2; i++ {
		r.Record(Record{RequestID: "r"})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close, further Record calls are no-ops and must not panic.
	r.Record(Record{RequestID: "after-close"})
}

func TestNewRequestIDFormat(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if len(id) != 16 {
			t.Fatalf("id %q length %d, want 16", id, len(id))
		}
		for _, r := range id {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("id %q contains non-hex %q", id, r)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = struct{}{}
	}
}

// ---- ObservingWriter ------------------------------------------------------

type flushSpy struct {
	http.ResponseWriter
	flushed int
}

func (f *flushSpy) Flush() { f.flushed++ }

type recordRW struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *recordRW) Header() http.Header         { return r.header }
func (r *recordRW) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recordRW) WriteHeader(s int)           { r.status = s }

func TestObservingWriterFirstWriteHookFiresOnce(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	var fired atomic.Int32
	var firstAt atomic.Int64
	w := NewObservingWriter(inner, func(t time.Time) {
		fired.Add(1)
		firstAt.Store(t.UnixNano())
	})
	w.Write([]byte("hello "))
	w.Write([]byte("world"))
	if got := fired.Load(); got != 1 {
		t.Errorf("hook fired %d times, want 1", got)
	}
	if got := firstAt.Load(); got == 0 {
		t.Error("firstAt not set")
	}
	if got := w.BytesOut(); got != 11 {
		t.Errorf("BytesOut = %d, want 11", got)
	}
}

func TestObservingWriterNoHookStillCounts(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	w.Write([]byte("abcd"))
	if got := w.BytesOut(); got != 4 {
		t.Errorf("BytesOut = %d, want 4", got)
	}
}

func TestObservingWriterFlushForwardsToInner(t *testing.T) {
	inner := &flushSpy{ResponseWriter: &recordRW{header: http.Header{}}}
	w := NewObservingWriter(inner, nil)
	w.Flush()
	if inner.flushed != 1 {
		t.Errorf("inner flushed %d times, want 1", inner.flushed)
	}
}

func TestObservingWriterFlushNoopWhenInnerNotFlusher(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	// Must not panic when inner doesn't implement Flusher.
	w.Flush()
}

// Roundtrip helper used to assert writes are streamed verbatim to inner.
func TestObservingWriterPassesBytesThrough(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	in := []byte("data: {\"x\":1}\n\n")
	n, err := w.Write(in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(in) {
		t.Errorf("n = %d, want %d", n, len(in))
	}
	if inner.body.String() != string(in) {
		t.Errorf("body = %q, want %q", inner.body.String(), in)
	}
}

func TestObservingWriterStatusCodeAfterWriteHeader(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	w.WriteHeader(http.StatusCreated)
	if got := w.StatusCode(); got != http.StatusCreated {
		t.Errorf("StatusCode() = %d, want %d", got, http.StatusCreated)
	}
	if inner.status != http.StatusCreated {
		t.Errorf("inner status = %d, want %d", inner.status, http.StatusCreated)
	}
}

func TestObservingWriterStatusCodeReturns200WhenWriteHeaderNeverCalled(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	// WriteHeader never called — Go default is 200
	if got := w.StatusCode(); got != http.StatusOK {
		t.Errorf("StatusCode() = %d, want %d (Go default)", got, http.StatusOK)
	}
}

func TestObservingWriterWriteHeaderIdempotent(t *testing.T) {
	inner := &recordRW{header: http.Header{}}
	w := NewObservingWriter(inner, nil)
	w.WriteHeader(http.StatusOK)
	// Second call: recording is CAS-idempotent (first status wins) but the
	// wrapper still forwards to inner (Go would panic on second call in real
	// http handlers, so httptest never sees the forward either).
	w.WriteHeader(http.StatusInternalServerError)
	if got := w.StatusCode(); got != http.StatusOK {
		t.Errorf("StatusCode() = %d, want %d (first call wins)", got, http.StatusOK)
	}
}

// ---- Rotation (issue #485) -----------------------------------------------

// countJSONLLines returns the number of newline-terminated JSON lines in
// path. Fails the test if the file cannot be read.
func countJSONLLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// rotatedGlob returns the rotated-file matches for path (path.<suffix>).
func rotatedGlob(path string) []string {
	m, _ := filepath.Glob(path + ".*")
	return m
}

// sampleLineLen marshals a representative record and returns its on-disk
// line length (JSON + trailing newline) so rotation thresholds can be set
// deterministically.
func sampleLineLen(t *testing.T) int {
	t.Helper()
	rec := Record{
		RequestID: "rot-test",
		Route:     "local",
		Model:     "qwen3-coder:8b",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	return len(b) + 1
}

func TestNoopRotationsZero(t *testing.T) {
	n := Noop{}
	if got := n.Rotations(); got != 0 {
		t.Errorf("Noop.Rotations() = %d, want 0", got)
	}
}

// With MAX_BYTES=0 (default) the recorder must behave identically to the
// pre-#485 path: a single append-only file, no rotated siblings, and a
// rotation counter pinned at zero.
func TestJSONLRecorderRotationDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, 0, 0)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	for i := 0; i < 20; i++ {
		r.Record(Record{RequestID: "r", Route: "local"})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.Rotations(); got != 0 {
		t.Errorf("Rotations = %d, want 0 (rotation disabled)", got)
	}
	if extras := rotatedGlob(path); len(extras) != 0 {
		t.Errorf("expected no rotated files, got %v", extras)
	}
	if got := countJSONLLines(t, path); got != 20 {
		t.Errorf("active file lines = %d, want 20", got)
	}
}

// Rotation fires exactly when the next record would cross the byte cap:
// with maxBytes = 2*lineLen the first two records share a file and the
// third triggers exactly one rotation, landing in a fresh active file.
func TestJSONLRecorderRotatesAtThreshold(t *testing.T) {
	lineLen := sampleLineLen(t)
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	// Two records fit exactly (2*lineLen); the third crosses the cap.
	r, err := NewJSONLRecorder(path, int64(2*lineLen), 5)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	for i := 0; i < 3; i++ {
		r.Record(Record{RequestID: "r", Route: "local", Model: "qwen3-coder:8b"})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.Rotations(); got != 1 {
		t.Fatalf("Rotations = %d, want 1", got)
	}
	extras := rotatedGlob(path)
	if len(extras) != 1 {
		t.Fatalf("rotated files = %d, want 1: %v", len(extras), extras)
	}
	if got := countJSONLLines(t, extras[0]); got != 2 {
		t.Errorf("rotated file lines = %d, want 2", got)
	}
	if got := countJSONLLines(t, path); got != 1 {
		t.Errorf("active file lines = %d, want 1", got)
	}
}

// At the rotated-file cap the oldest sibling is evicted: with maxFiles=2
// and enough writes to rotate three times, exactly two rotated files plus
// the active file remain on disk.
func TestJSONLRecorderEvictsOldestAtCap(t *testing.T) {
	lineLen := sampleLineLen(t)
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, int64(2*lineLen), 2)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	// 8 records → rotations at records 3, 5, 7 (3 rotations). maxFiles=2
	// evicts the oldest after the third, leaving 2 rotated + active.
	for i := 0; i < 8; i++ {
		r.Record(Record{RequestID: "r", Route: "local", Model: "qwen3-coder:8b"})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.Rotations(); got != 3 {
		t.Fatalf("Rotations = %d, want 3", got)
	}
	extras := rotatedGlob(path)
	if len(extras) != 2 {
		t.Fatalf("rotated files = %d, want 2: %v", len(extras), extras)
	}
	// Active file must still exist and hold the final two records.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active file missing: %v", err)
	}
	if got := countJSONLLines(t, path); got != 2 {
		t.Errorf("active file lines = %d, want 2", got)
	}
}

// Acceptance criterion (issue #485): with a size cap and maxFiles=3,
// writing enough records to rotate well past the cap leaves exactly
// three rotated files plus the active file. The production scenario is
// a 1 MiB cap against ~5 MiB of records; this test uses the same 5:1
// ratio with a proportionally smaller cap so the record count stays
// below the 1024-entry channel buffer (no drops → deterministic).
func TestJSONLRecorderRotationAcceptance(t *testing.T) {
	lineLen := sampleLineLen(t)
	const maxFiles = 3
	maxBytes := int64(2 * lineLen) // two records per rotated file
	// 12 records = 6 files of 2 → 6 rotations, well past maxFiles.
	numRecords := 12

	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path, maxBytes, maxFiles)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	for i := 0; i < numRecords; i++ {
		r.Record(Record{RequestID: "r", Route: "local", Model: "qwen3-coder:8b"})
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, want 0 (records must fit the buffer)", got)
	}
	if got := r.Rotations(); got <= uint64(maxFiles) {
		t.Errorf("Rotations = %d, want > %d (test did not exercise eviction)", got, maxFiles)
	}
	extras := rotatedGlob(path)
	if len(extras) != maxFiles {
		t.Fatalf("rotated files = %d, want exactly %d: %v", len(extras), maxFiles, extras)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active file missing: %v", err)
	}
}

// A pre-existing file larger than maxBytes is rotated on the very first
// record after boot, so a restart against an oversized log does not keep
// appending past the cap.
func TestJSONLRecorderRotatesPreExistingOversizedFile(t *testing.T) {
	lineLen := sampleLineLen(t)
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	// Seed a file already past the 2*lineLen cap.
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 3*lineLen), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewJSONLRecorder(path, int64(2*lineLen), 5)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	r.Record(Record{RequestID: "first", Route: "local", Model: "qwen3-coder:8b"})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := r.Rotations(); got != 1 {
		t.Fatalf("Rotations = %d, want 1 (pre-existing file should rotate on first record)", got)
	}
	extras := rotatedGlob(path)
	if len(extras) != 1 {
		t.Fatalf("rotated files = %d, want 1: %v", len(extras), extras)
	}
	// The rotated file preserves the seeded bytes.
	info, err := os.Stat(extras[0])
	if err != nil {
		t.Fatalf("stat rotated: %v", err)
	}
	if info.Size() != int64(3*lineLen) {
		t.Errorf("rotated file size = %d, want %d", info.Size(), 3*lineLen)
	}
	// The new active file holds exactly the one fresh record.
	if got := countJSONLLines(t, path); got != 1 {
		t.Errorf("active file lines = %d, want 1", got)
	}
}

// Rotation is race-safe under concurrent Record callers: many goroutines
// push records while the run loop rotates behind a small cap. Run under
// -race to catch any unsynchronised file-handle access. The total record
// count is kept below the 1024-entry channel buffer so no records drop,
// making the line-count assertion deterministic.
func TestJSONLRecorderRotationConcurrentSafe(t *testing.T) {
	lineLen := sampleLineLen(t)
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	// Large enough cap that the consumer stays fast (few rotations) but
	// small enough that at least one rotation fires under load.
	r, err := NewJSONLRecorder(path, int64(400*lineLen), 2)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	const goroutines = 8
	const perGoroutine = 100 // 800 total < 1024 buffer → no drops
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				r.Record(Record{RequestID: "r", Route: "local", Model: "qwen3-coder:8b"})
			}
		}()
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	total := goroutines * perGoroutine
	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, want 0", got)
	}
	// Every record must survive across active + rotated files as valid JSONL.
	files := append(rotatedGlob(path), path)
	count := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Errorf("corrupt JSONL in %s: %v body=%q", f, err, line)
			}
			count++
		}
	}
	if count != total {
		t.Errorf("total valid lines = %d, want %d", count, total)
	}
}
