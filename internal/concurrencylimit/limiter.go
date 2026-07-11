// Package concurrencylimit provides a bounded semaphore that gates
// in-flight local-route dispatches (issue #35). The chat handler holds
// a slot for the entire lifetime of any RouteLocal request (or the
// local panel member of RouteFusion) so concurrent coding agents
// cannot collectively exhaust VRAM and crash Ollama. Waiters
// queue-and-wait up to QueueTimeout, after which the chat handler
// fast-promotes the request to the frontier cascade and stamps
// X-Nexus-Overflow: true on the response.
//
// stdlib-only by design: the rest of the proxy does not pull in third
// party modules and this package sits on the chat hot path's import
// graph.
package concurrencylimit

import (
	"context"
	"time"
)

// Limiter is a bounded counting semaphore. The zero value is invalid;
// always construct via New.
//
// A nil Limiter is treated as "disabled" everywhere it is referenced,
// so the chat handler can leave Deps.Limiter unset (preserving the
// pre-issue-#35 unbounded behaviour) and tests can opt in per-case
// without sprinkling nil-checks down the stack.
//
// Internally: a buffered channel of size max acts as the slot pool.
// Each successful Acquire places one token; each Release drains one.
// The channel approach is stdlib-only, lock-free after the channel
// send/receive race, and pairs well with the chat handler's
// per-request goroutine model (issue #35 was tracked as a per-request
// resource ceiling, not a background worker pool).
type Limiter struct {
	sem chan struct{}
}

// New returns a Limiter that admits at most max concurrent holders.
// max <= 0 returns nil so the caller can treat "disabled" uniformly
// via a single nil-check at the use site. This matches the issue #35
// acceptance criterion: NEXUS_LOCAL_MAX_CONCURRENT=0 disables the
// limiter entirely.
//
// Negative values are folded into the same nil return; operators who
// fat-finger a sign get the safer "off" behaviour rather than a
// reverse-direction semaphore.
func New(max int) *Limiter {
	if max <= 0 {
		return nil
	}
	return &Limiter{sem: make(chan struct{}, max)}
}

// Acquire blocks until a slot becomes available, timeout elapses, or
// ctx is cancelled. Returns true when a slot is granted; false on
// timeout or cancellation.
//
// A nil receiver returns true unconditionally so call sites do not
// have to nil-check before invoking Acquire. timeout == 0 behaves as
// "try once and give up immediately" — no goroutine ever waits;
// timeout < 0 is treated like 0.
func (l *Limiter) Acquire(ctx context.Context, timeout time.Duration) bool {
	if l == nil || l.sem == nil {
		return true
	}
	// Fast path: a slot is immediately available. The send
	// competes with concurrent Release calls; if it loses we fall
	// through to the timed path below rather than retrying inline
	// (would risk a tight loop when many goroutines race).
	select {
	case l.sem <- struct{}{}:
		return true
	default:
	}
	if timeout <= 0 {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case l.sem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Release returns a slot to the pool. Idempotent and non-blocking:
// calling Release without a matching Acquire is a no-op (we drain at
// most one token from the channel), and calling Release on a nil
// receiver is also a no-op so callers may `defer l.Release()`
// unconditionally without an explicit guard. Double-release never
// over-credits the pool — the channel is bounded by max so each send
// in Acquire corresponds to exactly one successful Release.
func (l *Limiter) Release() {
	if l == nil || l.sem == nil {
		return
	}
	select {
	case <-l.sem:
	default:
		// Either we never held a slot (caller bug) or Release
		// has already been called. Stay non-blocking; the buffer
		// length is the ground truth for the in-flight count, so
		// the worst we can do here is silently drop a redundant
		// signal.
	}
}

// Max returns the configured in-flight ceiling. Returns 0 for a nil
// receiver so observability / logging code can print "0 == disabled"
// without a nil-check. Exported so chat.go can echo the configured
// value into log lines on the overflow path.
func (l *Limiter) Max() int {
	if l == nil || l.sem == nil {
		return 0
	}
	return cap(l.sem)
}
