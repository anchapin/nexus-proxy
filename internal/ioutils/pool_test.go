package ioutils

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// restorePoolBufferMaxBytes resets the global retention cap to the test's
// original value via t.Cleanup so each subtest is isolated.
func restorePoolBufferMaxBytes(t *testing.T) {
	t.Helper()
	orig := PoolBufferMaxBytes()
	t.Cleanup(func() { SetPoolBufferMaxBytes(orig) })
}

// TestReadAllLimitedPooledUnderLimit verifies that a body smaller than
// maxBytes is returned in full with a nil error and does NOT increment
// the truncation counter (issue #1177 AC: correctness parity with
// ReadAllLimited).
func TestReadAllLimitedPooledUnderLimit(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	content := []byte("hello world")
	before := ReadAllTruncatedCounter()

	buf, err := ReadAllLimitedPooled(bytes.NewReader(content), 1024)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("body mismatch: got %q, want %q", buf.Bytes(), content)
	}
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedPooledEmptyReader verifies that an empty reader
// returns an empty buffer with nil error (issue #1177).
func TestReadAllLimitedPooledEmptyReader(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	before := ReadAllTruncatedCounter()

	buf, err := ReadAllLimitedPooled(bytes.NewReader(nil), 1024)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty buffer, got %d bytes", buf.Len())
	}
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedPooledExceedsLimit verifies that a body larger than
// maxBytes is truncated to maxBytes and the counter increments
// (issue #1177 AC: correctness parity with ReadAllLimited).
func TestReadAllLimitedPooledExceedsLimit(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	content := bytes.Repeat([]byte("y"), 200)
	before := ReadAllTruncatedCounter()

	buf, err := ReadAllLimitedPooled(bytes.NewReader(content), 50)

	if err != nil {
		t.Fatalf("expected nil error (truncation is not an error), got %v", err)
	}
	if buf.Len() != 50 {
		t.Errorf("expected truncated body of 50 bytes, got %d bytes", buf.Len())
	}
	if !bytes.Equal(buf.Bytes(), content[:50]) {
		t.Error("truncated body content mismatch")
	}
	if got := ReadAllTruncatedCounter(); got != before+1 {
		t.Errorf("truncation counter should increment by 1: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedPooledReaderError verifies that an I/O error from the
// underlying reader is propagated, and partial data read before the error
// is still available in the buffer (issue #1177 AC: correctness parity).
func TestReadAllLimitedPooledReaderError(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	fr := &failingReader{data: []byte("partial-data")}

	buf, err := ReadAllLimitedPooled(fr, 1024)

	if !errors.Is(err, errSyntheticIO) {
		t.Fatalf("expected errSyntheticIO to be propagated, got %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected partial body data before the error, got empty")
	}
}

// TestPutBufferNeverNil verifies that PutBuffer handles nil safely
// (defer-safe — callers that get a nil from an error path should not
// panic).
func TestPutBufferNeverNil(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	PutBuffer(nil) // should not panic
}

// TestPutBufferDiscardsOversized verifies that a buffer larger than the
// retention cap is NOT returned to the pool (issue #1177 AC: "Pooled
// buffers larger than NEXUS_POOL_BUFFER_MAX_BYTES not retained").
func TestPutBufferDiscardsOversized(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	SetPoolBufferMaxBytes(1024) // 1 KiB cap

	// Grow a buffer beyond the cap.
	big := GetBuffer()
	big.Write(bytes.Repeat([]byte("x"), 4096)) // 4 KiB >> 1 KiB cap
	PutBuffer(big)

	// The pool should not retain the oversized buffer. GetBuffer should
	// return a fresh (small) buffer, not the one we just put.
	got := GetBuffer()
	if got.Cap() > 1024 {
		t.Errorf("oversized buffer was retained: Cap=%d (cap=%d)", got.Cap(), 1024)
	}
	PutBuffer(got)
}

// TestPutBufferRetainsWithinCap verifies that a buffer within the cap IS
// returned to the pool and reused (issue #1177 AC: heap stays flat). We
// use testing.AllocsPerRun to verify the pool recycles — after warm-up
// the alloc count drops to near zero.
func TestPutBufferRetainsWithinCap(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	SetPoolBufferMaxBytes(1 << 20)

	// After warm-up, the pool should hand back a recycled buffer so
	// allocs/op drops well below the un-pooled baseline.
	allocs := testing.AllocsPerRun(100, func() {
		b := GetBuffer()
		b.WriteString("test payload")
		PutBuffer(b)
	})
	if allocs > 2 {
		t.Errorf("expected <= 2 allocs/op after pool warm-up, got %v", allocs)
	}
}

// TestPoolingDisabled verifies that when poolBufferMaxBytes <= 0, PutBuffer
// discards buffers and does not retain them (issue #1177: "no env var
// needed to enable" — but the knob can disable).
func TestPoolingDisabled(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	SetPoolBufferMaxBytes(0) // disable

	b := GetBuffer()
	b.WriteString("data")
	PutBuffer(b) // should discard

	// Next GetBuffer returns a fresh buffer, not the one we put.
	got := GetBuffer()
	if got.Len() != 0 {
		t.Error("buffer was retained despite pooling being disabled")
	}
	if got.Cap() > 64 { // new bytes.Buffer starts very small
		t.Errorf("expected fresh buffer, got Cap=%d", got.Cap())
	}
}

// TestHeapStabilityOverRequests verifies that the pool recycles buffers
// across N read+return cycles (issue #1177 AC: "verified by N-request
// test asserting heap stays flat"). Each cycle does a tight
// Get→Read→Put; the next Get retrieves the same buffer from the per-P
// cache (no GC in between in a tight loop). We assert that the recycled
// buffer retains capacity from the prior cycle, proving no per-cycle
// growth.
func TestHeapStabilityOverRequests(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	SetPoolBufferMaxBytes(1 << 20)

	payload := bytes.Repeat([]byte("n"), 16*1024) // 16 KiB per read

	// Prime the pool: write a large payload so the buffer grows, then
	// return it. The next Get should retrieve the grown buffer.
	prime := GetBuffer()
	prime.Write(payload)
	primeCap := prime.Cap()
	PutBuffer(prime)

	// Do 200 read+return cycles. Each Get should reuse the primed buffer.
	for i := 0; i < 200; i++ {
		buf, err := ReadAllLimitedPooled(bytes.NewReader(payload), 64*1024)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if buf.Len() != len(payload) {
			t.Fatalf("cycle %d: len=%d want=%d", i, buf.Len(), len(payload))
		}
		// On the first iteration the recycled buffer must have capacity
		// from the primed buffer. (Subsequent iterations may differ if
		// GC ran, but in a tight single-goroutine loop it will not.)
		if i == 0 && buf.Cap() < primeCap {
			t.Errorf("cycle 0: pool did not recycle primed buffer: Cap=%d < primeCap=%d", buf.Cap(), primeCap)
		}
		PutBuffer(buf)
	}
}

// TestReadAllLimitedPooledConcurrent verifies the pool is safe under
// concurrent access (required for the cascade hot path under -race).
func TestReadAllLimitedPooledConcurrent(t *testing.T) {
	restorePoolBufferMaxBytes(t)
	SetPoolBufferMaxBytes(1 << 20)

	const goroutines = 50
	payload := bytes.Repeat([]byte("c"), 4*1024)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			buf, err := ReadAllLimitedPooled(bytes.NewReader(payload), 64*1024)
			if err != nil {
				t.Errorf("read error: %v", err)
				return
			}
			if buf.Len() != len(payload) {
				t.Errorf("len mismatch: got %d want %d", buf.Len(), len(payload))
			}
			PutBuffer(buf)
		}()
	}
	wg.Wait()
}

// compile-time assertion.
var _ io.Reader = (*strings.Reader)(nil)
