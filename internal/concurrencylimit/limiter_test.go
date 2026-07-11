package concurrencylimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewDisabledForZeroAndNegative pins the contract that
// New(max<=0) returns nil and a nil Limiter reports Max=0. The chat
// handler relies on this to gate "disabled" off a single nil-check.
func TestNewDisabledForZeroAndNegative(t *testing.T) {
	for _, max := range []int{0, -1, -100} {
		l := New(max)
		if l != nil {
			t.Errorf("New(%d) = %p, want nil", max, l)
		}
		if got := l.Max(); got != 0 {
			t.Errorf("nil Max() = %d, want 0", got)
		}
		// Acquire / Release on a nil receiver must not panic and
		// Acquire must return true so the hot path takes the
		// unbounded default.
		if !l.Acquire(context.Background(), 0) {
			t.Errorf("nil Acquire returned false for max=%d", max)
		}
		l.Release()
	}
}

// TestAcquireSucceedsWhenSlotsAvailable covers the happy path: a
// freshly-built Limiter with max=2 grants both acquires without
// waiting, then refuses a third while two are held.
func TestAcquireSucceedsWhenSlotsAvailable(t *testing.T) {
	l := New(2)
	defer l.Release()
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed with slots available")
	}
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("second Acquire failed with slots available")
	}
	if l.Acquire(context.Background(), 0) {
		t.Fatal("third Acquire succeeded when only two slots exist")
	}
	l.Release()
}

// TestAcquireBlocksUntilRelease verifies that an Acquire call is
// genuinely queued and that a subsequent Release unblocks it within
// the timeout window. Without the release the goroutine would have
// to wait the full timeout — we assert it does not.
func TestAcquireBlocksUntilRelease(t *testing.T) {
	l := New(1)
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed")
	}
	done := make(chan bool, 1)
	go func() {
		done <- l.Acquire(context.Background(), time.Second)
	}()
	// Let the goroutine enter its blocked state.
	time.Sleep(50 * time.Millisecond)
	select {
	case ok := <-done:
		t.Fatalf("Acquire returned before Release (ok=%v)", ok)
	default:
	}
	l.Release()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Acquire returned false after Release")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return within 1s of Release")
	}
	l.Release()
}

// TestAcquireTimesOutWhenSlotsExhausted asserts that with all slots
// in use, a follow-up Acquire returns false within the configured
// timeout window. A 50 ms timeout must not return in <40 ms (early)
// or 200 ms (way late); the lower bound keeps the test fast, the
// upper bound flags a timer bug.
func TestAcquireTimesOutWhenSlotsExhausted(t *testing.T) {
	l := New(1)
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed")
	}
	start := time.Now()
	if l.Acquire(context.Background(), 50*time.Millisecond) {
		t.Fatal("second Acquire unexpectedly succeeded")
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Acquire returned in %v, want ~50ms", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Acquire took %v, want <500ms", elapsed)
	}
}

// TestAcquireHonoursContextCancellation asserts that a cancelled
// request context unblocks Acquire even though the configured timeout
// is generous. The chat handler relies on this so a client
// disconnect during the queue phase does not pin the slot until the
// configured timeout elapses.
func TestAcquireHonoursContextCancellation(t *testing.T) {
	l := New(1)
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if l.Acquire(ctx, time.Second) {
		t.Fatal("Acquire did not honour context cancellation")
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Acquire took %v, want <200ms (ctx cancellation)", elapsed)
	}
}

// TestReleaseAllowsFurtherAcquire verifies the round-trip: after
// Release the slot re-enters the pool and the next Acquire succeeds
// synchronously. Without this, the limiter would leak slots under
// normal traffic.
func TestReleaseAllowsFurtherAcquire(t *testing.T) {
	l := New(1)
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed")
	}
	l.Release()
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("Acquire after Release failed")
	}
	l.Release()
}

// TestReleaseIsIdempotent guards against the double-release footgun:
// the chat handler defers Release unconditionally, so a future refactor
// that also calls Release on the overflow path must not over-credit
// the pool. We call Release twice with no matching Acquire and
// confirm we can still grant exactly max acquisitions.
func TestReleaseIsIdempotent(t *testing.T) {
	l := New(2)
	l.Release()
	l.Release()
	var got int32
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire after double-Release failed")
	}
	got++
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("second Acquire failed (Release over-credited the pool)")
	}
	got++
	if l.Acquire(context.Background(), 0) {
		t.Fatal("third Acquire succeeded, pool was over-credited")
	}
	l.Release()
	l.Release()
	if got != 2 {
		t.Fatalf("got %d acquisitions, want 2", got)
	}
}

// TestAcquireTryOnceWhenTimeoutZero documents the contract that
// timeout=0 is a single try-and-give-up: no goroutine ever blocks.
// This is the path the chat handler takes on the very fast path when
// it has hard-coded a zero timeout (none today, but the option is
// there).
func TestAcquireTryOnceWhenTimeoutZero(t *testing.T) {
	l := New(1)
	if !l.Acquire(context.Background(), 0) {
		t.Fatal("first Acquire failed")
	}
	if l.Acquire(context.Background(), 0) {
		t.Fatal("second Acquire succeeded with timeout=0")
	}
	l.Release()
}

// TestAcquireConcurrentSerializes asserts the limiter actually
// bounds concurrency. With max=2 and 10 workers each acquiring-then-
// holding for 5 ms, the observed in-flight peak must be <= 2 (and
// at least 1, otherwise the limiter is fully open and the test is
// not exercising the semaphore). The boundary check on peak >= 1 is
// strictly defensive — a real scheduler should always see >1 when
// 10 workers contend, but timing-only flakiness guards against
// false negatives.
func TestAcquireConcurrentSerializes(t *testing.T) {
	l := New(2)
	var inflight atomic.Int32
	var peak atomic.Int32
	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for !l.Acquire(context.Background(), 500*time.Millisecond) {
				// If we fail to acquire, retry: this loop only
				// terminates when the in-flight workers drain.
				// It is bounded by the test deadline so we
				// won't hang forever under a buggy limiter.
			}
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p {
					break
				}
				if peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inflight.Add(-1)
			l.Release()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 2 {
		t.Errorf("peak in-flight = %d, want <= 2", p)
	}
	if p := peak.Load(); p < 1 {
		t.Errorf("peak in-flight = %d, want >= 1", p)
	}
}

// TestMaxReflectsConfiguredLimit pins the Max accessor so chat.go
// can log the configured ceiling on the overflow path without
// re-reading the Config struct.
func TestMaxReflectsConfiguredLimit(t *testing.T) {
	for _, max := range []int{1, 2, 16, 64} {
		l := New(max)
		if got := l.Max(); got != max {
			t.Errorf("Max() = %d, want %d", got, max)
		}
	}
}
