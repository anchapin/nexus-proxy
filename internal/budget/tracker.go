// Package budget provides a rolling 24-hour spend tracker for
// frontier-route requests (issue #38). The tracker maintains an
// in-memory window of (timestamp, amount) pairs; WouldExceed is
// consulted before dispatching to the frontier, and Record is called
// after the request completes. When the daily budget is zero or
// unset, the tracker is disabled (NewSpendTracker returns nil) and
// both methods are no-ops — preserving the pre-issue-#38 behaviour.
package budget

import (
	"sort"
	"sync"
	"time"
)

const defaultWindow = 24 * time.Hour

type entry struct {
	at     time.Time
	amount float64
}

type BudgetObserverFunc func(event string, amount float64)

const (
	ObserverEventSpent    = "spent"
	ObserverEventExceeded = "exceeded"
)

type bucket struct {
	entries  []entry
	sum      float64
	oldestAt time.Time
}

type SpendTracker struct {
	mu       sync.Mutex
	window   time.Duration
	budget   float64
	observer BudgetObserverFunc

	buckets    map[int64]*bucket
	sortedKeys []int64
	total      float64
}

func NewSpendTracker(dailyBudgetUSD float64) *SpendTracker {
	if dailyBudgetUSD <= 0 {
		return nil
	}
	return &SpendTracker{
		window:  defaultWindow,
		budget:  dailyBudgetUSD,
		buckets: make(map[int64]*bucket),
	}
}

func (st *SpendTracker) SetObserver(observer BudgetObserverFunc) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.observer = observer
}

func (st *SpendTracker) RunningTotal() float64 {
	return st.CurrentSpend()
}

func (st *SpendTracker) bucketKey(t time.Time) int64 {
	return t.Unix() / 60
}

func (st *SpendTracker) pruneLocked(now time.Time) {
	if len(st.buckets) == 0 {
		return
	}
	cutoff := now.Add(-st.window)

	i := 0
	for i < len(st.sortedKeys) {
		bk := st.sortedKeys[i]
		b := st.buckets[bk]

		if b.oldestAt.Before(cutoff) {
			st.total -= b.sum
			delete(st.buckets, bk)
			i++
			continue
		}

		j := 0
		for j < len(b.entries) && b.entries[j].at.Before(cutoff) {
			j++
		}
		if j > 0 {
			expired := b.entries[:j]
			for _, e := range expired {
				st.total -= e.amount
			}
			b.entries = b.entries[j:]
			if len(b.entries) > 0 {
				b.oldestAt = b.entries[0].at
			}
			b.sum = 0
			for _, e := range b.entries {
				b.sum += e.amount
			}
			if len(b.entries) == 0 {
				delete(st.buckets, bk)
				i++
			}
		}
		break
	}
	if i > 0 {
		st.sortedKeys = st.sortedKeys[i:]
	}
}

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
	over := st.total+amount > st.budget
	if over && st.observer != nil {
		st.observer(ObserverEventExceeded, amount)
	}
	return over
}

func (st *SpendTracker) Record(amount float64) {
	if st == nil || amount <= 0 {
		return
	}
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(now)

	bk := st.bucketKey(now)
	b, ok := st.buckets[bk]
	if !ok {
		b = &bucket{oldestAt: now}
		st.buckets[bk] = b
		st.sortedKeys = append(st.sortedKeys, bk)
		sort.Slice(st.sortedKeys, func(i, j int) bool { return st.sortedKeys[i] < st.sortedKeys[j] })
	}
	b.entries = append(b.entries, entry{at: now, amount: amount})
	b.sum += amount
	st.total += amount

	if st.observer != nil {
		st.observer(ObserverEventSpent, amount)
	}
}

func (st *SpendTracker) CurrentSpend() float64 {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(time.Now())
	return st.total
}

func (st *SpendTracker) Budget() float64 {
	if st == nil {
		return 0
	}
	return st.budget
}

func (st *SpendTracker) RetryAfter() time.Duration {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneLocked(time.Now())
	if len(st.sortedKeys) == 0 {
		return 0
	}
	oldestBucket := st.buckets[st.sortedKeys[0]]
	reset := oldestBucket.oldestAt.Add(st.window)
	d := time.Until(reset)
	if d < 0 {
		return 0
	}
	return d
}
