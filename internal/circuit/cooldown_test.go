package circuit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable time source for deterministic tests.
// Advance moves the clock forward; Now returns the current value.
// It starts at a non-zero epoch so the zero-value sentinel in Cooldown
// (failedAt == 0 means "no failure") is never confused with a real
// timestamp.
type fakeClock struct {
	t atomic.Int64 // unix nano
}

func newFakeClock() *fakeClock {
	f := &fakeClock{}
	// Start at 2024-01-01T00:00:00Z so timestamps are always non-zero.
	f.t.Store(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return f
}

func (f *fakeClock) Now() time.Time { return time.Unix(0, f.t.Load()) }
func (f *fakeClock) Advance(d time.Duration) {
	f.t.Add(int64(d))
}

func TestCooldown_DisabledWhenDurationZero(t *testing.T) {
	t.Parallel()
	c := New(0)
	c.RecordFailure()
	if c.Active() {
		t.Fatal("disabled cooldown should not be active")
	}
}

func TestCooldown_DisabledWhenDurationNegative(t *testing.T) {
	t.Parallel()
	c := New(-5 * time.Second)
	c.RecordFailure()
	if c.Active() {
		t.Fatal("negative-duration cooldown should not be active")
	}
}

func TestCooldown_NilSafe(t *testing.T) {
	t.Parallel()
	var c *Cooldown
	c.RecordFailure() // must not panic
	if c.Active() {
		t.Fatal("nil cooldown should report inactive")
	}
}

func TestCooldown_ActiveAfterFailure(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)

	if c.Active() {
		t.Fatal("cooldown should be inactive before any failure")
	}

	c.RecordFailure()
	if !c.Active() {
		t.Fatal("cooldown should be active immediately after failure")
	}

	// Still active just before the window expires.
	clk.Advance(9 * time.Second)
	if !c.Active() {
		t.Fatal("cooldown should be active within the window")
	}
}

func TestCooldown_ExpiresAfterDuration(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)

	c.RecordFailure()
	clk.Advance(10 * time.Second)
	if c.Active() {
		t.Fatal("cooldown should expire exactly at Duration")
	}

	// A later failure after expiry re-arms the circuit.
	c.RecordFailure()
	if !c.Active() {
		t.Fatal("cooldown should re-arm after a new failure post-expiry")
	}
}

func TestCooldown_ExpiresAt(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()

	c := NewWithClock(30*time.Second, clk.Now)

	if !c.ExpiresAt().IsZero() {
		t.Fatalf("ExpiresAt should be zero before any failure, got %v", c.ExpiresAt())
	}

	c.RecordFailure()
	want := clk.Now().Add(30 * time.Second)
	got := c.ExpiresAt()
	if !got.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got, want)
	}
}

func TestCooldown_RecordFailureOverwrites(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)

	c.RecordFailure()
	clk.Advance(8 * time.Second)
	// A second failure resets the window.
	c.RecordFailure()
	clk.Advance(8 * time.Second)
	if !c.Active() {
		t.Fatal("cooldown should still be active after second failure reset the window")
	}
}

// TestCooldown_Concurrent exercises RecordFailure and Active under
// concurrent access to verify the atomic operations are race-free.
// Run with -race to catch data races.
func TestCooldown_Concurrent(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(5*time.Second, clk.Now)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.RecordFailure()
				_ = c.Active()
			}
		}()
	}
	wg.Wait()
}

func TestCooldown_FailureObserverCalled(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)

	var calls atomic.Int64
	c.SetFailureObserver(func() {
		calls.Add(1)
	})

	c.RecordFailure()
	if got := calls.Load(); got != 1 {
		t.Fatalf("observer called %d times after one RecordFailure, want 1", got)
	}

	// Second failure should call observer again.
	c.RecordFailure()
	if got := calls.Load(); got != 2 {
		t.Fatalf("observer called %d times after two RecordFailure calls, want 2", got)
	}
}

func TestCooldown_FailureObserverNotCalledWhenDisabled(t *testing.T) {
	t.Parallel()
	c := New(0) // disabled

	var calls atomic.Int64
	c.SetFailureObserver(func() {
		calls.Add(1)
	})

	c.RecordFailure()
	if got := calls.Load(); got != 0 {
		t.Fatalf("observer called %d times when cooldown is disabled, want 0", got)
	}
}

func TestCooldown_FailureObserverNilClearsObserver(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)

	var calls atomic.Int64
	c.SetFailureObserver(func() {
		calls.Add(1)
	})

	c.RecordFailure()
	c.SetFailureObserver(nil) // clear

	c.RecordFailure()
	if got := calls.Load(); got != 1 {
		t.Fatalf("observer called %d times after clear, want 1", got)
	}
}

func TestCooldown_FailureObserverNilSafe(t *testing.T) {
	t.Parallel()
	var c *Cooldown
	c.SetFailureObserver(func() {}) // must not panic
}

func TestCooldown_NewWithClockNilFallsBackToTimeNow(t *testing.T) {
	t.Parallel()
	c := NewWithClock(10*time.Second, nil)
	if c == nil {
		t.Fatal("NewWithClock with nil now should not return nil")
	}
	if c.now == nil {
		t.Fatal("NewWithClock with nil now should fall back to time.Now, not nil")
	}
	if c.Active() {
		t.Fatal("cooldown should be inactive before any failure")
	}
	c.RecordFailure()
	if !c.Active() {
		t.Fatal("cooldown with nil clock should be active after failure")
	}
}

func TestCooldown_ExpiresAtNilSafe(t *testing.T) {
	t.Parallel()
	var c *Cooldown
	if !c.ExpiresAt().IsZero() {
		t.Fatal("ExpiresAt on nil cooldown should return zero time")
	}
}

func TestCooldown_NewWithClockWithRealClock(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	c := NewWithClock(10*time.Second, clk.Now)
	if c.now == nil {
		t.Fatal("NewWithClock with non-nil now should preserve it")
	}
	if c.Active() {
		t.Fatal("cooldown should be inactive before any failure")
	}
	c.RecordFailure()
	clk.Advance(9 * time.Second)
	if !c.Active() {
		t.Fatal("cooldown with injected clock should be active within the window")
	}
	clk.Advance(2 * time.Second)
	if c.Active() {
		t.Fatal("cooldown with injected clock should expire after Duration")
	}
}
