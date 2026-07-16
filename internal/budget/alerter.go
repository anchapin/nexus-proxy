// Package budget provides alerting support for the rolling 24h spend guard
// (issue #183, #201). This file provides a Prometheus-based alerter that
// integrates with the observability package.
package budget

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
)

// PrometheusAlerter is an Alerter (issue #201) that updates atomic counters
// for Prometheus collection. It also logs at warn/error level per the
// alert type.
type PrometheusAlerter struct {
	// ExceedCount counts how many times the budget was exceeded.
	// Use atomic.LoadUint64 to read.
	ExceedCount atomic.Uint64

	// ApproachingCount counts how many times spend crossed the
	// approaching threshold.
	ApproachingCount atomic.Uint64

	// LastSpent stores the most recent recorded spend amount as a
	// uint64 encoding of a float64 (math.Float64bits).
	LastSpent atomic.Uint64

	// lastSource stores the source label of the most recent spend event
	// (e.g. "frontier" or "judge"). Accessed via LastSource().
	lastSource atomic.Value // stores string

	// LastState stores the most recent state snapshot.
	LastState atomic.Value // stores State

	// Logger is the structured logger for alert messages. If nil,
	// no log messages are emitted.
	Logger *slog.Logger

	// LogLevel is the slog level for log messages. Defaults to
	// slog.LevelWarn.
	LogLevel slog.Level
}

// NewPrometheusAlerter returns a ready-to-use alerter with an optional
// slog.Logger. When logger is nil, no log messages are emitted.
func NewPrometheusAlerter(logger *slog.Logger) *PrometheusAlerter {
	a := &PrometheusAlerter{
		Logger:   logger,
		LogLevel: slog.LevelWarn,
	}
	a.LastState.Store(State{})
	a.lastSource.Store("")
	return a
}

// OnExceed implements Alerter. It increments the exceed counter, logs
// at error level, and stores the state.
func (a *PrometheusAlerter) OnExceed(ctx context.Context, s State) {
	a.ExceedCount.Add(1)
	a.LastState.Store(s)
	if a.Logger != nil {
		a.Logger.Log(ctx, slog.LevelError, "budget exceeded",
			slog.Float64("spent_usd", s.Spent),
			slog.Float64("limit_usd", s.Limit),
			slog.Float64("remaining_usd", s.Remaining),
		)
	}
}

// OnSpend implements Alerter. It stores the spend amount, source, and state.
// The source label (e.g. "frontier" or "judge") is stored and logged so
// spend can be attributed to its origin.
func (a *PrometheusAlerter) OnSpend(ctx context.Context, s State, amount float64, source string) {
	a.LastSpent.Store(math.Float64bits(amount))
	a.lastSource.Store(source)
	a.LastState.Store(s)
	if a.Logger != nil {
		a.Logger.Log(ctx, slog.LevelInfo, "budget spend recorded",
			slog.Float64("spent_usd", amount),
			slog.Float64("total_spent_usd", s.Spent),
			slog.String("source", source),
		)
	}
}

// OnApproaching implements Alerter. It increments the approaching counter,
// logs at warn level, and stores the state.
func (a *PrometheusAlerter) OnApproaching(ctx context.Context, s State) {
	a.ApproachingCount.Add(1)
	a.LastState.Store(s)
	if a.Logger != nil {
		a.Logger.Log(ctx, a.LogLevel, "budget approaching limit",
			slog.Float64("spent_usd", s.Spent),
			slog.Float64("limit_usd", s.Limit),
			slog.Float64("remaining_usd", s.Remaining),
			slog.Float64("pct_used", s.Spent/s.Limit*100),
		)
	}
}

// ExceedTotal returns the total number of budget-exceeded events.
func (a *PrometheusAlerter) ExceedTotal() uint64 { return a.ExceedCount.Load() }

// ApproachingTotal returns the total number of approaching-threshold events.
func (a *PrometheusAlerter) ApproachingTotal() uint64 { return a.ApproachingCount.Load() }

// LastSpentAmount returns the most recently recorded spend amount.
func (a *PrometheusAlerter) LastSpentAmount() float64 {
	return math.Float64frombits(a.LastSpent.Load())
}

// State returns the most recent state snapshot.
func (a *PrometheusAlerter) State() State { return a.LastState.Load().(State) }

// LastSource returns the source label of the most recent spend event
// (e.g. "frontier" or "judge").
func (a *PrometheusAlerter) LastSource() string { return a.lastSource.Load().(string) }
