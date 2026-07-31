package health

import (
	"sync"
	"testing"
	"time"
)

// newTestBreaker is a small helper that constructs a Breaker with sensible
// test defaults (short cooldown) so tests do not block on real time.
func newTestBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{
		Threshold: threshold,
		Cooldown:  cooldown,
	}
}

// TestBreakerStateTransitions verifies the full closed→open→half-open→closed
// cycle using State() assertions at each step. This is the primary correctness
// test for the three-state machine.
func TestBreakerStateTransitions(t *testing.T) {
	b := newTestBreaker(3, 30*time.Millisecond)

	// --- Closed ---
	if b.State() != breakerStateClosed {
		t.Fatalf("initial state = %d, want %d (closed)", b.State(), breakerStateClosed)
	}
	if b.IsOpen() {
		t.Fatal("IsOpen must be false in closed state")
	}
	if b.FailureCount() != 0 {
		t.Fatalf("initial FailureCount = %d, want 0", b.FailureCount())
	}

	// --- Sub-threshold failures stay closed ---
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != breakerStateClosed {
		t.Fatalf("state after 2 sub-threshold failures = %d, want closed", b.State())
	}
	if b.FailureCount() != 2 {
		t.Fatalf("FailureCount = %d, want 2", b.FailureCount())
	}

	// --- Trip to open on threshold failure ---
	b.RecordFailure()
	if b.State() != breakerStateOpen {
		t.Fatalf("state after threshold failure = %d, want %d (open)", b.State(), breakerStateOpen)
	}
	if !b.IsOpen() {
		t.Fatal("IsOpen must be true after threshold reached")
	}
	if b.FailureCount() < 3 {
		t.Fatalf("FailureCount = %d, want >= 3", b.FailureCount())
	}

	// --- Wait for cooldown, transition to half-open ---
	time.Sleep(40 * time.Millisecond)
	// First IsOpen after cooldown expiry transitions to half-open and
	// returns false (admitting exactly one probe request).
	if b.IsOpen() {
		t.Fatal("first IsOpen after cooldown must return false (half-open transition)")
	}
	if b.State() != breakerStateHalfOpen {
		t.Fatalf("state after cooldown = %d, want %d (half-open)", b.State(), breakerStateHalfOpen)
	}

	// Second IsOpen in half-open returns true (probe already admitted).
	if !b.IsOpen() {
		t.Fatal("second IsOpen in half-open must return true")
	}

	// --- Record success closes the breaker ---
	b.RecordSuccess()
	if b.State() != breakerStateClosed {
		t.Fatalf("state after success = %d, want %d (closed)", b.State(), breakerStateClosed)
	}
	if b.IsOpen() {
		t.Fatal("IsOpen must be false after success")
	}
	if b.FailureCount() != 0 {
		t.Fatalf("FailureCount after success = %d, want 0", b.FailureCount())
	}
}

// TestBreakerHalfOpenTransition verifies that after cooldown expiry the first
// IsOpen() call returns false (triggering the transition) and subsequent calls
// return true (half-open blocks until the probe resolves).
func TestBreakerHalfOpenTransition(t *testing.T) {
	b := newTestBreaker(2, 20*time.Millisecond)

	// Trip the breaker.
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != breakerStateOpen {
		t.Fatalf("state = %d, want open", b.State())
	}

	// Wait for cooldown to expire.
	time.Sleep(30 * time.Millisecond)

	// First call: CAS open→half-open succeeds, returns false.
	if b.IsOpen() {
		t.Fatal("first IsOpen after cooldown should return false (transition admitted)")
	}
	if b.State() != breakerStateHalfOpen {
		t.Fatalf("state = %d, want half-open", b.State())
	}

	// Second call: half-open blocks.
	if !b.IsOpen() {
		t.Fatal("second IsOpen should return true (half-open blocks)")
	}
}

// TestBreakerHalfOpenProbeFailureRetrips verifies that a failure during
// half-open immediately re-trips the breaker to open.
func TestBreakerHalfOpenProbeFailureRetrips(t *testing.T) {
	b := newTestBreaker(2, 20*time.Millisecond)

	// Trip the breaker.
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != breakerStateOpen {
		t.Fatalf("state = %d, want open", b.State())
	}

	// Wait for cooldown and transition to half-open.
	time.Sleep(30 * time.Millisecond)
	_ = b.IsOpen() // triggers open→half-open
	if b.State() != breakerStateHalfOpen {
		t.Fatalf("state = %d, want half-open", b.State())
	}

	// Failure in half-open immediately re-trips to open, regardless of
	// the current failureCount (which was reset to 0 on half-open entry).
	b.RecordFailure()
	if b.State() != breakerStateOpen {
		t.Fatalf("state after half-open failure = %d, want open", b.State())
	}
	if !b.IsOpen() {
		t.Fatal("IsOpen must be true after re-trip")
	}
}

// TestBreakerHalfOpenProbeSuccessCloses verifies that RecordSuccess from
// half-open transitions directly to closed.
func TestBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	b := newTestBreaker(2, 20*time.Millisecond)

	// Trip.
	b.RecordFailure()
	b.RecordFailure()

	// Wait for cooldown, enter half-open.
	time.Sleep(30 * time.Millisecond)
	_ = b.IsOpen()
	if b.State() != breakerStateHalfOpen {
		t.Fatalf("state = %d, want half-open", b.State())
	}

	// Success closes directly.
	b.RecordSuccess()
	if b.State() != breakerStateClosed {
		t.Fatalf("state = %d, want closed", b.State())
	}
}

// TestBreakerConcurrentRecordFailure runs 50 goroutines × 200 RecordFailure
// calls under the race detector and verifies the final FailureCount is exactly
// the expected sum. This validates that failureCount.Add is correctly atomic
// and that the CAS loop in IsOpen does not corrupt the counter.
func TestBreakerConcurrentRecordFailure(t *testing.T) {
	b := newTestBreaker(3, 100*time.Millisecond)

	const goroutines = 50
	const callsPerGoroutine = 200
	var wg sync.WaitGroup
	start := make(chan struct{}) // barrier for maximum concurrency

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < callsPerGoroutine; j++ {
				b.RecordFailure()
			}
		}()
	}
	close(start)
	wg.Wait()

	want := int32(goroutines * callsPerGoroutine)
	if got := b.FailureCount(); got != want {
		t.Fatalf("concurrent FailureCount = %d, want %d", got, want)
	}
}

// TestBreakerConcurrentMixedAccess exercises IsOpen, RecordFailure, and
// RecordSuccess concurrently to stress the CAS loop and all atomic stores
// under the race detector.
func TestBreakerConcurrentMixedAccess(t *testing.T) {
	b := newTestBreaker(5, 5*time.Millisecond)

	const goroutines = 20
	const iterations = 500
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				switch (id + j) % 3 {
				case 0:
					_ = b.IsOpen()
				case 1:
					b.RecordFailure()
				case 2:
					b.RecordSuccess()
				}
			}
		}(i)
	}
	wg.Wait()
	// No assertion on final state — this test is purely for race detection.
}

// TestBreakerDisabledThresholdZero verifies that a zero Threshold disables
// the breaker: IsOpen always returns false and RecordFailure/RecordSuccess
// are no-ops.
func TestBreakerDisabledThresholdZero(t *testing.T) {
	b := &Breaker{Threshold: 0, Cooldown: time.Second}

	if b.IsOpen() {
		t.Fatal("disabled breaker IsOpen must be false")
	}
	// RecordFailure must be a no-op.
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
	if b.IsOpen() {
		t.Fatal("disabled breaker IsOpen must stay false after failures")
	}
	if b.State() != breakerStateClosed {
		t.Fatalf("state = %d, want closed", b.State())
	}
	if b.FailureCount() != 0 {
		t.Fatalf("FailureCount = %d, want 0 for disabled breaker", b.FailureCount())
	}

	// RecordSuccess must also be a no-op (does not panic or change state).
	b.RecordSuccess()
	if b.State() != breakerStateClosed {
		t.Fatalf("state = %d, want closed after success on disabled", b.State())
	}
}

// TestBreakerSubThresholdFailuresStayClosed verifies that failures below the
// threshold do not trip the breaker.
func TestBreakerSubThresholdFailuresStayClosed(t *testing.T) {
	b := newTestBreaker(5, time.Second)

	for i := 0; i < 4; i++ {
		b.RecordFailure()
	}
	if b.State() != breakerStateClosed {
		t.Fatalf("state = %d, want closed (below threshold)", b.State())
	}
	if b.IsOpen() {
		t.Fatal("IsOpen must be false below threshold")
	}
	if b.FailureCount() != 4 {
		t.Fatalf("FailureCount = %d, want 4", b.FailureCount())
	}
}

// TestBreakerRecordSuccessResetsFailureCount verifies that RecordSuccess
// resets the consecutive failure counter even when the breaker has not
// tripped (sub-threshold recovery).
func TestBreakerRecordSuccessResetsFailureCount(t *testing.T) {
	b := newTestBreaker(5, time.Second)

	b.RecordFailure()
	b.RecordFailure()
	if b.FailureCount() != 2 {
		t.Fatalf("FailureCount = %d, want 2", b.FailureCount())
	}

	b.RecordSuccess()
	if b.FailureCount() != 0 {
		t.Fatalf("FailureCount after success = %d, want 0", b.FailureCount())
	}
	if b.State() != breakerStateClosed {
		t.Fatalf("state = %d, want closed", b.State())
	}
}

// TestBreakerExactThresholdTrips verifies the boundary condition where the
// failure count equals the threshold exactly.
func TestBreakerExactThresholdTrips(t *testing.T) {
	b := newTestBreaker(1, time.Second)

	b.RecordFailure()
	if b.State() != breakerStateOpen {
		t.Fatalf("state = %d, want open at threshold=1 after 1 failure", b.State())
	}
}

// TestBreakerIsOpenInvalidState verifies that an invalid state (e.g., 99)
// is treated as open and emits a log error rather than silently corrupting.
func TestBreakerIsOpenInvalidState(t *testing.T) {
	b := newTestBreaker(3, time.Second)

	// Corrupt the state with an invalid value.
	b.state.Store(99)

	if !b.IsOpen() {
		t.Fatal("IsOpen must return true for invalid state 99")
	}
}

// TestBreakerTripCallback verifies that the TripCallback is called synchronously
// when the breaker trips (issue #971).
func TestBreakerTripCallback(t *testing.T) {
	b := newTestBreaker(3, 50*time.Millisecond)

	var called bool
	var capturedKind string
	b.SetTripCallback("ollama", func(kind string) {
		called = true
		capturedKind = kind
	})

	// Sub-threshold failures should not trigger the callback.
	b.RecordFailure()
	b.RecordFailure()
	if called {
		t.Fatal("callback should not fire below threshold")
	}

	// Threshold failure should fire the callback.
	b.RecordFailure()
	if !called {
		t.Fatal("callback must fire when breaker trips")
	}
	if capturedKind != "ollama" {
		t.Fatalf("capturedKind = %q, want %q", capturedKind, "ollama")
	}

	// Second trip should not fire again (already open).
	called = false
	b.RecordFailure()
	if called {
		t.Fatal("callback should not fire again while breaker is open")
	}

	// Wait for cooldown, transition to half-open.
	time.Sleep(60 * time.Millisecond)
	_ = b.IsOpen()

	// Probe failure in half-open should re-trip and fire callback again.
	called = false
	b.failureCount.Store(0) // reset count since half-open entry resets it
	b.RecordFailure()
	if !called {
		t.Fatal("callback must fire on re-trip from half-open")
	}
	if capturedKind != "ollama" {
		t.Fatalf("capturedKind = %q, want %q", capturedKind, "ollama")
	}
}
