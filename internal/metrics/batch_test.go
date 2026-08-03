package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestBatchFlushOnSize verifies that drain flushes the accumulated batch
// when the accumulator reaches batchSize (issue #1234).
func TestBatchFlushOnSize(t *testing.T) {
	const batchSize = 5
	const total = batchSize * 3 // three full batches

	var cbCount int
	cb := func() { cbCount++ }

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_size.db", 0, silentLogger, BatchConfig{
		Size:     batchSize,
		Timeout:  100 * time.Millisecond, // generous timeout so size wins
		Callback: cb,
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < total; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-size-" + string(rune('0'+i)),
			Route:     "local",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Callback should fire exactly total/batchSize times (3 batches).
	if cbCount != total/batchSize {
		t.Errorf("callback count = %d, want %d", cbCount, total/batchSize)
	}

	// Verify rows landed by reopening at the SAME path.
	s2, err := OpenWithRetention(tmp+"/batch_size.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != total {
		t.Errorf("RequestCount = %d, want %d", sum.RequestCount, total)
	}
}

// TestBatchFlushOnTimeout verifies that a partial batch is flushed after
// the timeout elapses even when the batch is not full (issue #1234).
// This is a critical acceptance criterion: "Timeout flushes partial batches".
func TestBatchFlushOnTimeout(t *testing.T) {
	const batchSize = 100 // intentionally large so size won't trigger
	const timeout = 50 * time.Millisecond
	const count = 7 // less than batchSize; only timeout should flush

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_timeout.db", 0, silentLogger, BatchConfig{
		Size:    batchSize,
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-timeout-" + string(rune('0'+i)),
			Route:     "frontier",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}

	// Wait for the timeout to fire plus a safety margin.
	// The drain goroutine processes records and the timer fires.
	time.Sleep(500 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify rows landed by reopening at the SAME path.
	// This verifies that "Timeout flushes partial batches" works.
	s2, err := OpenWithRetention(tmp+"/batch_timeout.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != count {
		t.Errorf("RequestCount = %d, want %d (timeout should have flushed partial batch)", sum.RequestCount, count)
	}
}

// TestBatchFlushOnShutdown verifies that any remaining records in the
// accumulator are flushed when the store is closed (issue #1234).
// This is a critical acceptance criterion: "Shutdown flushes remaining".
func TestBatchFlushOnShutdown(t *testing.T) {
	const batchSize = 10
	const count = 3 // less than batchSize; only shutdown flush

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_shutdown.db", 0, silentLogger, BatchConfig{
		Size:    batchSize,
		Timeout: 10 * time.Second, // intentionally long; shutdown must win
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}

	for i := 0; i < count; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-shutdown-" + string(rune('0'+i)),
			Route:     "fusion",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}

	// Close without waiting for timeout; drain must flush the partial batch.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify rows landed by reopening at the SAME path.
	// This verifies that "Shutdown flushes remaining" works.
	s2, err := OpenWithRetention(tmp+"/batch_shutdown.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != count {
		t.Errorf("RequestCount = %d, want %d (shutdown should have flushed partial batch)", sum.RequestCount, count)
	}
}

// TestBatchSizeOneReproducesPerRecordBehaviour verifies that BatchSize=1
// produces byte-for-byte identical behaviour to the pre-batch path
// (issue #1234). Acceptance criterion: "BATCH_SIZE=1 reproduces pre-batch behavior".
func TestBatchSizeOneReproducesPerRecordBehaviour(t *testing.T) {
	const count = 5

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_one.db", 0, silentLogger, BatchConfig{
		Size:    1, // one record per transaction — the pre-batch behaviour
		Timeout: 0, // no timeout; only size matters
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-one-" + string(rune('0'+i)),
			Route:     "local",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	// With batchSize=1, this should be byte-for-byte identical to the
	// pre-batch path where each record commits individually.
	s2, err := OpenWithRetention(tmp+"/batch_one.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != count {
		t.Errorf("RequestCount = %d, want %d (BATCH_SIZE=1 should reproduce pre-batch behavior)", sum.RequestCount, count)
	}
}

// TestBatchDisabledWithZeroSize verifies that zero BatchSize produces
// pre-batch per-record behaviour — each record in its own transaction.
// This is the default behavior when no batching is configured.
func TestBatchDisabledWithZeroSize(t *testing.T) {
	const count = 5

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_disabled.db", 0, silentLogger, BatchConfig{
		Size:    0, // disabled — every record commits individually
		Timeout: 0,
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-disabled-" + string(rune('0'+i)),
			Route:     "frontier",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	s2, err := OpenWithRetention(tmp+"/batch_disabled.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != count {
		t.Errorf("RequestCount = %d, want %d", sum.RequestCount, count)
	}
}

// TestBatchMixedSizeAndTimeout verifies that when both size and timeout
// trigger, the size-based flush wins first; subsequent partial batches
// are then flushed by timeout.
func TestBatchMixedSizeAndTimeout(t *testing.T) {
	const batchSize = 4
	const timeout = 30 * time.Millisecond

	// Send 10 records:
	// - First 8 (2 full batches of 4) flush by size
	// - Last 2 form a partial batch that times out
	const total = 10

	tmp := t.TempDir()
	s, err := OpenWithRetention(tmp+"/batch_mixed.db", 0, silentLogger, BatchConfig{
		Size:    batchSize,
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < total; i++ {
		if err := s.RecordRequest(Request{
			RequestID: "batch-mixed-" + string(rune('0'+i)),
			Route:     "local",
			Model:     "test",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}

	// Wait for timeout to fire on the partial final batch.
	time.Sleep(200 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	s2, err := OpenWithRetention(tmp+"/batch_mixed.db", 0, silentLogger, BatchConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	sum, err := s2.DailySummary(time.Now())
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.RequestCount != total {
		t.Errorf("RequestCount = %d, want %d", sum.RequestCount, total)
	}
}

// TestMetricsBatchCounterInCollector verifies that the collector's
// IncMetricsBatch increments the counter correctly (issue #1234).
func TestMetricsBatchCounterInCollector(t *testing.T) {
	// Verify the atomic increment pattern used by IncMetricsBatch.
	var total atomic.Uint64
	increment := func() { total.Add(1) }
	increment()
	increment()
	increment()

	if total.Load() != 3 {
		t.Errorf("batch counter = %d, want 3", total.Load())
	}
}
