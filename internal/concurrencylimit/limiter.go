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
//
// Multi-GPU support (issue #775): NewVRAMLimiter creates one semaphore
// per GPU and distributes requests in round-robin order. When a GPU's
// semaphore is exhausted (its in-flight >= effective slots based on that
// GPU's free VRAM), the limiter falls through to the next GPU in the
// ring. The fallback traversal is bounded at O(gpuCount).
package concurrencylimit

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
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
	// wait forever (issue #439). The callback grabs l.mu so the
	// Broadcast is strictly ordered with respect to Cond.Wait's
	// register-and-park step: either the Broadcast acquires l.mu
	// before Wait releases it (lost — but in that case Wait has not
	// yet registered, so Wait will re-check ctx.Err() on the next
	// loop iteration once it does register), or the Broadcast is
	// sequenced behind Wait's release and observes the waiter
	// already on the list. The naive "Broadcast without l.mu"
	// pattern can lose the wakeup because the Broadcast can run
	// before Cond.Wait installs the listener.
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})

	var err error
	l.mu.Lock()
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		if l.inFlight < l.effectiveLocked() {
			l.inFlight++
			break
		}
		l.cond.Wait()
	}
	l.mu.Unlock()

	// Deregister the AfterFunc outside the mutex. If a cancellation
	// callback is currently running its Broadcast under l.mu, Stop
	// may block briefly waiting for it to complete; that is safe
	// because we have already released l.mu and so cannot be the
	// reason the callback is stuck. Calling stop inside the locked
	// section would risk a self-deadlock against the callback's
	// own l.mu.Lock().
	stop()

	if err != nil {
		return nil, err
	}
	return l.release, nil
}

// release decrements the in-flight counter and wakes blocked
// acquirers. It is the function returned by a successful Acquire.
func (l *Limiter) release() {
	l.mu.Lock()
	prevEff := l.effectiveLocked()
	if l.inFlight > 0 {
		l.inFlight--
	}
	// Broadcast only when the effective count has grown (probe
	// reported more VRAM) so that multiple waiters can proceed at
	// once. In the normal single-slot-release case the effective
	// count is unchanged; Signal is sufficient and avoids waking
	// every waiter when only one slot is available.
	newEff := l.effectiveLocked()
	if newEff > prevEff {
		l.cond.Broadcast()
	} else {
		l.cond.Signal()
	}
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

// gpuSlot tracks in-flight slots and effective limit for one GPU.
type gpuSlot struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
	lastEff  int
	haveLast bool
}

// gpuLimiter distributes Acquire calls across multiple per-GPU
// semaphores using round-robin GPU selection (issue #775). Each GPU
// has its own in-flight counter and condition variable so that a
// request blocked on GPU 0 does not prevent GPU 1 from accepting work.
type gpuLimiter struct {
	ceiling      int
	bytesPerSlot int64
	// FreeVRAM returns the free VRAM in bytes for GPU i. A nil entry
	// or non-positive value means "probe unavailable for this GPU" and
	// the limiter falls back to the per-GPU ceiling share.
	freeVRAM []func() int64
	gpuCount int

	slots []gpuSlot
	rr    atomic.Uint64 // round-robin counter

	// ceilingPerGPU is ceiling/gpuCount rounded up; used as the
	// default effective slots when probe returns nil/0.
	ceilingPerGPU int

	// onWait is called just before a goroutine enters cond.Wait().
	// It is nil in production; tests set it to observe synchronization.
	// Stored in atomic.Value (storing func()) for race-free access.
	onWait atomic.Value // stores func()
}

// NewVRAMLimiter constructs a multi-GPU limiter (issue #775). It
// distributes Acquire calls across gpuCount GPUs in round-robin order
// using an atomic counter. When a GPU's semaphore is exhausted, the
// call falls through to the next GPU in the ring (bounded O(gpuCount)).
//
// The total concurrent slots across all GPUs is capped at ceiling.
// Each GPU gets at most ceilingPerGPU = ceil(ceiling/gpuCount) slots
// based on its own free-VRAM reading.
//
// A non-positive ceiling, bytesPerSlot, or gpuCount < 1 produces a
// disabled limiter (Acquire is a no-op). A gpuCount of 1 still creates
// a valid limiter (no round-robin needed).
//
// freeVRAM is an array of per-GPU closures. A nil closure or
// non-positive return means "probe unavailable for this GPU" and
// that GPU falls back to its share of the ceiling.
func NewVRAMLimiter(ceiling int, bytesPerSlot int64, freeVRAM []func() int64, gpuCount int) *gpuLimiter {
	if ceiling <= 0 || gpuCount < 1 {
		return &gpuLimiter{}
	}
	if bytesPerSlot <= 0 {
		bytesPerSlot = DefaultBytesPerSlot
	}
	slots := make([]gpuSlot, gpuCount)
	for i := 0; i < gpuCount; i++ {
		slots[i].cond = sync.NewCond(&slots[i].mu)
	}
	ceilingPerGPU := int(math.Ceil(float64(ceiling) / float64(gpuCount)))
	return &gpuLimiter{
		ceiling:       ceiling,
		bytesPerSlot:  bytesPerSlot,
		freeVRAM:      freeVRAM,
		gpuCount:      gpuCount,
		slots:         slots,
		ceilingPerGPU: ceilingPerGPU,
	}
}

// EffectivePerGPU returns the effective slot count for GPU i.
func (g *gpuLimiter) EffectivePerGPU(i int) int {
	if g == nil || i < 0 || i >= g.gpuCount {
		return 0
	}
	g.slots[i].mu.Lock()
	defer g.slots[i].mu.Unlock()
	return g.effectivePerGPULocked(i)
}

func (g *gpuLimiter) effectivePerGPULocked(i int) int {
	if g.ceiling <= 0 {
		return 0
	}
	perGPU := g.ceilingPerGPU
	if i < len(g.freeVRAM) && g.freeVRAM[i] != nil {
		free := g.freeVRAM[i]()
		if free > 0 && g.bytesPerSlot > 0 {
			slots := int(free / g.bytesPerSlot)
			if slots < 1 {
				slots = 1
			}
			if slots > g.ceilingPerGPU {
				slots = g.ceilingPerGPU
			}
			perGPU = slots
		}
	}
	if !g.slots[i].haveLast || g.slots[i].lastEff != perGPU {
		g.slots[i].lastEff = perGPU
		g.slots[i].haveLast = true
		slog.Info("local concurrency effective slots (per GPU)",
			slog.Int("gpu", i),
			slog.Int("slots", perGPU),
			slog.Int("ceiling_per_gpu", g.ceilingPerGPU),
		)
	}
	return perGPU
}

// AcquireGPU blocks until a slot is available on one of the GPUs or ctx
// is cancelled. On success it returns the GPU index that was assigned and
// a release function to call when done. On ctx cancellation it returns
// ctx.Err() and -1.
//
// GPU selection is round-robin via an atomic counter. When the selected
// GPU has no available slot, the limiter falls through to the next GPU
// in the ring (at most gpuCount attempts, O(gpuCount) bounded).
//
// A disabled limiter (ceiling <= 0) returns a no-op release immediately.
func (g *gpuLimiter) AcquireGPU(ctx context.Context) (func(), error) {
	if g == nil || g.ceiling <= 0 {
		return func() {}, nil
	}
	if g.gpuCount == 1 {
		return g.acquireSingle(ctx, 0)
	}
	start := int(g.rr.Add(1) - 1)
	for attempt := 0; attempt < g.gpuCount; attempt++ {
		gpu := (start + attempt) % g.gpuCount
		rel, ok := g.tryAcquire(ctx, gpu)
		if ok {
			return rel, nil
		}
	}
	return g.acquireSingle(ctx, start%g.gpuCount)
}

func (g *gpuLimiter) tryAcquire(ctx context.Context, gpu int) (func(), bool) {
	stop := context.AfterFunc(ctx, func() {
		g.slots[gpu].mu.Lock()
		g.slots[gpu].cond.Broadcast()
		g.slots[gpu].mu.Unlock()
	})

	g.slots[gpu].mu.Lock()
	for {
		if err := ctx.Err(); err != nil {
			g.slots[gpu].mu.Unlock()
			stop()
			return nil, false
		}
		eff := g.effectivePerGPULocked(gpu)
		if g.slots[gpu].inFlight < eff {
			g.slots[gpu].inFlight++
			g.slots[gpu].mu.Unlock()
			stop()
			return func() { g.releaseGPU(gpu) }, true
		}
		if cb, ok := g.onWait.Load().(func()); ok && cb != nil {
			cb()
		}
		g.slots[gpu].cond.Wait()
	}
}

func (g *gpuLimiter) acquireSingle(ctx context.Context, gpu int) (func(), error) {
	stop := context.AfterFunc(ctx, func() {
		g.slots[gpu].mu.Lock()
		g.slots[gpu].cond.Broadcast()
		g.slots[gpu].mu.Unlock()
	})

	g.slots[gpu].mu.Lock()
	for {
		if err := ctx.Err(); err != nil {
			g.slots[gpu].mu.Unlock()
			stop()
			return nil, err
		}
		eff := g.effectivePerGPULocked(gpu)
		if g.slots[gpu].inFlight < eff {
			g.slots[gpu].inFlight++
			g.slots[gpu].mu.Unlock()
			stop()
			return func() { g.releaseGPU(gpu) }, nil
		}
		if cb, ok := g.onWait.Load().(func()); ok && cb != nil {
			cb()
		}
		g.slots[gpu].cond.Wait()
	}
}

func (g *gpuLimiter) releaseGPU(gpu int) {
	g.slots[gpu].mu.Lock()
	prevEff := g.effectivePerGPULocked(gpu)
	if g.slots[gpu].inFlight > 0 {
		g.slots[gpu].inFlight--
	}
	newEff := g.effectivePerGPULocked(gpu)
	if newEff > prevEff {
		g.slots[gpu].cond.Broadcast()
	} else {
		g.slots[gpu].cond.Signal()
	}
	g.slots[gpu].mu.Unlock()
}

// InFlightByGPU returns the in-flight count per GPU. The returned slice
// has length gpuCount; a nil slice means the limiter is disabled.
func (g *gpuLimiter) InFlightByGPU() []int {
	if g == nil || g.ceiling <= 0 || len(g.slots) == 0 {
		return nil
	}
	out := make([]int, len(g.slots))
	for i := 0; i < len(g.slots); i++ {
		g.slots[i].mu.Lock()
		out[i] = g.slots[i].inFlight
		g.slots[i].mu.Unlock()
	}
	return out
}
