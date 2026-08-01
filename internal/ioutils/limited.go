// Package ioutils provides io helpers that avoid import cycles with the
// observability and upstream packages.
package ioutils

import (
	"bytes"
	"io"
	"sync"
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

// defaultPoolBufferMaxBytes is the default retention cap for pooled
// buffers. Buffers that grew beyond this capacity are discarded rather
// than returned to the pool. 1 MiB is generous for typical multi-KiB
// completions while preventing a single 64 MiB response from pinning
// pool memory (issue #1177).
const defaultPoolBufferMaxBytes = 1 << 20 // 1 MiB

// poolBufferMaxBytes is the maximum capacity a buffer may retain to be
// returned to the pool. Set via SetPoolBufferMaxBytes from config at
// boot. A value <= 0 disables pooling entirely — GetBuffer still
// allocates, but PutBuffer discards instead of returning to the pool.
var poolBufferMaxBytes atomic.Int64

func init() {
	poolBufferMaxBytes.Store(defaultPoolBufferMaxBytes)
}

// bufferPool recycles *bytes.Buffer instances to reduce GC pressure on
// the cascade hot path (issue #1177). Buffers larger than
// poolBufferMaxBytes are discarded rather than returned to the pool so
// a single huge response never pins pool memory.
var bufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// SetPoolBufferMaxBytes configures the maximum retained buffer capacity
// for the response-body pool (issue #1177). Called once at boot from
// buildServer with the value from config.Load(). Values <= 0 disable
// pooling — GetBuffer still allocates fresh buffers but PutBuffer
// discards them instead of returning to the pool.
func SetPoolBufferMaxBytes(n int) {
	poolBufferMaxBytes.Store(int64(n))
}

// PoolBufferMaxBytes returns the current retention cap. Primarily for
// testing and diagnostics.
func PoolBufferMaxBytes() int {
	return int(poolBufferMaxBytes.Load())
}

// GetBuffer acquires a *bytes.Buffer from the pool. The returned buffer
// is empty (Len==0) and ready for use. When pooling is disabled (cap
// <= 0) a fresh buffer is still allocated; the caller simply should not
// call PutBuffer on it.
func GetBuffer() *bytes.Buffer {
	if v := bufferPool.Get(); v != nil {
		return v.(*bytes.Buffer)
	}
	return new(bytes.Buffer)
}

// PutBuffer returns a buffer to the pool for reuse. If the buffer's
// capacity exceeds the retention cap it is discarded to avoid pinning
// memory from a single large response. nil and disabled-pool (cap <= 0)
// inputs are handled safely as no-ops.
func PutBuffer(b *bytes.Buffer) {
	if b == nil {
		return
	}
	max := poolBufferMaxBytes.Load()
	if max <= 0 {
		return
	}
	if int64(b.Cap()) > max {
		return
	}
	b.Reset()
	bufferPool.Put(b)
}

// ReadAllLimitedPooled reads from r into a pooled *bytes.Buffer with a
// byte limit of maxBytes. The caller MUST call PutBuffer on the returned
// buffer when done — use defer to guarantee it on every code path. If
// the response body is larger than maxBytes, the body is truncated and
// IncrementTruncationCounter is called. This is the pooled equivalent of
// ReadAllLimited: it recycles the underlying buffer across calls to
// reduce GC pressure on the hot path (issue #1177). The returned buffer
// is never nil; on read error it contains whatever partial data was
// read before the error.
func ReadAllLimitedPooled(r io.Reader, maxBytes int) (*bytes.Buffer, error) {
	buf := GetBuffer()
	lr := io.LimitReader(r, int64(maxBytes))
	_, err := io.Copy(buf, lr)
	if buf.Len() >= maxBytes {
		IncrementTruncationCounter()
	}
	return buf, err
}
