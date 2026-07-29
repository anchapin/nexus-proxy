// Package budget provides a rolling 24-hour spend tracker for
// frontier-route requests (issue #38). The tracker maintains an
// in-memory window of (timestamp, amount) pairs; WouldExceed is
// consulted before dispatching to the frontier, and Record is called
// after the request completes. When the daily budget is zero or
// unset, the tracker is disabled (NewSpendTracker returns nil) and
// both methods are no-ops — preserving the pre-issue-#38 behaviour.
package budget

import (
	"sync"
	"time"
)

// defaultWindow is the rolling spend window. Matches the issue #38
// spec: "rolling daily spend cap".
const defaultWindow = 24 * time.Hour

// ringCapacity is the initial and maximum capacity of the ring buffer.
// 16384 entries can absorb ~1700 frontier requests/hour for 24 hours —
// far beyond any realistic deployment (issue #987).
const ringCapacity = 16384

// entry is a single spend record inside the rolling window.
type entry struct {
	at     time.Time
	amount float64
}

// BudgetObserverFunc is the function-typed hook invoked by a
// SpendTracker on every spend and exceed event (issue #70). The event
// label is one of ObserverEventSpent or ObserverEventExceeded.
//
// Implementations must be safe to call concurrently from many
// request goroutines and must be allocation-free on the hot path
// (the production observer is a single atomic add per call).
type BudgetObserverFunc func(event string, amount float64)

// Event labels passed to BudgetObserverFunc (issue #70). Using
// named constants instead of bare strings avoids typos at the wiring
// site and survives an event-namespace refactor cleanly.
const (
	// ObserverEventSpent is emitted after a Record(amount) call
	// appends the amount to the rolling window.
	ObserverEventSpent = "spent"
	// ObserverEventExceeded is emitted when WouldExceed(amount)
	// returns true (i.e. recording amount would cross the cap).
	ObserverEventExceeded = "exceeded"
)

// SpendTracker is a rolling-window sum of frontier-route spend. It
// is safe for concurrent use. A nil SpendTracker is treated as
// "disabled" everywhere it is referenced, so the chat handler can
// leave Deps.SpendGuard unset (preserving the pre-issue-#38
// behaviour) and tests can opt in per-case.
//
// Internally a ring buffer with head/tail indices (issue #987):
// - head: index of oldest entry (advances during prune — O(1))
// - tail: index for next write (advances after each Record)
// - count: number of active entries in the ring
//
// The ring stores entries in chronological order starting at head,
// wrapping via modulo. This means pruning is O(1) — just advance head —
type SpendTracker struct {
	mu       sync.Mutex
	window   time.Duration
	budget   float64
	observer BudgetObserverFunc

	// Ring buffer fields (issue #987).
	entries []entry // backing store; len=capacity, first count slots are active
	head    int     // index of oldest active entry (0 when count==0)
	tail    int     // index for the next write (mod cap(entries))
	count   int     // number of active entries (0 when empty)
}

// SetObserver installs an observer hook. A nil hook clears any prior
// observer; calling SetObserver on a nil receiver is a no-op (the
// tracker is "disabled" so no observations are possible). Issue #70.
func (st *SpendTracker) SetObserver(observer BudgetObserverFunc) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.observer = observer
}

// RunningTotal returns the current spend inside the rolling window.
// It is identical to CurrentSpend but exposes the issue-#70 gauge
// name nexus_budget_spend_usd so the collector can render the
// rolling total separately from the cumulative RecordedUSD counter.
func (st *SpendTracker) RunningTotal() float64 {
	return st.CurrentSpend()
}

// NewSpendTracker creates a SpendTracker with the given daily budget
// and a 24-hour rolling window. A dailyBudget <= 0 returns nil so
// callers can gate "disabled" off a single nil-check at the use site,
// exactly like ratelimit.New and concurrencylimit.New.
func NewSpendTracker(dailyBudgetUSD float64) *SpendTracker {
	if dailyBudgetUSD <= 0 {
		return nil
	}
	return &SpendTracker{
		window:  defaultWindow,
		budget:  dailyBudgetUSD,
		entries: make([]entry, ringCapacity, ringCapacity), // pre-allocated ring buffer
	}
}

// pruneLocked removes entries older than the rolling window. Must be
// called with the mutex held. After pruning, the entry at head is the
// oldest surviving record (or the ring is empty).
//
// Ring-buffer pruning is O(1) — just advance the head pointer — vs
// the original O(n) forward scan + copy (issue #987).
func (st *SpendTracker) pruneLocked(now time.Time) {
	if st.count == 0 {
		return
	}
	cutoff := now.Add(-st.window)

	// Count how many expired entries are at the head of the ring.
	expired := 0
	for expired < st.count {
		idx := (st.head + expired) % cap(st.entries)
		if !st.entries[idx].at.Before(cutoff) {
			break
		}
		expired++
	}
	if expired > 0 {
		st.head = (st.head + expired) % cap(st.entries)
		st.count -= expired
	}
}

// sumLocked returns the total spend inside the rolling window. Must
// be called with the mutex held and after pruneLocked.
func (st *SpendTracker) sumLocked() float64 {
	if st.count == 0 {
		return 0
	}
	var total float64
	for i := 0; i < st.count; i++ {
		idx := (st.head + i) % cap(st.entries)
		total += st.entries[idx].amount
	}
	return total
}

// WouldExceed reports whether recording amount would push the
// rolling 24-hour spend past the configured daily budget. The check
// is advisory: it does not reserve the amount, so concurrent
// requests that all pass the check can collectively exceed the cap.
// This is acceptable for a local-development guardrail — the
// alternative (a hard reservation) would require undoing the
// reservation on failure, which is out of scope for issue #38.
//
// When the configured observer is non-nil and this call returns
// true, the observer is invoked with event="exceeded" and the
// (would-have-been) amount so the Prometheus collector can count
// budget hits without a separate wrapper.
//
// A nil receiver returns false (never blocks) so the handler can
// call WouldExceed unconditionally.
func (st *SpendTracker) WouldExceed(amount float64) bool {
	if st == nil {
		return false
	}
	if amount <= 0 {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(time.Now())
	current := st.sumLocked()
	over := current+amount > st.budget
	if over && st.observer != nil {
		st.observer(ObserverEventExceeded, amount)
	}
	return over
}

// Record adds amount to the rolling window. Called after a
// frontier-route request completes (success or upstream error — the
// frontier API consumed tokens either way). Safe for concurrent use.
//
// When the configured observer is non-nil it is invoked with
// event="spent" and the recorded amount so the Prometheus collector
// can accumulate cumulative recorded USD without a separate wrapper.
//
// A nil receiver is a no-op.
func (st *SpendTracker) Record(amount float64) {
	if st == nil || amount <= 0 {
		return
	}
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(now)

	// Grow ring if at capacity (rare — only pathological traffic exceeds ringCapacity).
	if st.count == cap(st.entries) {
		st.growLocked()
	}

	st.entries[st.tail] = entry{at: now, amount: amount}
	st.tail = (st.tail + 1) % cap(st.entries)
	st.count++

	if st.observer != nil {
		st.observer(ObserverEventSpent, amount)
	}
}

// growLocked doubles the ring capacity and copies existing entries in
// chronological order to the new backing array. Must be called with
// the mutex held.
func (st *SpendTracker) growLocked() {
	newCap := cap(st.entries) * 2
	newEntries := make([]entry, newCap, newCap)

	// Copy entries in logical order (head → oldest, wrapping).
	for i := 0; i < st.count; i++ {
		oldIdx := (st.head + i) % cap(st.entries)
		newEntries[i] = st.entries[oldIdx]
	}
	st.entries = newEntries
	st.head = 0
	st.tail = st.count
}

// CurrentSpend returns the sum of all entries inside the rolling
// window. Exported for /healthz and operator introspection.
func (st *SpendTracker) CurrentSpend() float64 {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(time.Now())
	return st.sumLocked()
}

// Budget returns the configured daily cap. Zero when disabled.
func (st *SpendTracker) Budget() float64 {
	if st == nil {
		return 0
	}
	return st.budget
}

// RetryAfter returns a hint for how long the client should wait
// before retrying when the budget is exhausted. It is the time until
// the oldest entry in the window expires (which would free up that
// portion of the budget). Returns 0 when the window is empty.
func (st *SpendTracker) RetryAfter() time.Duration {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(time.Now())
	if st.count == 0 {
		return 0
	}
	oldest := st.entries[st.head].at
	reset := oldest.Add(st.window)
	d := time.Until(reset)
	if d < 0 {
		return 0
	}
	return d
}
