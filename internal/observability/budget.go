// Package observability exposes in-process counters for routing
// decisions (issue #74). The proxy is stdlib-only by design, so this
// package implements a tiny Prometheus-text-format exposition rather
// than pulling in the official client library. The counters are
// updated synchronously from the chat handler's request goroutine —
// each Observe call is a handful of atomic increments, so the hot
// path pays negligible overhead.
package observability

import (
	"sync"
)

// BudgetMetrics holds the rolling 24h frontier spend state for Prometheus
// exposition (issue #183). It is safe for concurrent use. The zero value
// is ready to use.
type BudgetMetrics struct {
	mu        sync.Mutex
	spent     float64
	limit     float64
	remaining float64
}

// Observe updates the budget window snapshot. Called after each frontier
// call records its cost so the /metrics endpoint is always current.
func (b *BudgetMetrics) Observe(spent, limit, remaining float64) {
	b.mu.Lock()
	b.spent = spent
	b.limit = limit
	b.remaining = remaining
	b.mu.Unlock()
}

// Spent returns the last observed spent value.
func (b *BudgetMetrics) Spent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Limit returns the last observed limit value.
func (b *BudgetMetrics) Limit() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Remaining returns the last observed remaining value.
func (b *BudgetMetrics) Remaining() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}
