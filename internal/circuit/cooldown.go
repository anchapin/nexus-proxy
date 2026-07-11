// Package circuit implements a short local-route cooldown used after the
// cascade detects an Ollama / local-model failure (issue #80).
//
// The cooldown closes the window between the cascade observing a local
// failure and the background health poller catching up. Without it, every
// subsequent request re-attempts the (now-dead) local endpoint and pays
// the full upstream timeout before falling back — a window of repeated
// slow local attempts that the issue calls out.
//
// Design:
//
//   - Race-safe via a single atomic.Int64 holding the Unix-nano timestamp
//     of the last recorded failure. No mutex on the hot path.
//   - A Duration <= 0 disables the circuit: Active always returns false
//     and RecordFailure is a no-op. This is the NEXUS_LOCAL_COOLDOWN=0
//     acceptance criterion.
//   - The clock is injectable (now func() time.Time) so tests can drive
//     the active / expired / disabled transitions deterministically
//     without sleeping.
package circuit

import (
	"sync/atomic"
	"time"
)

// Cooldown is a race-safe local-route cooldown circuit. After
// RecordFailure is called, Active reports true for the configured
// Duration. The zero value (Duration == 0) is a disabled circuit.
//
// Cooldown is safe for concurrent use by many goroutines (the chat
// handler is itself concurrent). The hot-path read (Active) is a single
// atomic load; RecordFailure is a single atomic store.
type Cooldown struct {
	// Duration is the length of the cooldown window. Zero or negative
	// disables the circuit entirely — Active always returns false and
	// RecordFailure is a no-op. Configured via NEXUS_LOCAL_COOLDOWN.
	Duration time.Duration

	// now returns the current time. Overridable for tests via
	// NewWithClock. When nil, time.Now is used.
	now func() time.Time

	// failedAt stores the Unix-nano timestamp of the last recorded
	// local failure. Zero means "no failure recorded". Stored and
	// loaded atomically so concurrent RecordFailure / Active calls
	// never tear.
	failedAt atomic.Int64
}

// New constructs a Cooldown with the given duration. A non-positive
// duration produces a disabled circuit (Active always returns false).
func New(d time.Duration) *Cooldown {
	return &Cooldown{Duration: d, now: time.Now}
}

// NewWithClock constructs a Cooldown whose clock is driven by now.
// Intended for tests that need deterministic time progression without
// sleeping. A nil now falls back to time.Now.
func NewWithClock(d time.Duration, now func() time.Time) *Cooldown {
	if now == nil {
		now = time.Now
	}
	return &Cooldown{Duration: d, now: now}
}

// RecordFailure stamps the current time as the last local failure.
// Subsequent calls to Active report true until Duration has elapsed.
//
// When the circuit is disabled (Duration <= 0) this is a no-op.
// Safe to call from many goroutines.
func (c *Cooldown) RecordFailure() {
	if c == nil || c.Duration <= 0 {
		return
	}
	c.failedAt.Store(c.now().UnixNano())
}

// Active reports whether the cooldown window is still in effect — i.e.
// a failure was recorded and fewer than Duration have elapsed since.
//
// Returns false when:
//   - the circuit is nil (wired as optional, same as health.Health);
//   - Duration <= 0 (disabled via NEXUS_LOCAL_COOLDOWN=0);
//   - no failure has been recorded yet;
//   - the last recorded failure is older than Duration.
func (c *Cooldown) Active() bool {
	if c == nil || c.Duration <= 0 {
		return false
	}
	ts := c.failedAt.Load()
	if ts == 0 {
		return false
	}
	failedAt := time.Unix(0, ts)
	return c.now().Sub(failedAt) < c.Duration
}

// ExpiresAt returns the wall-clock time at which the current cooldown
// window ends, or the zero time if no failure has been recorded.
// Exposed for /healthz diagnostics, logging, and tests.
func (c *Cooldown) ExpiresAt() time.Time {
	if c == nil || c.Duration <= 0 {
		return time.Time{}
	}
	ts := c.failedAt.Load()
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(0, ts).Add(c.Duration)
}
