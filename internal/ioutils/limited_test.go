package ioutils

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// errSyntheticIO is returned by the failing reader in TestReadAllLimitedReaderError
// to simulate a mid-stream I/O failure (issue #1140 AC: error propagation).
var errSyntheticIO = errors.New("synthetic I/O error")

// failingReader returns the first chunk of data on the first Read call,
// then returns errSyntheticIO on every subsequent Read. This simulates an
// upstream connection that drops mid-response.
type failingReader struct {
	data      []byte
	delivered bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.delivered {
		f.delivered = true
		n := copy(p, f.data)
		return n, nil
	}
	return 0, errSyntheticIO
}

// TestReadAllLimitedUnderLimit verifies that a body smaller than maxBytes is
// returned in full with a nil error and does NOT increment the truncation
// counter (issue #1140 AC: content under limit).
func TestReadAllLimitedUnderLimit(t *testing.T) {
	content := []byte("hello world")
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(bytes.NewReader(content), 1024)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !bytes.Equal(body, content) {
		t.Errorf("body mismatch: got %q, want %q", body, content)
	}
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedEmptyReader verifies that an empty reader returns an empty
// body with a nil error (issue #1140 AC: empty reader).
func TestReadAllLimitedEmptyReader(t *testing.T) {
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(bytes.NewReader(nil), 1024)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(body))
	}
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedAtLimit verifies the boundary case where the body is
// exactly maxBytes. Because len(body) >= maxBytes is true, the truncation
// counter is incremented even though no actual data was lost — this is a
// documented edge case (issue #1140 AC: boundary case).
func TestReadAllLimitedAtLimit(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 100)
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(bytes.NewReader(content), 100)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(body) != 100 {
		t.Errorf("expected 100 bytes, got %d", len(body))
	}
	if !bytes.Equal(body, content) {
		t.Error("body content mismatch at limit boundary")
	}
	if got := ReadAllTruncatedCounter(); got != before+1 {
		t.Errorf("truncation counter should increment by 1: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedExceedsLimit verifies that a body larger than maxBytes is
// truncated to maxBytes. The error is nil (LimitReader+ReadAll treat the
// truncation as a clean EOF), but the truncation counter is incremented
// (issue #1140 AC: content exceeds limit).
func TestReadAllLimitedExceedsLimit(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 200)
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(bytes.NewReader(content), 50)

	if err != nil {
		t.Fatalf("expected nil error (truncation is not an error), got %v", err)
	}
	if len(body) != 50 {
		t.Errorf("expected truncated body of 50 bytes, got %d bytes", len(body))
	}
	if !bytes.Equal(body, content[:50]) {
		t.Error("truncated body content mismatch")
	}
	if got := ReadAllTruncatedCounter(); got != before+1 {
		t.Errorf("truncation counter should increment by 1: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedReaderError verifies that an I/O error from the underlying
// reader is propagated to the caller rather than being silently swallowed
// (issue #1140 AC: error propagation — directly relevant to issue #1114
// where ReadAllLimited errors were discarded).
func TestReadAllLimitedReaderError(t *testing.T) {
	fr := &failingReader{data: []byte("partial-data")}
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(fr, 1024)

	if !errors.Is(err, errSyntheticIO) {
		t.Fatalf("expected errSyntheticIO to be propagated, got %v", err)
	}
	// The partial data read before the error should still be returned.
	if len(body) == 0 {
		t.Error("expected partial body data before the error, got empty")
	}
	// Because len(body) < maxBytes, the counter must NOT increment.
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change on read error: before=%d, got=%d", before, got)
	}
}

// TestReadAllLimitedImmediateReaderError verifies that a reader which fails
// on the very first Read call propagates the error with an empty body.
func TestReadAllLimitedImmediateReaderError(t *testing.T) {
	fr := &failingReader{} // no data → first Read returns the error
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(fr, 1024)

	if !errors.Is(err, errSyntheticIO) {
		t.Fatalf("expected errSyntheticIO, got %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body on immediate error, got %d bytes", len(body))
	}
	if got := ReadAllTruncatedCounter(); got != before {
		t.Errorf("truncation counter should not change: before=%d, got=%d", before, got)
	}
}

// TestTruncationCounterFunctions verifies that IncrementTruncationCounter and
// ReadAllTruncatedCounter work as a matched pair, exercising the atomic
// operations directly (covers the two exported helper functions that
// ReadAllLimited delegates to).
func TestTruncationCounterFunctions(t *testing.T) {
	before := ReadAllTruncatedCounter()

	IncrementTruncationCounter()
	IncrementTruncationCounter()
	IncrementTruncationCounter()

	after := ReadAllTruncatedCounter()
	if delta := after - before; delta != 3 {
		t.Errorf("expected counter to increase by 3, got delta=%d (before=%d, after=%d)", delta, before, after)
	}
}

// TestReadAllLimitedZeroMaxBytes is an edge case: with maxBytes=0, LimitReader
// reads nothing and ReadAll returns an empty body with nil error. Because
// len(body) >= 0 is always true, the truncation counter increments.
func TestReadAllLimitedZeroMaxBytes(t *testing.T) {
	content := []byte("non-empty content that should be truncated to zero")
	before := ReadAllTruncatedCounter()

	body, err := ReadAllLimited(bytes.NewReader(content), 0)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body with maxBytes=0, got %d bytes", len(body))
	}
	if got := ReadAllTruncatedCounter(); got != before+1 {
		t.Errorf("truncation counter should increment: before=%d, got=%d", before, got)
	}
}

// Compile-time assertion that bytes.Reader and *failingReader satisfy io.Reader.
var (
	_ io.Reader = (*bytes.Reader)(nil)
	_ io.Reader = (*failingReader)(nil)
)
