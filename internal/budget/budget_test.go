package budget

import (
	"math"
	"sort"
	"testing"
	"time"
)

func TestGuardZeroValue(t *testing.T) {
	g := &Guard{}
	if g.Check(1.0) {
		t.Error("zero Guard.Check should return false (no limit)")
	}
	g.Record(1.0)
	if len(g.window) != 0 {
		t.Error("zero Guard.Record should not store entries")
	}
}

func TestGuardDisabled(t *testing.T) {
	g := NewGuard(0)
	if g.Check(999999) {
		t.Error("Check(999999) on limit=0 guard should return false")
	}
	g.Record(100)
	if len(g.window) != 0 {
		t.Error("disabled guard should not store entries")
	}
}

func TestGuardCheckAllowsWithinBudget(t *testing.T) {
	g := NewGuard(10.0)
	if g.Check(5.0) {
		t.Error("Check(5.0) with limit=10 should be allowed")
	}
	if g.Check(10.0) {
		t.Error("Check(10.0) exactly at limit should be allowed")
	}
}

func TestGuardCheckRejectsOverBudget(t *testing.T) {
	g := NewGuard(10.0)
	if !g.Check(10.01) {
		t.Error("Check(10.01) over limit=10 should be rejected")
	}
}

func TestGuardRecordAndState(t *testing.T) {
	g := NewGuard(10.0)
	g.Record(3.0)
	g.Record(2.0)

	state := g.State()
	if state.Spent != 5.0 {
		t.Errorf("expected spent=5.0, got %v", state.Spent)
	}
	if state.Limit != 10.0 {
		t.Errorf("expected limit=10.0, got %v", state.Limit)
	}
	if state.Remaining != 5.0 {
		t.Errorf("expected remaining=5.0, got %v", state.Remaining)
	}
	if state.Exhausted {
		t.Error("should not be exhausted at spent=5, limit=10")
	}
}

func TestGuardExhausted(t *testing.T) {
	g := NewGuard(5.0)
	g.Record(3.0)
	if g.Check(1.0) {
		t.Error("Check(1.0) should fit in remaining 2.0")
	}
	if !g.Check(3.0) {
		t.Error("Check(3.0) should NOT fit in remaining 2.0")
	}
	g.Record(2.0) // now spent=5.0, limit=5.0
	state := g.State()
	if !state.Exhausted {
		t.Error("should be exhausted at spent=limit=5.0")
	}
	if state.Remaining != 0 {
		t.Errorf("expected remaining=0, got %v", state.Remaining)
	}
}

func TestGuardEviction(t *testing.T) {
	g := &Guard{limit: 10.0}
	// Simulate old entries
	now := time.Now()
	g.window = []Entry{
		{At: now.Add(-25 * time.Hour), Cost: 5.0},
		{At: now.Add(-23 * time.Hour), Cost: 3.0},
		{At: now.Add(-1 * time.Hour), Cost: 1.0},
	}

	state := g.State()
	// T-25h evicted (>24h), T-23h and T-1h kept (both within 24h window)
	// spent = 3.0 + 1.0 = 4.0
	if state.Spent != 4.0 {
		t.Errorf("expected spent=4.0 after eviction, got %v", state.Spent)
	}
}

func TestGuardEvictionAllExpired(t *testing.T) {
	g := &Guard{limit: 10.0}
	now := time.Now()
	g.window = []Entry{
		{At: now.Add(-48 * time.Hour), Cost: 5.0},
		{At: now.Add(-25 * time.Hour), Cost: 3.0},
	}
	state := g.State()
	if state.Spent != 0 {
		t.Errorf("expected spent=0 when all entries expired, got %v", state.Spent)
	}
}

func TestGuardRecordZeroOrNegative(t *testing.T) {
	g := NewGuard(10.0)
	g.Record(0)
	g.Record(-1.0)
	if len(g.window) != 0 {
		t.Error("Record(0) and Record(-1) should not add entries")
	}
}

func TestGuardSetLimit(t *testing.T) {
	g := NewGuard(10.0)
	g.Record(5.0)
	g.SetLimit(3.0)
	state := g.State()
	if state.Limit != 3.0 {
		t.Errorf("expected limit=3.0, got %v", state.Limit)
	}
	// Spent is still 5.0, remaining is -2.0
	if state.Remaining != -2.0 {
		t.Errorf("expected remaining=-2.0, got %v", state.Remaining)
	}
}

func TestGuardConcurrentAccess(t *testing.T) {
	g := NewGuard(100.0)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			g.Record(0.1)
			g.Check(0.1)
		}
		select {
		case <-done:
		default:
		}
	}()
	go func() {
		for i := 0; i < 100; i++ {
			g.Record(0.1)
			g.Check(0.1)
		}
		select {
		case <-done:
		default:
		}
	}()
	close(done)
	// No crash = test passes
}

func TestGuardNextReset(t *testing.T) {
	g := NewGuard(10.0)
	state := g.State()
	// NextReset should be approximately 24h from now
	expectedMin := time.Now().Add(Window - time.Second)
	expectedMax := time.Now().Add(Window + time.Second)
	if state.NextReset.Before(expectedMin) || state.NextReset.After(expectedMax) {
		t.Errorf("NextReset out of expected range: got %v, expected between %v and %v",
			state.NextReset, expectedMin, expectedMax)
	}
}

func TestGuardNextResetWithEntries(t *testing.T) {
	g := &Guard{limit: 10.0}
	now := time.Now()
	g.window = []Entry{
		{At: now.Add(-1 * time.Hour), Cost: 1.0},
	}
	state := g.State()
	// NextReset should be 24h after the oldest entry
	expectedReset := now.Add(-1 * time.Hour).Add(Window)
	if state.NextReset.Unix() != expectedReset.Unix() {
		t.Errorf("expected NextReset=%v, got %v", expectedReset, state.NextReset)
	}
}

func TestNewGuardNegative(t *testing.T) {
	g := NewGuard(-5.0)
	if g.limit != 0 {
		t.Errorf("expected limit=0 for negative input, got %v", g.limit)
	}
}

func TestGuardSpentRounding(t *testing.T) {
	g := NewGuard(10.0)
	g.Record(0.123456)
	g.Record(0.654321)
	state := g.State()
	if math.Abs(state.Spent-0.777777) > 0.0001 {
		t.Errorf("expected spent≈0.777777, got %v", state.Spent)
	}
}

func TestGuardCheckOrder(t *testing.T) {
	// Test that Check() evictOldEntries first, so an entry
	// that would have expired doesn't contribute to the check.
	g := &Guard{limit: 5.0}
	now := time.Now()
	g.window = []Entry{
		{At: now.Add(-25 * time.Hour), Cost: 4.0}, // would expire
	}
	// Without eviction, Check(2.0) would say "over" (4+2=6 > 5)
	// After eviction, Check(2.0) should say "ok" (no entries)
	if g.Check(2.0) {
		t.Error("Check(2.0) should be allowed after eviction of old entries")
	}
}

func TestGuardStateDeterministic(t *testing.T) {
	g := NewGuard(10.0)
	g.Record(1.0)
	g.Record(2.0)
	g.Record(3.0)

	states := make([]State, 5)
	for i := 0; i < 5; i++ {
		states[i] = g.State()
	}
	for i := 1; i < 5; i++ {
		if states[i] != states[0] {
			t.Errorf("State() not deterministic: %v vs %v", states[i], states[0])
		}
	}
}

var _ sort.Interface = (*EntrySlice)(nil)

type EntrySlice []Entry

func (e EntrySlice) Len() int { return len(e) }
func (e EntrySlice) Less(i, j int) bool {
	return e[i].At.Before(e[j].At)
}
func (e EntrySlice) Swap(i, j int) { e[i], e[j] = e[j], e[i] }
