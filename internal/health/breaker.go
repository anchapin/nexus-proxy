// Package health exposes the embedder circuit breaker state and the shared
// Breaker struct used by all three embedder implementations (ollama, openai,
// cohere). Having the breaker in a shared package avoids the duplication of
// the isOpen/recordFailure/recordSuccess logic across each embedder while
// keeping the health poller separate from the rag package.
//
// The breaker implements the same three-state machine as the Ollama health
// poller breaker: closed → half-open → open, with a configurable consecutive-
// failure threshold and cooldown window. A zero Threshold disables the breaker
// entirely.
package health

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// BreakerConfig configures a per-embedder circuit breaker.
// A zero Threshold disables the breaker.
type BreakerConfig struct {
	Threshold int           // consecutive failures that trip the breaker; 0 = disabled
	Cooldown  time.Duration // how long the breaker stays open after tripping
}

// Breaker is a three-state circuit breaker: closed, half-open, and open.
// It is safe for concurrent use via atomic operations.
//
// A zero Breaker (Threshold==0) is disabled: all calls to IsOpen return
// false and recordFailure/recordSuccess are no-ops.
type Breaker struct {
	// Threshold is the number of consecutive failures required to trip
	// the breaker. Zero disables the breaker.
	Threshold int

	// Cooldown is how long the breaker stays open before transitioning
	// to half-open.
	Cooldown time.Duration

	// Internal state (atomic):
	//   state: 0=closed, 1=half_open, 2=open
	//   failureCount: consecutive failures since last success
	//   cooldownUntil: nanoseconds since Unix epoch; 0 = not in cooldown
	state         atomic.Int32
	failureCount  atomic.Int32
	cooldownUntil atomic.Int64
}

const (
	breakerStateClosed   int32 = 0
	breakerStateHalfOpen int32 = 1
	breakerStateOpen     int32 = 2
)

// IsOpen reports whether the circuit is currently in the open (cooldown)
// state. When the cooldown deadline has passed, IsOpen atomically transitions
// to half-open and admits exactly one probe request.
func (b *Breaker) IsOpen() bool {
	if b.Threshold == 0 {
		return false
	}
	for {
		switch b.state.Load() {
		case breakerStateClosed:
			return false
		case breakerStateHalfOpen:
			return true
		case breakerStateOpen:
			deadline := b.cooldownUntil.Load()
			if deadline == 0 || time.Now().UnixNano() < deadline {
				return true
			}
			if !b.state.CompareAndSwap(breakerStateOpen, breakerStateHalfOpen) {
				continue
			}
			b.failureCount.Store(0)
			b.cooldownUntil.Store(0)
			return false
		default:
			return true
		}
	}
}

// RecordFailure increments the consecutive-failure counter and trips the
// breaker when the threshold is reached. The cooldown window starts from
// the current time, or immediately when a half-open probe fails.
func (b *Breaker) RecordFailure() {
	if b.Threshold == 0 {
		return
	}
	count := b.failureCount.Add(1)
	if b.state.Load() == breakerStateHalfOpen || count >= int32(b.Threshold) {
		// Trip: set the cooldown deadline. We add one nanosecond so that
		// the comparison in IsOpen is strict (deadline > now, not >=).
		b.cooldownUntil.Store(time.Now().Add(b.Cooldown).UnixNano() + 1)
		b.state.Store(breakerStateOpen)
		slog.Warn("embedder circuit breaker tripped",
			slog.Int("failures", int(count)),
			slog.Int("threshold", b.Threshold),
			slog.Duration("cooldown", b.Cooldown),
		)
	}
}

// RecordSuccess resets the consecutive-failure counter and transitions
// the breaker to closed. No-op when the breaker is already closed.
func (b *Breaker) RecordSuccess() {
	if b.Threshold == 0 {
		return
	}
	b.failureCount.Store(0)
	b.cooldownUntil.Store(0)
	b.state.Store(breakerStateClosed)
}

// State returns the current breaker state: 0=closed, 1=half_open, 2=open.
func (b *Breaker) State() int32 {
	return b.state.Load()
}

// FailureCount returns the current consecutive-failure count.
func (b *Breaker) FailureCount() int32 {
	return b.failureCount.Load()
}

// IsEmbedderHealthy reports whether the embedder of the given kind is
// currently healthy (circuit breaker not open). The kind is one of
// "ollama", "openai", or "cohere".
//
// If no breaker has been registered for the given kind, IsEmbedderHealthy
// returns true (no circuit breaker means no blocking).
func IsEmbedderHealthy(kind string) bool {
	mu.RLock()
	defer mu.RUnlock()
	if b, ok := breakers[kind]; ok {
		return !b.IsOpen()
	}
	return true // no breaker registered = assume healthy
}

// embedderBreakers is the registry of per-kind embedder circuit breakers.
// Wired from rag.go at construction time.
var (
	breakers = make(map[string]*Breaker)
	mu       sync.RWMutex
)

// RegisterBreaker registers a circuit breaker for the given embedder kind.
// This is called from rag.go when each embedder is constructed so that
// IsEmbedderHealthy can be used by observability code without importing
// the rag package.
func RegisterBreaker(kind string, brk *Breaker) {
	mu.Lock()
	defer mu.Unlock()
	breakers[kind] = brk
}

// EmbedderBreakers returns the current set of registered embedder breakers
// as a map. Used by the Prometheus renderer to emit per-kind gauges.
func EmbedderBreakers() map[string]*Breaker {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]*Breaker, len(breakers))
	for k, v := range breakers {
		out[k] = v
	}
	return out
}
