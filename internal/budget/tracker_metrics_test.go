package budget

import (
	"sync"
	"sync/atomic"
	"testing"
)

// captureObserver records every "spent" and "exceeded" event the
// tracker emits. Issue #70 uses the same hook to drive the
// Prometheus collector; tests verify the wiring on the tracker
// side.
type captureObserver struct {
	spentCount    atomic.Uint32
	exceededCount atomic.Uint32
	spentTotal    atomic.Uint64 // bits-trick via float64 encoding not needed; we sum cents-ish
}

func (c *captureObserver) observe(event string, amount float64) {
	switch event {
	case ObserverEventSpent:
		c.spentCount.Add(1)
		c.spentTotal.Add(uint64(amount * 1000)) // 0.001 USD precision, integer-safe
	case ObserverEventExceeded:
		c.exceededCount.Add(1)
	}
}

// TestObserverSpent (issue #70) verifies the tracker fires the
// observer with event="spent" on Record().
func TestObserverSpent(t *testing.T) {
	o := &captureObserver{}
	st := NewSpendTracker(10.0)
	defer st.SetObserver(nil) // defer-nil clear is safe; no-op
	st.SetObserver(o.observe)

	st.Record(1.5)
	st.Record(2.5)

	if got := o.spentCount.Load(); got != 2 {
		t.Errorf("spent events = %d, want 2", got)
	}
	if got := o.exceededCount.Load(); got != 0 {
		t.Errorf("exceeded events = %d, want 0", got)
	}
	// Sum should equal 1.5 + 2.5 = 4.0 USD; we encoded as 0.001 cents × 1000.
	if got := float64(o.spentTotal.Load()) / 1000.0; got != 4.0 {
		t.Errorf("spent sum = %v, want 4.0", got)
	}
}

// TestObserverExceeded (issue #70) verifies the tracker fires the
// observer with event="exceeded" only when WouldExceed returns
// true (not on advisory checks that don't trip).
func TestObserverExceeded(t *testing.T) {
	o := &captureObserver{}
	st := NewSpendTracker(10.0)
	st.SetObserver(o.observe)

	// Under budget → no exceeded event.
	if st.WouldExceed(5.0) {
		t.Fatal("WouldExceed(5.0) = true; want false")
	}
	if got := o.exceededCount.Load(); got != 0 {
		t.Errorf("exceeded fired on under-budget WouldExceed: %d", got)
	}

	// Record enough that the next WouldExceed crosses the cap.
	st.Record(8.0)

	// WouldExceed(3.0) → 8.0 + 3.0 = 11.0 > 10.0 → exceeds.
	if !st.WouldExceed(3.0) {
		t.Fatal("WouldExceed(3.0) = false; want true (8.0+3.0 > 10.0)")
	}
	if got := o.exceededCount.Load(); got != 1 {
		t.Errorf("exceeded events = %d, want 1", got)
	}

	// Subsequent advisory check that still trips must keep firing.
	if !st.WouldExceed(2.5) {
		t.Fatal("WouldExceed(2.5) = false; want true (8.0+2.5 > 10.0)")
	}
	if got := o.exceededCount.Load(); got != 2 {
		t.Errorf("exceeded events = %d, want 2 (every over-budget check fires)", got)
	}
}

// TestObserverNilAmount (issue #70) verifies zero and negative
// amounts produce no observation (matching the existing Record
// guard — observing a no-op would inflate counters without
// representing any real spend).
func TestObserverNilAmount(t *testing.T) {
	o := &captureObserver{}
	st := NewSpendTracker(10.0)
	st.SetObserver(o.observe)

	st.Record(0)
	st.Record(-1.0)
	st.Record(0.0001) // tiny but positive: this DOES fire

	if got := o.spentCount.Load(); got != 1 {
		t.Errorf("spent events = %d, want 1 (only the positive tiny amount)", got)
	}
	if got := o.exceededCount.Load(); got != 0 {
		t.Errorf("exceeded events = %d, want 0", got)
	}
}

// TestObserverNilReceiver (issue #70) verifies SetObserver and the
// Record / WouldExceed methods are all nil-safe — a disabled
// (nil) tracker never invokes observers.
func TestObserverNilReceiver(t *testing.T) {
	o := &captureObserver{}
	var st *SpendTracker

	// Nil receiver + non-nil observer must not panic.
	st.SetObserver(o.observe)
	st.Record(1.0)
	if st.WouldExceed(100.0) {
		t.Error("nil WouldExceed returned true on disabled tracker")
	}

	if got := o.spentCount.Load() + o.exceededCount.Load(); got != 0 {
		t.Errorf("observer fired on nil tracker: %d", got)
	}
}

// TestObserverNilObserver (issue #70) verifies a tracker with no
// observer installed does not panic when its hot-path methods are
// called.
func TestObserverNilObserver(t *testing.T) {
	st := NewSpendTracker(10.0)
	// No SetObserver call: observer remains nil.
	st.Record(1.0)
	if !st.WouldExceed(100.0) {
		t.Error("WouldExceed(100.0) should return true after 1.0 record")
	}
}

// TestObserverConcurrent (issue #70) hammers Record + WouldExceed
// from many goroutines under -race to confirm the observer hook is
// safely serialised by the existing tracker mutex.
func TestObserverConcurrent(t *testing.T) {
	o := &captureObserver{}
	st := NewSpendTracker(1e9) // effectively unlimited budget
	st.SetObserver(o.observe)

	var wg sync.WaitGroup
	const workers = 10
	const iters = 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				st.Record(0.01)
				_ = st.WouldExceed(1.0)
			}
		}()
	}
	wg.Wait()

	if got := o.spentCount.Load(); got != workers*iters {
		t.Errorf("spent events = %d, want %d", got, workers*iters)
	}
}

// TestRunningTotal (issue #70) verifies RunningTotal returns the
// current spend inside the rolling window (alias for CurrentSpend)
// so the Prometheus collector can render the live rolling-total
// gauge.
func TestRunningTotal(t *testing.T) {
	st := NewSpendTracker(100.0)

	st.Record(2.5)
	st.Record(3.5)
	if got := st.RunningTotal(); got != 6.0 {
		t.Errorf("RunningTotal = %v, want 6.0", got)
	}

	// Disabled tracker returns 0.
	var nilSt *SpendTracker
	if got := nilSt.RunningTotal(); got != 0 {
		t.Errorf("nil RunningTotal = %v, want 0", got)
	}
}
