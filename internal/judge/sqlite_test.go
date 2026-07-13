package judge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestSQLiteStoreRecord verifies that a score can be recorded without
// error. The async channel means we don't wait for the drain goroutine;
// actual persistence across restarts is tested by TestSQLiteStoreOnDisk.
func TestSQLiteStoreRecord(t *testing.T) {
	store, err := OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	score := JudgeScore{
		RequestID:   "req-test-1",
		Score:       4,
		RawResponse: `{"choices":[{"message":{"content":"4"}}]}`,
		Cost:        0.003,
		PromptTok:   200,
		OutputTok:   50,
		Timestamp:   time.Now().UTC(),
	}
	if err := store.Record(score); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Record is non-blocking; the drain goroutine processes it asynchronously.
	// We don't wait here — the synchronous persistence test is on-disk below.
}

// TestSQLiteStoreOnDisk tests that scores survive a close/reopen cycle
// on a real file path using the same pattern the metrics package uses.
func TestSQLiteStoreOnDisk(t *testing.T) {
	tmp := t.TempDir() + "/judge_test.db"

	store1, err := OpenSQLiteStore(tmp)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (first): %v", err)
	}

	const n = 5
	for i := 0; i < n; i++ {
		if err := store1.Record(JudgeScore{
			RequestID: fmt.Sprintf("req-disk-%d", i),
			Score:    i + 1,
			Cost:     0.001,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// Reopen the same file path.
	store2, err := OpenSQLiteStore(tmp)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (reopen): %v", err)
	}
	defer store2.Close()

	// Scores from store1 must be visible in store2.
	got, err := store2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != n {
		t.Errorf("got %d scores after reopen, want %d", len(got), n)
	}
}

// allScores reads all rows from the judge_scores table directly
// using the raw db handle (exported for tests only).
func (s *SQLiteStore) allScores(ctx context.Context) ([]JudgeScore, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT request_id, score, error, cost_usd FROM judge_scores ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scores []JudgeScore
	for rows.Next() {
		var s JudgeScore
		var errStr string
		if err := rows.Scan(&s.RequestID, &s.Score, &errStr, &s.Cost); err != nil {
			return nil, err
		}
		if errStr != "" {
			s.Err = errors.New(errStr)
		}
		scores = append(scores, s)
	}
	return scores, rows.Err()
}

// TestSQLiteStoreRecordError verifies that a score with Err set is
// persisted with score=0 and the error string stored in the error column.
func TestSQLiteStoreRecordError(t *testing.T) {
	tmp := t.TempDir() + "/judge_error_test.db"
	store, err := OpenSQLiteStore(tmp)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}

	score := JudgeScore{
		RequestID: "req-err",
		Score:     0,
		Err:       context.DeadlineExceeded,
		Cost:      0,
		Timestamp: time.Now().UTC(),
	}
	if err := store.Record(score); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen to verify persistence across restarts.
	store2, err := OpenSQLiteStore(tmp)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (reopen): %v", err)
	}
	defer store2.Close()

	got, err := store2.allScores(context.Background())
	if err != nil {
		t.Fatalf("allScores: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d scores, want 1", len(got))
	}
	if got[0].Score != 0 {
		t.Errorf("Score = %d, want 0 on error record", got[0].Score)
	}
	if got[0].Err == nil || got[0].Err.Error() != context.DeadlineExceeded.Error() {
		t.Errorf("Err = %v, want %v", got[0].Err, context.DeadlineExceeded)
	}
}

// Compile-time guard: SQLiteStore must satisfy judge.Storage.
var _ Storage = (*SQLiteStore)(nil)
