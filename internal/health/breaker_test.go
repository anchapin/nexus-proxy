package health

import (
	"sync"
	"sync/atomic"
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

// TestRegisterBreakerAndEmbedderBreakers verifies the round-trip: registering
// breakers under specific kind keys and retrieving them via EmbedderBreakers
// with those same keys.
func TestRegisterBreakerAndEmbedderBreakers(t *testing.T) {
	// Snapshot and restore the global registry to isolate this test.
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	ollamaBrk := newTestBreaker(3, time.Second)
	openaiBrk := newTestBreaker(5, 2*time.Second)
	cohereBrk := newTestBreaker(2, 500*time.Millisecond)

	RegisterBreaker("ollama", ollamaBrk)
	RegisterBreaker("openai", openaiBrk)
	RegisterBreaker("cohere", cohereBrk)

	got := EmbedderBreakers()
	if len(got) != 3 {
		t.Fatalf("EmbedderBreakers len = %d, want 3", len(got))
	}
	for kind, want := range map[string]*Breaker{
		"ollama": ollamaBrk,
		"openai": openaiBrk,
		"cohere": cohereBrk,
	} {
		brk, ok := got[kind]
		if !ok {
			t.Errorf("EmbedderBreakers missing key %q", kind)
			continue
		}
		if brk != want {
			t.Errorf("EmbedderBreakers[%q] pointer mismatch", kind)
		}
	}
}

// TestRegisterBreakerOverwrite verifies that re-registering a kind replaces
// the previous breaker.
func TestRegisterBreakerOverwrite(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	first := newTestBreaker(3, time.Second)
	second := newTestBreaker(7, 2*time.Second)

	RegisterBreaker("ollama", first)
	RegisterBreaker("ollama", second)

	got := EmbedderBreakers()
	if got["ollama"] != second {
		t.Fatal("re-registering must overwrite the previous breaker")
	}
}

// TestEmbedderBreakersReturnsCopy verifies that mutations to the returned map
// do not affect the internal registry.
func TestEmbedderBreakersReturnsCopy(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	RegisterBreaker("ollama", newTestBreaker(3, time.Second))

	got := EmbedderBreakers()
	got["injected"] = newTestBreaker(1, time.Second)
	delete(got, "ollama")

	again := EmbedderBreakers()
	if _, ok := again["injected"]; ok {
		t.Fatal("mutating returned map must not affect internal registry")
	}
	if _, ok := again["ollama"]; !ok {
		t.Fatal("deleting from returned map must not affect internal registry")
	}
}

// TestIsEmbedderHealthyRegistered verifies that IsEmbedderHealthy reflects the
// breaker state for registered kinds.
func TestIsEmbedderHealthyRegistered(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	brk := newTestBreaker(2, time.Second)
	RegisterBreaker("ollama", brk)

	// Healthy when breaker is closed.
	if !IsEmbedderHealthy("ollama") {
		t.Fatal("IsEmbedderHealthy must be true when breaker is closed")
	}

	// Trip the breaker.
	brk.RecordFailure()
	brk.RecordFailure()
	if IsEmbedderHealthy("ollama") {
		t.Fatal("IsEmbedderHealthy must be false when breaker is open")
	}

	// Recover.
	brk.RecordSuccess()
	if !IsEmbedderHealthy("ollama") {
		t.Fatal("IsEmbedderHealthy must be true after recovery")
	}
}

// TestIsEmbedderHealthyUnregistered verifies that an unregistered kind is
// treated as healthy (no breaker = no blocking).
func TestIsEmbedderHealthyUnregistered(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	if !IsEmbedderHealthy("nonexistent") {
		t.Fatal("IsEmbedderHealthy must return true for unregistered kind")
	}
}

// TestIsEmbedderHealthyDisabledBreaker verifies that a registered but disabled
// breaker (Threshold==0) always reports healthy.
func TestIsEmbedderHealthyDisabledBreaker(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	brk := &Breaker{Threshold: 0}
	RegisterBreaker("ollama", brk)

	brk.RecordFailure()
	if !IsEmbedderHealthy("ollama") {
		t.Fatal("disabled breaker must always report healthy")
	}
}

// TestBreakerConcurrentIsOpenAndRegistry exercises concurrent reads of the
// global breaker registry alongside concurrent RegisterBreaker writes under
// the race detector.
func TestBreakerConcurrentIsOpenAndRegistry(t *testing.T) {
	mu.Lock()
	saved := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		saved[k] = v
	}
	breakers = make(map[string]*Breaker)
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		breakers = saved
		mu.Unlock()
	})

	var wg sync.WaitGroup
	var stop atomic.Bool

	// Writer: repeatedly register breakers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			brk := newTestBreaker(3, time.Second)
			RegisterBreaker("ollama", brk)
			if i%2 == 0 {
				RegisterBreaker("openai", newTestBreaker(2, time.Second))
			}
		}
	}()

	// Readers: repeatedly query the registry.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = IsEmbedderHealthy("ollama")
				_ = EmbedderBreakers()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}
