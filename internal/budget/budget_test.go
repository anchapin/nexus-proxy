package budget

import (
	"context"
	"testing"
	"time"
)

// TestGuardCheckWithinBudget verifies Check returns false when the cost
// would not exceed the budget.
func TestGuardCheckWithinBudget(t *testing.T) {
	g := NewGuard(100.0)
	if g.Check(context.Background(), 50.0) {
		t.Error("Check(50) on $100 budget should return false")
	}
}

// TestGuardCheckExceedsBudget verifies Check returns true when the cost
// would exceed the budget.
func TestGuardCheckExceedsBudget(t *testing.T) {
	g := NewGuard(100.0)
	if !g.Check(context.Background(), 150.0) {
		t.Error("Check(150) on $100 budget should return true")
	}
}

// TestGuardCheckDisabled returns false when limit is 0.
func TestGuardCheckDisabled(t *testing.T) {
	g := NewGuard(0)
	if g.Check(context.Background(), 1000.0) {
		t.Error("Check on disabled guard (limit=0) should always return false")
	}
}

// TestGuardRecordAndState verifies Record updates State correctly.
func TestGuardRecordAndState(t *testing.T) {
	g := NewGuard(100.0)
	g.Record(context.Background(), 25.0, "frontier")
	g.Record(context.Background(), 30.0, "frontier")

	state := g.State()
	if state.Spent != 55.0 {
		t.Errorf("spent=%.2f, want 55.0", state.Spent)
	}
	if state.Remaining != 45.0 {
		t.Errorf("remaining=%.2f, want 45.0", state.Remaining)
	}
	if state.Exhausted {
		t.Error("Exhausted should be false when spent < limit")
	}
	if state.Limit != 100.0 {
		t.Errorf("limit=%.2f, want 100.0", state.Limit)
	}
}

// TestGuardExhausted verifies Exhausted is true when spent >= limit.
func TestGuardExhausted(t *testing.T) {
	g := NewGuard(100.0)
	g.Record(context.Background(), 100.0, "frontier")

	state := g.State()
	if !state.Exhausted {
		t.Error("Exhausted should be true when spent >= limit")
	}
}

// TestGuardRecordZeroOrNegative is a no-op.
func TestGuardRecordZeroOrNegative(t *testing.T) {
	g := NewGuard(100.0)
	g.Record(context.Background(), 0.0, "frontier")
	g.Record(context.Background(), -10.0, "frontier")

	state := g.State()
	if state.Spent != 0.0 {
		t.Errorf("spent after zero/negative record = %.2f, want 0.0", state.Spent)
	}
}

// TestGuardCheckDisabledNeverBlocks verifies that Check on a disabled
// guard returns false even when spend would exceed the limit.
func TestGuardCheckDisabledNeverBlocks(t *testing.T) {
	g := NewGuard(0) // disabled
	g.Record(context.Background(), 1000.0, "frontier")
	if g.Check(context.Background(), 50.0) {
		t.Error("Check on disabled guard should always return false")
	}
}

// TestGuardSetLimit updates the limit dynamically.
func TestGuardSetLimit(t *testing.T) {
	g := NewGuard(100.0)
	g.Record(context.Background(), 50.0, "frontier")

	g.SetLimit(40.0)
	state := g.State()
	if state.Remaining >= 0 {
		t.Error("after SetLimit(40), remaining should be negative (over budget)")
	}
}

// TestGuardCheckAfterSetLimit verifies Check respects the new limit.
func TestGuardCheckAfterSetLimit(t *testing.T) {
	g := NewGuard(100.0)
	g.SetLimit(30.0)

	if !g.Check(context.Background(), 40.0) {
		t.Error("Check(40) on $30 limit should return true")
	}
	if g.Check(context.Background(), 20.0) {
		t.Error("Check(20) on $30 limit should return false")
	}
}

// TestGuardStateNextReset verifies NextReset is set correctly.
func TestGuardStateNextReset(t *testing.T) {
	g := NewGuard(100.0)
	g.Record(context.Background(), 10.0, "frontier")

	state := g.State()
	if state.NextReset.IsZero() {
		t.Error("NextReset should not be zero after recording spend")
	}
	// NextReset should be approximately window from now
	expected := time.Now().Add(Window)
	diff := state.NextReset.Sub(expected)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("NextReset off by %v, want ~%v", diff, expected)
	}
}

// TestGuardNoSpendNextReset verifies NextReset when no spend recorded.
func TestGuardNoSpendNextReset(t *testing.T) {
	g := NewGuard(100.0)
	state := g.State()
	if state.NextReset.IsZero() {
		t.Error("NextReset should not be zero even with no spend")
	}
}

// TestGuardLimitAccessor verifies Limit() returns the configured limit.
func TestGuardLimitAccessor(t *testing.T) {
	g := NewGuard(42.5)
	if g.Limit() != 42.5 {
		t.Errorf("Limit() = %.2f, want 42.5", g.Limit())
	}
}

// TestGuardSourceLabel verifies that the source label is stored with
// each entry and surfaced in the alerter callback.
func TestGuardSourceLabel(t *testing.T) {
	var lastSource string
	g := NewGuard(100.0)
	g.SetAlerter(AlerterFunc(func(_ context.Context, _ State, _ float64, source string) {
		lastSource = source
	}))

	g.Record(context.Background(), 10.0, "frontier")
	if lastSource != "frontier" {
		t.Errorf("source = %q, want frontier", lastSource)
	}

	g.Record(context.Background(), 5.0, "judge")
	if lastSource != "judge" {
		t.Errorf("source = %q, want judge", lastSource)
	}
}

// AlerterFunc is a thin adapter so tests can use a plain function as
// an Alerter without defining an interface implementation.
type AlerterFunc func(context.Context, State, float64, string)

func (f AlerterFunc) OnExceed(ctx context.Context, s State) {}
func (f AlerterFunc) OnSpend(ctx context.Context, state State, cost float64, src string) {
	f(ctx, state, cost, src)
}
func (f AlerterFunc) OnApproaching(ctx context.Context, s State) {}

// TestGuardNilAlerterNoPanic verifies that operations don't panic when
// no alerter is set.
func TestGuardNilAlerterNoPanic(t *testing.T) {
	g := NewGuard(100.0)
	g.SetAlerter(nil) // explicitly nil

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("operations panicked with nil alerter: %v", r)
			}
		}()
		g.Check(context.Background(), 50.0)
		g.Record(context.Background(), 50.0, "frontier")
		g.CheckApproaching(context.Background())
	}()
}
