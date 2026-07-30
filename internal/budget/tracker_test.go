package budget

import (
	"sync"
	"testing"
	"time"
)

// TestNewSpendTrackerDisabled pins the backward-compatibility
// contract: a budget <= 0 returns nil so callers can gate "disabled"
// off a single nil-check.
func TestNewSpendTrackerDisabled(t *testing.T) {
	for _, budget := range []float64{0, -1, -0.01} {
		if st := NewSpendTracker(budget); st != nil {
			t.Errorf("NewSpendTracker(%v) = %p, want nil", budget, st)
		}
	}
	// A nil tracker must be safe to call and never block.
	var st *SpendTracker
	if st.WouldExceed(1.0) {
		t.Error("nil WouldExceeded returned true")
	}
	st.Record(1.0) // no-op, must not panic
	if st.CurrentSpend() != 0 {
		t.Error("nil CurrentSpend returned non-zero")
	}
}

// TestUnderBudgetAllows covers the happy path: spend below the cap
// does not trip WouldExceed.
func TestUnderBudgetAllows(t *testing.T) {
	st := NewSpendTracker(10.0)

	st.Record(3.0)
	if st.WouldExceed(5.0) {
		t.Error("WouldExceed(5.0) = true, want false (3+5 < 10)")
	}
	if st.WouldExceed(7.0) {
		t.Error("WouldExceed(7.0) = true, want false (3+7 == 10, not > 10)")
	}
}

// TestOverBudgetBlocks verifies that crossing the cap trips
// WouldExceed.
func TestOverBudgetBlocks(t *testing.T) {
	st := NewSpendTracker(10.0)

	st.Record(8.0)
	if !st.WouldExceed(3.0) {
		t.Error("WouldExceed(3.0) = false, want true (8+3 > 10)")
	}
}

// TestWouldExceedDoesNotRecord verifies that WouldExceed is purely
// advisory — it never mutates the window.
func TestWouldExceedDoesNotRecord(t *testing.T) {
	st := NewSpendTracker(10.0)

	st.Record(5.0)
	_ = st.WouldExceed(100.0) // way over budget
	_ = st.WouldExceed(1.0)   // still over
	_ = st.WouldExceed(0.0)   // zero amount, no-op

	if got := st.CurrentSpend(); got != 5.0 {
		t.Errorf("CurrentSpend = %v, want 5.0 (WouldExceed must not record)", got)
	}
}

// TestBudgetGatesOnlyFrontier is a functional test verifying the
// tracker's semantics: non-frontier (zero or negative) amounts never
// affect the window, matching the handler's cost function which
// returns 0 for non-frontier routes.
func TestBudgetGatesOnlyFrontier(t *testing.T) {
	st := NewSpendTracker(10.0)

	// Zero amounts (local route cost) never affect the tracker.
	st.Record(0)
	st.Record(0)
	if got := st.CurrentSpend(); got != 0 {
		t.Errorf("CurrentSpend after zero records = %v, want 0", got)
	}
	if st.WouldExceed(0) {
		t.Error("WouldExceed(0) = true, want false")
	}

	// Positive amounts (frontier route cost) are tracked.
	st.Record(5.0)
	if got := st.CurrentSpend(); got != 5.0 {
		t.Errorf("CurrentSpend = %v, want 5.0", got)
	}
}

// TestBudgetRecoveryAfter24h verifies that spend entries older than
// the rolling window are pruned, allowing the budget to "recover".
// Uses a short window override via direct struct manipulation to
// keep the test fast.
func TestBudgetRecoveryAfter24h(t *testing.T) {
	st := NewSpendTracker(10.0)
	// Override the window to 100ms for a fast test.
	st.window = 100 * time.Millisecond

	st.Record(8.0)
	if got := st.CurrentSpend(); got != 8.0 {
		t.Fatalf("CurrentSpend = %v, want 8.0", got)
	}
	if !st.WouldExceed(3.0) {
		t.Error("WouldExceed(3.0) = false, want true (8+3 > 10)")
	}

	// Wait for the entry to expire.
	time.Sleep(150 * time.Millisecond)

	// After expiry, the spend should be zero and the budget recovered.
	if got := st.CurrentSpend(); got != 0 {
		t.Errorf("CurrentSpend after window = %v, want 0", got)
	}
	if st.WouldExceed(3.0) {
		t.Error("WouldExceed(3.0) = true after recovery, want false")
	}
}

// TestConcurrentRecords hammers the tracker from many goroutines to
// verify there are no data races (run with -race).
func TestConcurrentRecords(t *testing.T) {
	st := NewSpendTracker(1000.0)

	var wg sync.WaitGroup
	const workers = 20
	const iters = 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				st.Record(0.01)
				_ = st.WouldExceed(0.01)
				_ = st.CurrentSpend()
			}
		}()
	}
	wg.Wait()

	// Total should be workers * iters * 0.01 = 20.0.
	got := st.CurrentSpend()
	if got < 19.0 || got > 21.0 {
		t.Errorf("CurrentSpend = %v, want ~20.0", got)
	}
}

// TestRetryAfter verifies the RetryAfter hint returns the time until
// the oldest entry expires.
func TestRetryAfter(t *testing.T) {
	st := NewSpendTracker(1.0)
	st.window = 200 * time.Millisecond

	st.Record(0.5)
	ra := st.RetryAfter()
	if ra <= 0 {
		t.Errorf("RetryAfter = %v, want positive", ra)
	}
	if ra > 200*time.Millisecond {
		t.Errorf("RetryAfter = %v, want <= 200ms", ra)
	}

	// After the window, RetryAfter should be 0.
	time.Sleep(250 * time.Millisecond)
	if ra := st.RetryAfter(); ra != 0 {
		t.Errorf("RetryAfter after window = %v, want 0", ra)
	}
}

// TestBudgetAccessor verifies the Budget() method returns the
// configured cap.
func TestBudgetAccessor(t *testing.T) {
	st := NewSpendTracker(42.5)
	if got := st.Budget(); got != 42.5 {
		t.Errorf("Budget = %v, want 42.5", got)
	}
}

// TestBudgetNilReceiver verifies Budget() returns 0 on a nil receiver
// (disabled tracker).
func TestBudgetNilReceiver(t *testing.T) {
	var st *SpendTracker
	if got := st.Budget(); got != 0 {
		t.Errorf("Budget on nil = %v, want 0", got)
	}
}

// TestRetryAfterNilReceiver verifies RetryAfter() returns 0 on a nil
// receiver (disabled tracker).
func TestRetryAfterNilReceiver(t *testing.T) {
	var st *SpendTracker
	if got := st.RetryAfter(); got != 0 {
		t.Errorf("RetryAfter on nil = %v, want 0", got)
	}
}

// TestBucketPruning verifies that entries older than the rolling window
// are pruned and the total is correctly maintained.
func TestBucketPruning(t *testing.T) {
	st := NewSpendTracker(100.0)
	st.window = 50 * time.Millisecond

	st.Record(1.0)
	if got := st.CurrentSpend(); got != 1.0 {
		t.Fatalf("after Record: CurrentSpend = %v, want 1.0", got)
	}

	time.Sleep(60 * time.Millisecond)

	_ = st.CurrentSpend()

	if got := st.CurrentSpend(); got != 0.0 {
		t.Errorf("after prune: CurrentSpend = %v, want 0.0", got)
	}
}

// TestManySmallRequests tests that many small requests don't cause
// O(n) performance degradation (issue #1070).
func TestManySmallRequests(t *testing.T) {
	st := NewSpendTracker(10000.0)

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	got := st.CurrentSpend()
	if got < 9.99 || got > 10.01 {
		t.Errorf("CurrentSpend = %v, want ~10.0", got)
	}

	if st.WouldExceed(1.0) {
		t.Error("WouldExceed(1.0) = true, want false (10 + 1 < 10000)")
	}
}

// TestBucketOverflow tests that the tracker handles many entries
// in the same bucket correctly.
func TestBucketOverflow(t *testing.T) {
	st := NewSpendTracker(100.0)

	for i := 0; i < 1000; i++ {
		st.Record(0.01)
	}

	got := st.CurrentSpend()
	if got < 9.99 || got > 10.01 {
		t.Errorf("CurrentSpend = %v, want ~10.0", got)
	}
}
