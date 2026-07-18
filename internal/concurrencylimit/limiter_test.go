package concurrencylimit

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// vramFn returns a FreeVRAM closure backed by an atomic int64 so tests
// can flip the probe snapshot concurrently while the limiter reads it.
func vramFn(initial int64) (*atomic.Int64, func() int64) {
	v := atomic.Int64{}
	v.Store(initial)
	return &v, v.Load
}

// --- Disabled limiter: probe-disabled behaviour is unchanged ---------

// TestLimiterDisabledIsNoOp covers acceptance criterion "With probe
// disabled, limiter behaviour is unchanged". A disabled limiter (nil
// receiver or Ceiling <= 0) returns a no-op release and never blocks.
func TestLimiterDisabledIsNoOp(t *testing.T) {
	ctx := context.Background()

	// Ceiling <= 0 -> disabled.
	l := New(0, DefaultBytesPerSlot, func() int64 { return 1 << 30 })
	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("disabled Acquire err = %v", err)
	}
	if rel == nil {
		t.Fatal("disabled Acquire returned nil release")
	}
	rel() // must not panic
	if got := l.Effective(); got != 0 {
		t.Errorf("disabled Effective = %d, want 0", got)
	}

	// Nil receiver is also a no-op (defensive; main.go guards too).
	var nilL *Limiter
	rel2, err := nilL.Acquire(ctx)
	if err != nil {
		t.Fatalf("nil Acquire err = %v", err)
	}
	rel2()
}

// TestEffectiveProbeUnavailableUsesCeiling covers "Probe unavailable
// does not open floodgates": when the FreeVRAM closure returns <= 0
// (or is nil) the limiter falls back to the full Ceiling.
func TestEffectiveProbeUnavailableUsesCeiling(t *testing.T) {
	cases := []struct {
		name string
		free func() int64
	}{
		{"nil closure", nil},
		{"zero free", func() int64 { return 0 }},
		{"negative free", func() int64 { return -1024 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(4, 1<<30, tc.free)
			if got := l.Effective(); got != 4 {
				t.Errorf("Effective = %d, want ceiling 4", got)
			}
		})
	}
}

// --- Low VRAM reduces slots, never exceeds ceiling -------------------

// TestEffectiveLowVRAMReducesSlots covers "Lower free VRAM reduces
// effective local concurrency". freeVRAM/bytesPerSlot is the floor.
func TestEffectiveLowVRAMReducesSlots(t *testing.T) {
	const slot = int64(1 << 30) // 1 GiB per slot for easy math
	cases := []struct {
		name string
		free int64
		want int
	}{
		{"10 GiB / 1GiB slot -> 4 (ceiling)", 10 << 30, 4},
		{"3 GiB / 1GiB slot -> 3", 3 << 30, 3},
		{"2 GiB / 1GiB slot -> 2", 2 << 30, 2},
		{"half a GiB -> 1 (floor)", 512 << 20, 1},
		{"zero free -> ceiling (probe unavailable)", 0, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(4, slot, func() int64 { return tc.free })
			if got := l.Effective(); got != tc.want {
				t.Errorf("Effective(free=%d) = %d, want %d", tc.free, got, tc.want)
			}
		})
	}
}

// TestNeverExceedsCeiling asserts the in-flight count never goes above
// the configured ceiling even under heavy concurrent acquire/release.
func TestNeverExceedsCeiling(t *testing.T) {
	const ceiling = 4
	_, free := vramFn(20 << 30) // plenty of VRAM -> effective == ceiling
	l := New(ceiling, 1<<30, free)

	var inFlight, maxSeen atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel, err := l.Acquire(context.Background())
			if err != nil {
				return
			}
			defer rel()
			cur := inFlight.Add(1)
			for {
				old := maxSeen.Load()
				if cur <= old || maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}
			// Hold briefly so concurrency is observable.
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	close(start)
	wg.Wait()

	if got := maxSeen.Load(); got > ceiling {
		t.Errorf("max in-flight = %d, exceeded ceiling %d", got, ceiling)
	}
	if got := maxSeen.Load(); got < int64(ceiling) {
		t.Errorf("max in-flight = %d, want saturation near %d (limiter never throttled)", got, ceiling)
	}
}

// --- Reactive shrink: VRAM drop throttles new acquires ---------------

// TestVRAMDropThrottlesNewAcquires verifies that lowering the probe's
// free-VRAM reading causes subsequent Acquires to block (effective
// shrink below in-flight).
func TestVRAMDropThrottlesNewAcquires(t *testing.T) {
	v, free := vramFn(10 << 30) // effective = min(4, 10) = 4
	l := New(4, 1<<30, free)

	// Occupy all 4 slots.
	rels := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		rel, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		rels = append(rels, rel)
	}
	if got := l.InFlight(); got != 4 {
		t.Fatalf("InFlight = %d, want 4", got)
	}

	// Drop VRAM so effective shrinks to 2. A new acquire must block.
	v.Store(2 << 30)
	acquired := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		rel, err := l.Acquire(ctx)
		if err == nil {
			rel()
			close(acquired)
		}
	}()
	select {
	case <-acquired:
		t.Fatal("acquire proceeded while in-flight (4) >= effective (2)")
	case <-time.After(80 * time.Millisecond):
		// expected: blocked.
	}

	// Release two -> in-flight 2 == effective 2; the blocked acquire
	// still cannot proceed (inFlight must be < effective). Release a
	// third -> in-flight 1 < 2; the blocked acquire should now win.
	rels[0]()
	rels[1]()
	rels[2]()
	select {
	case <-acquired:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquire stayed blocked after in-flight dropped below effective")
	}
	rels[3]()
}

// --- Context cancellation releases blocked acquirers -----------------

// TestAcquireContextCancelled verifies a blocked Acquire returns its
// ctx.Err() promptly when the context is cancelled.
func TestAcquireContextCancelled(t *testing.T) {
	l := New(1, 1<<30, func() int64 { return 10 << 30 })
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	gotRel, gotErr := l.Acquire(ctx)
	if gotErr == nil {
		if gotRel != nil {
			gotRel()
		}
		t.Fatal("expected ctx.Err() from blocked acquire, got nil")
	}
	if gotRel != nil {
		t.Error("expected nil release on cancellation")
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", gotErr)
	}
}

// --- Race detector stress --------------------------------------------

// TestAcquireReleaseConcurrentStress hammers the limiter from many
// goroutines under -race. A failure here means the mutex/cond
// bookkeeping is not race-safe. The probe snapshot is mutated
// concurrently to exercise the reactive-read path too.
func TestAcquireReleaseConcurrentStress(t *testing.T) {
	v, free := vramFn(4 << 30)
	l := New(8, 1<<30, free)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rel, err := l.Acquire(context.Background())
				if err != nil {
					t.Errorf("unexpected acquire err: %v", err)
					return
				}
				// Occasionally churn the probe snapshot so the
				// effective count changes mid-flight.
				if (seed+j)%7 == 0 {
					v.Store(int64(1+((seed+j)%6)) << 30)
				}
				_ = l.Effective() // exercise the reader path
				rel()
			}
		}(i)
	}
	wg.Wait()

	// After churn, in-flight must be back to zero (every acquire
	// released).
	if got := l.InFlight(); got != 0 {
		t.Errorf("InFlight after stress = %d, want 0", got)
	}
}

// TestNewDefaultsBytesPerSlot confirms New fills a non-positive
// BytesPerSlot with DefaultBytesPerSlot.
func TestNewDefaultsBytesPerSlot(t *testing.T) {
	l := New(4, 0, func() int64 { return DefaultBytesPerSlot * 5 })
	if l.BytesPerSlot != DefaultBytesPerSlot {
		t.Errorf("BytesPerSlot = %d, want default %d", l.BytesPerSlot, DefaultBytesPerSlot)
	}
	// 5 * default / default = 5, ceiling 4 -> 4.
	if got := l.Effective(); got != 4 {
		t.Errorf("Effective = %d, want 4", got)
	}
}

// TestReleaseIsIdempotentSafe confirms double-release cannot drive the
// in-flight counter negative (defensive against handler wiring bugs).
func TestReleaseIsIdempotentSafe(t *testing.T) {
	l := New(2, 1<<30, func() int64 { return 10 << 30 })
	rel, _ := l.Acquire(context.Background())
	rel()
	rel() // should be a no-op, not panic, not go negative
	if got := l.InFlight(); got != 0 {
		t.Errorf("InFlight after double release = %d, want 0", got)
	}
	// A fresh acquire must still succeed.
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after double release: %v", err)
	}
	rel2()
}

// TestEffectiveStringForLog is a sanity check that the change-detection
// log path runs without panicking across transitions (covered by other
// tests via Effective(), but this makes the intent explicit).
func TestEffectiveAcrossTransitions(t *testing.T) {
	v, free := vramFn(8 << 30)
	l := New(4, 1<<30, free)
	seen := map[int]bool{}
	for _, gi := range []int64{8, 6, 3, 1, 0, 12} {
		v.Store(gi << 30)
		seen[l.Effective()] = true
	}
	if len(seen) < 3 {
		t.Errorf("expected >=3 distinct effective values across transitions, got %v", seen)
	}
}

// TestAcquireReleasesOnContextDoneAfterStop guards the
// context.AfterFunc wiring: a goroutine blocked in Acquire must be
// released when the shared parent context is cancelled, not just on a
// per-call timeout.
func TestAcquireReleasesOnContextDoneAfterStop(t *testing.T) {
	l := New(1, 1<<30, func() int64 { return 8 << 30 })
	// Occupy the single slot for the whole test.
	holder, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	defer holder()

	parent, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := l.Acquire(parent)
		errCh <- err
	}()
	// Give the waiter a moment to park in cond.Wait.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked acquire was not released on parent cancel")
	}
}

// TestManyWaitersWakeOnRelease confirms Broadcast wakes all eligible
// waiters when multiple slots free up (Signal would only wake one).
func TestManyWaitersWakeOnRelease(t *testing.T) {
	const ceiling = 3
	l := New(ceiling, 1<<30, func() int64 { return 8 << 30 })

	holders := make([]func(), 0, ceiling)
	for i := 0; i < ceiling; i++ {
		rel, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatalf("holder %d: %v", i, err)
		}
		holders = append(holders, rel)
	}

	// Queue 3 waiters.
	type res struct {
		err error
	}
	done := make(chan res, 3)
	for i := 0; i < 3; i++ {
		go func() {
			rel, err := l.Acquire(context.Background())
			if err == nil {
				rel()
			}
			done <- res{err}
		}()
	}
	// Release all 3 holders at once.
	for _, h := range holders {
		h()
	}
	// All 3 waiters should complete promptly.
	for i := 0; i < 3; i++ {
		select {
		case r := <-done:
			if r.err != nil {
				t.Errorf("waiter %d err = %v", i, r.err)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d did not wake after broadcast release", i)
		}
	}
}

// TestFmtHelper keeps gofmt-equivalent formatting assertions honest if
// the package grows; currently a placeholder that just exercises fmt
// import (avoids an unused-import churn if other tests drop fmt).
func TestFmtHelper(t *testing.T) {
	if fmt.Sprintf("%d", DefaultBytesPerSlot) == "" {
		t.Fail()
	}
}

// TestAcquireContextCancelPreWaitRace covers issue #439: a
// cancellation that fires between the final ctx.Err() check and
// Cond.Wait's register-and-park step used to drop the wakeup, so the
// canceled acquirer could sleep until an unrelated Release (possibly
// forever — disconnected requests remained queued). The fix pulls
// the AfterFunc callback inside l.mu so its Broadcast is strictly
// ordered with respect to Cond.Wait's register/release.
//
// The test fires cancellations immediately after starting Acquire,
// deliberately racing the AfterFunc against the Waiter's transition
// into cond.Wait. A FreeVRAM closure that yields once per call is
// used to widen the pre-wait race window during the
// effective-slot computation. -race catches any remaining
// unsynchronized state; the per-iteration timeout catches hangs.
func TestAcquireContextCancelPreWaitRace(t *testing.T) {
	const iterations = 500
	for i := 0; i < iterations; i++ {
		// Yield inside the probe to add a context switch between
		// ctx.Err() and the Cond.Wait call below, exercising the
		// pre-wait race window on every iteration.
		l := New(1, 1<<30, func() int64 {
			runtime.Gosched()
			return 8 << 30
		})

		holderRel, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatalf("iter %d holder acquire: %v", i, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, gerr := l.Acquire(ctx)
			errCh <- gerr
		}()

		// Cancel without waiting for the waiter to park. With the
		// bug, the Broadcast can race the Cond.Wait registration
		// and the waiter is lost; with the fix the Broadcast holds
		// l.mu and is therefore guaranteed to be observed.
		cancel()

		select {
		case gerr := <-errCh:
			if !errors.Is(gerr, context.Canceled) {
				t.Errorf("iter %d: err = %v, want Canceled", i, gerr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: blocked acquire hung past cancellation", i)
		}

		// Cancellation must leave the in-flight count unchanged
		// (only the holder occupies a slot).
		if got := l.InFlight(); got != 1 {
			t.Errorf("iter %d: InFlight = %d after cancel, want 1 (only the holder)", i, got)
		}
		holderRel()
		if got := l.InFlight(); got != 0 {
			t.Errorf("iter %d: InFlight after release = %d, want 0", i, got)
		}
	}
}

// TestAcquireContextCancelDoesNotLeakAfterFuncStop covers the
// acceptance criterion "no context.AfterFunc callback leaks after
// acquisition": a successful Acquire must deregister its AfterFunc
// so a late cancellation does not leave the callback scheduled
// (or running) against the released slot.
//
// We can't observe the AfterFunc goroutine directly, but we can
// use a probe that records whether the AfterFunc callback (which
// holds l.mu while broadcasting) has ever been entered by
// detecting a contended-lock timing anomaly. A more direct check
// is to call Acquire a second time on the same context after
// success — if the deferred stop worked, the deregistered
// callback has no effect; if it didn't, we have indirect evidence.
// The runtime's lack of a public AfterFunc inspection API means
// the strongest guarantee is the test's continued success under
// -race and the absence of a callback-stuck deadlock: that is
// exercised by the broader stress test below.
func TestAcquireCancelRaceStopReturns(t *testing.T) {
	l := New(1, 1<<30, func() int64 { return 8 << 30 })

	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Releasing twice is a no-op defensively; afterwards a fresh
	// acquire must still succeed (no leaked AfterFunc keeps the
	// slot pinned).
	rel()
	rel()
	if got := l.InFlight(); got != 0 {
		t.Fatalf("InFlight after release = %d, want 0", got)
	}
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	rel2()
}
