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
type SpendTracker struct {
	mu       sync.Mutex
	window   time.Duration
	budget   float64
	entries  []entry
	observer BudgetObserverFunc
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
		entries: make([]entry, 0, 128),
	}
}

// pruneLocked removes entries older than the rolling window. Must be
// called with the mutex held. After pruning, entries[0] is the oldest
// surviving record (or the slice is empty). Uses a simple forward
// scan rather than a ring buffer: the entry count is bounded by the
// request rate, which for a local-development proxy is at most a few
// thousand per day.
func (st *SpendTracker) pruneLocked(now time.Time) {
	cutoff := now.Add(-st.window)
	// Find the first surviving entry.
	idx := 0
	for idx < len(st.entries) && st.entries[idx].at.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		// Shift surviving entries to the front and reslice.
		copy(st.entries, st.entries[idx:])
		st.entries = st.entries[:len(st.entries)-idx]
	}
}

// sumLocked returns the total spend inside the rolling window. Must
// be called with the mutex held and after pruneLocked.
func (st *SpendTracker) sumLocked() float64 {
	var total float64
	for _, e := range st.entries {
		total += e.amount
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
	st.entries = append(st.entries, entry{at: now, amount: amount})
	if st.observer != nil {
		st.observer(ObserverEventSpent, amount)
	}
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
	if len(st.entries) == 0 {
		return 0
	}
	oldest := st.entries[0].at
	reset := oldest.Add(st.window)
	d := time.Until(reset)
	if d < 0 {
		return 0
	}
	return d
}
