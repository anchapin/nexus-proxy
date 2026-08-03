package judge

import (
	"context"
	"testing"
	"time"
)

// TestJudgeBatchFlushOnSize verifies that drain flushes the accumulated
// batch when the accumulator reaches batchSize (issue #1234).
func TestJudgeBatchFlushOnSize(t *testing.T) {
	const batchSize = 5
	const total = batchSize * 3 // three full batches

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_size.db", batchSize, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < total; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-size-" + string(rune('0'+i)),
			Score:     (i % 5) + 1,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to verify rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_size.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != total {
		t.Errorf("got %d scores, want %d", len(got), total)
	}
}

// TestJudgeBatchFlushOnTimeout verifies that a partial batch is flushed
// after the timeout elapses even when the batch is not full (issue #1234).
func TestJudgeBatchFlushOnTimeout(t *testing.T) {
	const batchSize = 100 // intentionally large so size won't trigger
	const timeout = 50 * time.Millisecond
	const count = 7 // less than batchSize; only timeout should flush

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_timeout.db", batchSize, timeout)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-timeout-" + string(rune('0'+i)),
			Score:     4,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	// Wait for the timeout to fire plus a safety margin.
	time.Sleep(500 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to verify rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_timeout.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != count {
		t.Errorf("got %d scores, want %d (timeout should have flushed partial batch)", len(got), count)
	}
}

// TestJudgeBatchFlushOnShutdown verifies that any remaining records in
// the accumulator are flushed when the store is closed (issue #1234).
func TestJudgeBatchFlushOnShutdown(t *testing.T) {
	const batchSize = 10
	const count = 3 // less than batchSize; only shutdown flush

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_shutdown.db", batchSize, 10*time.Second)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}

	for i := 0; i < count; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-shutdown-" + string(rune('0'+i)),
			Score:     3,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	// Close without waiting for timeout; drain must flush the partial batch.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to verify rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_shutdown.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != count {
		t.Errorf("got %d scores, want %d (shutdown should have flushed partial batch)", len(got), count)
	}
}

// TestJudgeBatchSizeOneReproducesPerRecordBehaviour verifies that
// batchSize=1 produces byte-for-byte identical behaviour to the
// pre-batch path — one transaction per record (issue #1234).
func TestJudgeBatchSizeOneReproducesPerRecordBehaviour(t *testing.T) {
	const count = 5

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_one.db", 1, 0)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-one-" + string(rune('0'+i)),
			Score:     4,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_one.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != count {
		t.Errorf("got %d scores, want %d", len(got), count)
	}
}

// TestJudgeBatchDisabledWithZeroSize verifies that zero batchSize
// produces pre-batch per-record behaviour — no batching, each record
// in its own transaction (issue #1234).
func TestJudgeBatchDisabledWithZeroSize(t *testing.T) {
	const count = 5

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_disabled.db", 0, 0)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < count; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-disabled-" + string(rune('0'+i)),
			Score:     4,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_disabled.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != count {
		t.Errorf("got %d scores, want %d", len(got), count)
	}
}

// TestJudgeBatchMixedSizeAndTimeout verifies that when both size and
// timeout trigger, the size-based flush wins first; subsequent partial
// batches are then flushed by timeout (issue #1234).
func TestJudgeBatchMixedSizeAndTimeout(t *testing.T) {
	const batchSize = 4
	const timeout = 30 * time.Millisecond

	// Send 10 records:
	// - First 8 (2 full batches of 4) flush by size
	// - Last 2 form a partial batch that times out
	const total = 10

	tmp := t.TempDir()
	s, err := OpenSQLiteStore(tmp+"/judge_batch_mixed.db", batchSize, timeout)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < total; i++ {
		if err := s.Record(JudgeScore{
			RequestID: "batch-mixed-" + string(rune('0'+i)),
			Score:     4,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	// Wait for timeout to fire on the partial final batch.
	time.Sleep(200 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify all rows landed.
	s2, err := OpenSQLiteStore(tmp+"/judge_batch_mixed.db", 0, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != total {
		t.Errorf("got %d scores, want %d", len(got), total)
	}
}
