// Package budget implements a rolling 24h spend guard for frontier API calls
// (issue #183) with opt-in alerting (issue #201). It tracks USD costs over
// a sliding window and enforces a configurable daily limit, returning 429
// when the limit is exhausted.
//
// The Guard is safe to call from many goroutines concurrently. The hot path
// (Check) is a single mutex lock + window eviction — O(window entries) worst
// case but typically O(1) since expired entries are evicted in batches.
//
// Alerting (issue #201): When an Alerter is set via SetAlerter, it receives
// callbacks when spend is recorded and when the budget is exceeded. This
// enables webhook, log-level-bump, and Prometheus-metric alerting patterns.
package budget

import (
	"context"
	"sort"
	"sync"
	"time"
)

const (
	// Window is the rolling spend window (24h).
	Window = 24 * time.Hour
)

// Entry is one recorded frontier call cost with its timestamp and source.
type Entry struct {
	At     time.Time
	Cost   float64 // USD
	Source string  // label such as "frontier" or "judge"
}

// State is the budget guard's read-only snapshot returned by State().
type State struct {
	Spent     float64 // total USD spent in the rolling 24h window
	Limit     float64 // configured daily limit in USD
	Remaining float64 // limit - spent; negative means over budget
	Exhausted bool    // true when spent >= limit
	NextReset time.Time
}

// Guard tracks frontier spend over a rolling 24h window. A zero Guard is
// valid but always returns Exhausted=false from Check (no limit enforcement)
// until a positive Limit is configured.
type Guard struct {
	mu      sync.Mutex
	limit   float64
	window  []Entry // sorted by At, oldest first
	alerter Alerter
}

// Alerter is invoked by Guard when spend events occur (issue #201).
// It receives a context (for cancellation/timeout propagation) and the
// current state.
type Alerter interface {
	// OnExceed is called when a request would exceed the budget.
	OnExceed(context.Context, State)
	// OnSpend is called after a spend amount is recorded. The source
	// label (e.g. "frontier" or "judge") allows the alerter to
	// attribute spend to its origin.
	OnSpend(context.Context, State, float64, string)
	// OnApproaching is called when spend crosses the approaching threshold
	// (e.g., 80% of limit). It is called at most once per threshold crossing.
	OnApproaching(context.Context, State)
}

// SetAlerter installs an alerter. A nil alerter disables alerting (the
// guard continues to enforce the limit without sending alerts).
func (g *Guard) SetAlerter(a Alerter) {
	g.mu.Lock()
	g.alerter = a
	g.mu.Unlock()
}

// approachingSent tracks whether we've already sent an "approaching" alert
// since the last reset. It is protected by g.mu.
var alreadySent = false

// NewGuard returns a Guard with the configured daily limit in USD.
// A zero or negative limit disables enforcement (Check always returns false).
func NewGuard(limitUSD float64) *Guard {
	return &Guard{limit: limitLimit(limitUSD)}
}

// limitLimit normalizes the limit: zero or negative becomes 0 (disabled).
func limitLimit(limitUSD float64) float64 {
	if limitUSD <= 0 {
		return 0
	}
	return limitUSD
}

// Check reports whether recording a frontier call of the given cost would
// exceed the daily budget. It evicts entries older than Window before the
// check. Returns false when the guard has no limit configured or the
// result would be within budget; true when the request should be rejected
// with a 429.
func (g *Guard) Check(ctx context.Context, cost float64) bool {
	g.mu.Lock()
	g.evictLocked()
	if g.limit <= 0 {
		g.mu.Unlock()
		return false
	}
	over := g.currentSpentLocked()+cost > g.limit
	if over && g.alerter != nil {
		g.alerter.OnExceed(ctx, g.copyStateLocked())
	}
	g.mu.Unlock()
	return over
}

// Record registers a frontier call cost in the rolling window. It evicts
// stale entries before inserting. Calling Record with cost=0 or when the
// guard is disabled (limit=0) is a no-op.
//
// The source label (e.g. "frontier" or "judge") is stored with the entry
// and passed to the alerter's OnSpend callback so spend can be attributed.
//
// If an alerter is set, it is called with the updated state after recording.
func (g *Guard) Record(ctx context.Context, cost float64, source string) {
	if cost <= 0 {
		return
	}
	g.mu.Lock()
	if g.limit <= 0 {
		g.mu.Unlock()
		return
	}
	g.evictLocked()
	g.window = append(g.window, Entry{At: time.Now(), Cost: cost, Source: source})
	state := g.copyStateLocked()
	g.mu.Unlock()

	if g.alerter != nil {
		g.alerter.OnSpend(ctx, state, cost, source)
	}
}

// CheckApproaching reports whether spend has crossed the approaching
// threshold (80% of limit) since the last call. It returns true exactly
// once per threshold crossing; subsequent calls return false until spend
// drops back below the threshold and crosses it again.
//
// If an alerter is set, OnApproaching is called when the threshold is
// first crossed.
func (g *Guard) CheckApproaching(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.evictLocked()
	if g.limit <= 0 {
		return false
	}
	spent := g.currentSpentLocked()
	threshold := g.limit * 0.8
	if spent >= threshold && !alreadySent {
		alreadySent = true
		if g.alerter != nil {
			g.alerter.OnApproaching(ctx, g.copyStateLocked())
		}
		return true
	}
	if spent < threshold {
		alreadySent = false
	}
	return false
}

// State returns a snapshot of the current budget state. It evicts stale
// entries before computing the total. The returned State is immutable.
func (g *Guard) State() State {
	g.mu.Lock()
	g.evictLocked()
	s := g.copyStateLocked()
	g.mu.Unlock()
	return s
}

// Limit returns the configured daily limit in USD.
func (g *Guard) Limit() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// SetLimit updates the daily limit. A zero or negative value disables
// enforcement (Check always returns false). Resetting the limit also
// resets the "approaching" alert flag.
func (g *Guard) SetLimit(limitUSD float64) {
	g.mu.Lock()
	g.limit = limitLimit(limitUSD)
	alreadySent = false
	g.mu.Unlock()
}

// evictLocked removes entries older than Window while holding mu.
// Caller must hold g.mu.
func (g *Guard) evictLocked() {
	if len(g.window) == 0 {
		return
	}
	cutoff := time.Now().Add(-Window)
	i := sort.Search(len(g.window), func(i int) bool {
		return !g.window[i].At.Before(cutoff)
	})
	if i > 0 {
		g.window = append(g.window[:0:0], g.window[i:]...)
	}
}

// currentSpentLocked returns the sum of all entry costs in the window.
// Caller must hold g.mu.
func (g *Guard) currentSpentLocked() float64 {
	var total float64
	for _, e := range g.window {
		total += e.Cost
	}
	return total
}

// copyStateLocked returns a State snapshot. Caller must hold g.mu.
func (g *Guard) copyStateLocked() State {
	s := State{
		Spent:     g.currentSpentLocked(),
		Limit:     g.limit,
		Remaining: g.limit - g.currentSpentLocked(),
		Exhausted: g.limit > 0 && g.currentSpentLocked() >= g.limit,
	}
	if len(g.window) > 0 {
		s.NextReset = g.window[0].At.Add(Window)
	} else {
		s.NextReset = time.Now().Add(Window)
	}
	return s
}
