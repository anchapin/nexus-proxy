// Package testutil provides shared test helpers that are safe to use from
// parallel tests across packages.
package testutil

import (
	"log/slog"
	"sync"
	"testing"
)

// defaultMu serializes all access to slog's process-wide default logger
// (slog.SetDefault / slog.Default) from tests. slog.SetDefault mutates a
// global variable, so unsynchronized concurrent calls from parallel tests
// trigger the race detector (issue #1138). Every test that swaps the
// default logger must go through SetDefault so the swap is serialized.
var defaultMu sync.Mutex

// SetDefault replaces slog's process-wide default logger with logger,
// serialized behind the shared defaultMu so that parallel tests do not race
// on slog.SetDefault's global mutation (issue #1138). It registers a cleanup
// on tb that restores the previous logger (also under the mutex).
//
// Prefer this over calling slog.SetDefault directly in tests.
func SetDefault(tb testing.TB, logger *slog.Logger) {
	tb.Helper()
	defaultMu.Lock()
	prev := slog.Default()
	slog.SetDefault(logger)
	defaultMu.Unlock()
	tb.Cleanup(func() {
		defaultMu.Lock()
		slog.SetDefault(prev)
		defaultMu.Unlock()
	})
}
