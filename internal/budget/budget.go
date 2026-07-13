// Package budget implements a rolling 24h spend guard for frontier API calls
// (issue #183). It tracks USD costs over a sliding window and enforces a
// configurable daily limit, returning 429 when the limit is exhausted.
//
// The Guard is safe to call from many goroutines concurrently. The hot path
// (Check) is a single mutex lock + window eviction — O(window entries) worst
// case but typically O(1) since expired entries are evicted in batches.
package budget

import (
	"sort"
	"sync"
	"time"
)

const (
	// Window is the rolling spend window (24h).
	Window = 24 * time.Hour
)

// Entry is one recorded frontier call cost with its timestamp.
type Entry struct {
	At   time.Time
	Cost float64 // USD
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
	mu     sync.Mutex
	limit  float64
	window []Entry // sorted by At, oldest first
}

// NewGuard returns a Guard with the configured daily limit in USD.
// A zero or negative limit disables enforcement (Check always returns false).
func NewGuard(limitUSD float64) *Guard {
	g := &Guard{limit: limitUSD}
	if g.limit <= 0 {
		g.limit = 0
	}
	return g
}

// Check reports whether recording a frontier call of the given cost would
// exceed the daily budget. It evicts entries older than Window before the
// check. Returns false when the guard has no limit configured or the
// result would be within budget; true when the request should be rejected
// with a 429.
func (g *Guard) Check(cost float64) bool {
	g.mu.Lock()
	g.evictLocked()
	if g.limit <= 0 {
		g.mu.Unlock()
		return false
	}
	over := g.currentSpentLocked()+cost > g.limit
	g.mu.Unlock()
	return over
}

// Record registers a frontier call cost in the rolling window. It evicts
// stale entries before inserting. Calling Record with cost=0 or when the
// guard is disabled (limit=0) is a no-op.
func (g *Guard) Record(cost float64) {
	if cost <= 0 {
		return
	}
	g.mu.Lock()
	if g.limit <= 0 {
		g.mu.Unlock()
		return
	}
	g.evictLocked()
	g.window = append(g.window, Entry{At: time.Now(), Cost: cost})
	g.mu.Unlock()
}

// State returns a snapshot of the current budget state. It evicts stale
// entries before computing the total. The returned State is immutable.
func (g *Guard) State() State {
	g.mu.Lock()
	g.evictLocked()
	s := State{
		Spent:     g.currentSpentLocked(),
		Limit:     g.limit,
		Remaining: g.limit - g.currentSpentLocked(),
		Exhausted: g.limit > 0 && g.currentSpentLocked() >= g.limit,
	}
	g.mu.Unlock()
	if len(g.window) > 0 {
		s.NextReset = g.window[0].At.Add(Window)
	} else {
		s.NextReset = time.Now().Add(Window)
	}
	return s
}

// Limit returns the configured daily limit in USD.
func (g *Guard) Limit() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// SetLimit updates the daily limit. A zero or negative value disables
// enforcement (Check always returns false).
func (g *Guard) SetLimit(limitUSD float64) {
	g.mu.Lock()
	g.limit = limitUSD
	if g.limit <= 0 {
		g.limit = 0
	}
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
