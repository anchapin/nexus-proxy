package budget

import (
	"log/slog"
	"strings"
	"testing"
)

// TestPrometheusAlerterOnExceed verifies OnExceed increments counter and logs.
func TestPrometheusAlerterOnExceed(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	a := NewPrometheusAlerter(logger)

	state := State{Spent: 95, Limit: 100, Remaining: 5, Exhausted: true}
	a.OnExceed(state)

	if a.ExceedTotal() != 1 {
		t.Errorf("ExceedTotal = %d, want 1", a.ExceedTotal())
	}
	if !strings.Contains(logBuf.String(), "budget exceeded") {
		t.Error("log should contain 'budget exceeded'")
	}
}

// TestPrometheusAlerterOnApproaching verifies OnApproaching increments counter.
func TestPrometheusAlerterOnApproaching(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	a := NewPrometheusAlerter(logger)

	state := State{Spent: 82, Limit: 100, Remaining: 18, Exhausted: false}
	a.OnApproaching(state)

	if a.ApproachingTotal() != 1 {
		t.Errorf("ApproachingTotal = %d, want 1", a.ApproachingTotal())
	}
	if !strings.Contains(logBuf.String(), "budget approaching") {
		t.Error("log should contain 'budget approaching'")
	}
}

// TestPrometheusAlerterOnSpend verifies OnSpend stores the amount.
func TestPrometheusAlerterOnSpend(t *testing.T) {
	a := NewPrometheusAlerter(nil) // no logger

	state := State{Spent: 10, Limit: 100, Remaining: 90, Exhausted: false}
	a.OnSpend(state, 10.0)

	if a.LastSpentAmount() != 10.0 {
		t.Errorf("LastSpentAmount = %.2f, want 10.0", a.LastSpentAmount())
	}
	got := a.State()
	if got.Spent != state.Spent {
		t.Errorf("State().Spent = %.2f, want %.2f", got.Spent, state.Spent)
	}
}

// TestPrometheusAlerterMultipleExceed verifies counter increments.
func TestPrometheusAlerterMultipleExceed(t *testing.T) {
	a := NewPrometheusAlerter(nil)
	state := State{Spent: 100, Limit: 100, Remaining: 0, Exhausted: true}

	a.OnExceed(state)
	a.OnExceed(state)
	a.OnExceed(state)

	if a.ExceedTotal() != 3 {
		t.Errorf("ExceedTotal = %d, want 3", a.ExceedTotal())
	}
}

// TestPrometheusAlerterNilLoggerNoPanic verifies nil logger doesn't panic.
func TestPrometheusAlerterNilLoggerNoPanic(t *testing.T) {
	a := NewPrometheusAlerter(nil)
	state := State{Spent: 100, Limit: 100, Remaining: 0, Exhausted: true}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic with nil logger: %v", r)
			}
		}()
		a.OnExceed(state)
		a.OnApproaching(state)
		a.OnSpend(state, 5.0)
	}()
}

// TestPrometheusAlerterStateAccess verifies State returns latest.
func TestPrometheusAlerterStateAccess(t *testing.T) {
	a := NewPrometheusAlerter(nil)

	initial := State{Spent: 0, Limit: 100, Remaining: 100, Exhausted: false}
	a.OnSpend(initial, 0)

	afterSpend := State{Spent: 25, Limit: 100, Remaining: 75, Exhausted: false}
	a.OnSpend(afterSpend, 25.0)

	got := a.State()
	if got.Spent != 25.0 {
		t.Errorf("State().Spent = %.2f, want 25.0", got.Spent)
	}
}
