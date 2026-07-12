// Package concurrencylimit implements a VRAM-aware concurrency limiter
// for the local-route path of the chat handler (issue #81).
//
// The limiter treats NEXUS_LOCAL_MAX_CONCURRENT as a hard ceiling and
// shrinks the effective slot count when the latest VRAM probe reports
// less free memory. The formula is:
//
//	effective = min(Ceiling, freeVRAM / BytesPerSlot)
//
// When the probe is unavailable (nil FreeVRAM closure, or it returns a
// non-positive value) the limiter retains the full Ceiling — the "safe
// static behaviour" the issue mandates — so a missing probe never opens
// the floodgates.
//
// The limiter is reactive rather than proactive: it reads the latest
// probe snapshot on every Acquire. No background goroutine is required;
// the chat hot path simply calls Acquire/Release around the local
// upstream dispatch.
//
// The implementation is stdlib-only and race-safe: a sync.Mutex guards
// the in-flight counter and a sync.Cond wakes blocked acquirers when a
// slot frees or the effective count grows. Context cancellation is
// honoured via context.AfterFunc so a request whose context is done
// never blocks indefinitely.
package concurrencylimit

import (
	"context"
	"log/slog"
	"sync"
)

// DefaultBytesPerSlot is the conservative VRAM reservation assumed per
// concurrent local-route slot when NEXUS_LOCAL_VRAM_BYTES_PER_SLOT is
// not set. 2 GiB keeps a Q4-quantised 8B model plus a modest context
// window resident; on the PRD's target 8-12 GiB GPUs this yields ~3-5
// effective slots once the loaded model's footprint is accounted for.
const DefaultBytesPerSlot int64 = 2 << 30 // 2 GiB

// Limiter bounds the number of concurrent local-route requests the
// proxy issues against the local Ollama instance. It is safe for
// concurrent use by many goroutines (the chat handler is itself
// concurrent). The zero value is a no-op limiter (Ceiling <= 0); always
// construct via New.
//
// The effective slot count is recomputed on every Acquire from the
// latest probe snapshot, so a thermal-throttle or model-swap event that
// drops free VRAM is reflected on the very next request without
// restarting the process. Existing in-flight requests are never
// preempted: if the effective count shrinks below the in-flight count,
// new Acquires block until enough requests Release to bring in-flight
// under the new ceiling.
type Limiter struct {
	// Ceiling is the hard upper bound on concurrent slots
	// (NEXUS_LOCAL_MAX_CONCURRENT). Zero or negative disables the
	// limiter entirely: Acquire returns immediately with a no-op
	// release. This preserves the pre-issue-#81 unlimited behaviour
	// for operators who leave the knob unset.
	Ceiling int

	// BytesPerSlot is the VRAM reservation each concurrent slot
	// assumes. Zero or negative falls back to DefaultBytesPerSlot
	// (set by New). It only affects the dynamic shrink path; when
	// the probe is unavailable the full Ceiling is used regardless.
	BytesPerSlot int64

	// FreeVRAM returns the latest free-VRAM snapshot in bytes from
	// the probe manager. A nil closure or a non-positive return
	// means "probe unavailable" and the limiter falls back to the
	// full Ceiling (safe static behaviour). Wired as a closure in
	// cmd/nexus/main.go so this package never imports internal/probe.
	FreeVRAM func() int64

	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
	lastEff  int // last effective slot count we logged
	haveLast bool
}

// New constructs a Limiter. A non-positive ceiling produces a disabled
// limiter (Acquire is a no-op). A non-positive bytesPerSlot falls back
// to DefaultBytesPerSlot. freeVRAM may be nil; a nil closure makes the
// limiter always use the full Ceiling (probe-unavailable path).
func New(ceiling int, bytesPerSlot int64, freeVRAM func() int64) *Limiter {
	if bytesPerSlot <= 0 {
		bytesPerSlot = DefaultBytesPerSlot
	}
	l := &Limiter{
		Ceiling:      ceiling,
		BytesPerSlot: bytesPerSlot,
		FreeVRAM:     freeVRAM,
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Effective returns the current slot count the limiter would honour.
// It reads the latest probe snapshot and applies the
// min(Ceiling, freeVRAM/BytesPerSlot) formula. Exposed for /healthz
// and tests; the chat hot path exercises it via Acquire. Returns 0
// when the limiter is disabled (Ceiling <= 0).
func (l *Limiter) Effective() int {
	if l == nil || l.Ceiling <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effectiveLocked()
}

// effectiveLocked computes the effective slot count and emits a
// low-cardinality slog line when the value changes since the last
// computation. Caller must hold l.mu.
func (l *Limiter) effectiveLocked() int {
	if l.Ceiling <= 0 {
		return 0
	}
	var free int64
	if l.FreeVRAM != nil {
		free = l.FreeVRAM()
	}
	var slots int
	switch {
	case free <= 0 || l.BytesPerSlot <= 0:
		// Probe unavailable: retain the full ceiling so a missing
		// probe never opens the floodgates beyond the operator's
		// configured hard limit.
		slots = l.Ceiling
	default:
		slots = int(free / l.BytesPerSlot)
		if slots < 1 {
			// Never zero out entirely when the probe IS reporting:
			// a single slot keeps the local path serviceable while
			// the cascade handles any resulting OOM via fallback.
			slots = 1
		}
		if slots > l.Ceiling {
			slots = l.Ceiling
		}
	}
	if !l.haveLast || l.lastEff != slots {
		l.lastEff = slots
		l.haveLast = true
		slog.Info("local concurrency effective slots",
			slog.Int("slots", slots),
			slog.Int("ceiling", l.Ceiling),
			slog.Int64("free_vram_bytes", free),
		)
	}
	return slots
}

// Acquire blocks until a slot is available or ctx is cancelled. On
// success it returns a non-nil release function the caller MUST invoke
// exactly once when the local-route work is done (typically deferred).
// On ctx cancellation it returns ctx.Err() and a nil release.
//
// A disabled limiter (nil receiver or Ceiling <= 0) returns a no-op
// release immediately so the hot path is byte-for-byte identical to the
// pre-issue-#81 unlimited path.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if l == nil || l.Ceiling <= 0 {
		return func() {}, nil
	}
	// Wake blocked acquirers when ctx is cancelled so they do not
	// wait forever. context.AfterFunc runs f in a new goroutine if
	// the context is already done; Broadcast is safe to call without
	// holding the lock (sync.Cond allows it).
	stop := context.AfterFunc(ctx, func() { l.cond.Broadcast() })

	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			// stop is documented as not waiting for a running f, so
			// this cannot deadlock against our held mutex.
			stop()
			return nil, err
		}
		if l.inFlight < l.effectiveLocked() {
			l.inFlight++
			// Deregister the AfterFunc; a late Broadcast would be a
			// harmless no-op but cleaning up avoids leaking it.
			stop()
			return l.release, nil
		}
		l.cond.Wait()
	}
}

// release decrements the in-flight counter and wakes one blocked
// acquirer. It is the function returned by a successful Acquire.
func (l *Limiter) release() {
	l.mu.Lock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	// Broadcast rather than Signal so that, when the effective count
	// has grown (probe reported more VRAM), multiple waiters can
	// proceed at once. Signal would only wake one.
	l.cond.Broadcast()
	l.mu.Unlock()
}

// InFlight returns the current number of held slots. Exposed for
// /healthz diagnostics and tests; not consulted by the chat hot path.
func (l *Limiter) InFlight() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight
}
