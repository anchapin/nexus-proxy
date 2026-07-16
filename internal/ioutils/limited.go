// Package ioutils provides io helpers that avoid import cycles with the
// observability and upstream packages.
package ioutils

import (
	"io"
	"sync/atomic"
)

// responseTruncated is a package-level counter for truncated upstream
// responses. It is atomically incremented when ReadAllLimited truncates
// a response that exceeded MaxResponseBytes (issue #365).
var responseTruncated atomic.Uint64

// IncrementTruncationCounter increments the response truncation counter.
func IncrementTruncationCounter() {
	responseTruncated.Add(1)
}

// ReadAllTruncatedCounter returns the current truncation count.
func ReadAllTruncatedCounter() uint64 {
	return responseTruncated.Load()
}

// ReadAllLimited reads from r with a byte limit of maxBytes. If the
// response body is larger than maxBytes, the body is truncated and
// IncrementTruncationCounter is called. This prevents memory exhaustion
// from a malicious upstream returning gigabytes. The returned error is
// any read error encountered before hitting the limit; a truncation
// itself is not treated as an error.
func ReadAllLimited(r io.Reader, maxBytes int) ([]byte, error) {
	lr := io.LimitReader(r, int64(maxBytes))
	body, err := io.ReadAll(lr)
	// If we read exactly maxBytes, the response was likely truncated.
	// The edge case of a response that is exactly maxBytes is
	// astronomically unlikely at 64 MiB.
	if len(body) >= maxBytes {
		IncrementTruncationCounter()
	}
	return body, err
}
