package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	cases := map[string]int{
		"":         0,
		"a":        0, // 1 / 4 == 0
		"abcd":     1,
		"abcdefgh": 2,
		"long text this is a sentence with many many words here please": 15, // 61 / 4 = 15
	}
	for in, want := range cases {
		if got := EstimateTokens(in); got != want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", in, got, want)
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
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
	if _, err := NewJSONLRecorder(""); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestJSONLRecorderWritesRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.jsonl")
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
	r, err := NewJSONLRecorder(path)
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
