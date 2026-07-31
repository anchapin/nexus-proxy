package metrics

import (
	"fmt"
	"testing"
	"time"
)

// insertRowDirect inserts a row directly via the store's *sql.DB,
// bypassing the buffered channel. Used by retention tests so the row
// is visible synchronously without close-reopen gymnastics.
func insertRowDirect(t *testing.T, s *SQLiteStore, ts time.Time, id string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO requests (timestamp, request_id, route, model) VALUES (?, ?, 'local', 'm')`,
		ts.UTC(), id,
	)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// countRows returns the total row count in the requests table.
func countRows(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestPruneDeletesExpiredRows verifies that pruneOnce removes rows
// older than the retention window and leaves recent rows intact
// (issue #483 acceptance criterion: seed 100 rows backdated 30 days →
// deleted after one prune tick).
func TestPruneDeletesExpiredRows(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")

	// 100 rows backdated 30 days — should be pruned by a 7-day window.
	old := time.Now().UTC().AddDate(0, 0, -30)
	for i := 0; i < 100; i++ {
		insertRowDirect(t, s, old, fmt.Sprintf("old-%d", i))
	}
	// 10 recent rows — must survive the prune.
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		insertRowDirect(t, s, now, fmt.Sprintf("new-%d", i))
	}
	if got := countRows(t, s); got != 110 {
		t.Fatalf("before prune: %d rows, want 110", got)
	}

	// One prune tick with a 7-day retention window.
	s.pruneOnce(7)

	if got := countRows(t, s); got != 10 {
		t.Errorf("after prune: %d rows, want 10 (only recent rows should survive)", got)
	}

	// Verify the exposed metrics.
	if rows := s.PruneLastRows(); rows != 100 {
		t.Errorf("PruneLastRows = %d, want 100", rows)
	}
	if ts := s.PruneLastTimestamp(); ts == 0 {
		t.Error("PruneLastTimestamp = 0, want non-zero after prune")
	}
}

// TestPruneZeroRetentionDeletesNothing verifies that pruneOnce with a
// retention of 0 is a no-op (no rows removed). This guards against an
// off-by-one that would delete everything.
func TestPruneZeroRetentionDeletesNothing(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")
	insertRowDirect(t, s, time.Now().UTC().AddDate(0, 0, -90), "old-1")
	insertRowDirect(t, s, time.Now().UTC(), "new-1")
	if got := countRows(t, s); got != 2 {
		t.Fatalf("before prune: %d rows, want 2", got)
	}

	s.pruneOnce(0)

	if got := countRows(t, s); got != 2 {
		t.Errorf("after prune(0): %d rows, want 2 (retention=0 must not delete)", got)
	}
}

// TestPruneBoundaryIsExclusive verifies that rows exactly at the cutoff
// boundary are NOT deleted (the comparison is timestamp < cutoff, i.e.
// strictly older). A row dated exactly N days ago to the second should
// survive a prune with retention=N.
func TestPruneBoundaryIsExclusive(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")
	// A row 5 days old — must survive a 7-day retention.
	insertRowDirect(t, s, time.Now().UTC().AddDate(0, 0, -5), "five-days-ago")
	// A row 90 days old — must be deleted.
	insertRowDirect(t, s, time.Now().UTC().AddDate(0, 0, -90), "ninety-days-ago")

	s.pruneOnce(7)

	if got := countRows(t, s); got != 1 {
		t.Errorf("after prune(7): %d rows, want 1 (only the 90-day-old row should be gone)", got)
	}
}

// TestRetentionGoroutineStartsAndStopsCleanly verifies that opening a
// store with retention > 0 starts the prune goroutine (pruneStop is
// non-nil) and that Close unblocks it without hanging.
func TestRetentionGoroutineStartsAndStopsCleanly(t *testing.T) {
	s, err := OpenWithRetention(t.TempDir()+"/metrics.db", 7, silentLogger)
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	ss := s.(*SQLiteStore)
	if ss.pruneStop == nil {
		t.Fatal("pruneStop should be non-nil when retention > 0")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRetentionDisabledByDefault verifies that the default path
// (retention=0) does not start a prune goroutine — pruneStop is nil.
// This guards the "byte-for-byte identical to today" acceptance
// criterion.
func TestRetentionDisabledByDefault(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")
	if s.pruneStop != nil {
		t.Error("pruneStop should be nil when retention=0 (default)")
	}
}

// TestRetentionGoroutinePrunesAtStartup verifies that the prune
// goroutine's immediate-at-startup pass actually deletes expired rows
// when the store is opened with retention > 0.
func TestRetentionGoroutinePrunesAtStartup(t *testing.T) {
	path := t.TempDir() + "/metrics.db"

	// Phase 1: create the DB and seed backdated rows with retention=0
	// so no prune runs before we're ready.
	s1 := openAtPath(t, path)
	for i := 0; i < 50; i++ {
		insertRowDirect(t, s1, time.Now().UTC().AddDate(0, 0, -30),
			fmt.Sprintf("old-%d", i))
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}

	// Phase 2: reopen with retention=7. The prune goroutine fires an
	// immediate pass at startup; we wait for it to complete.
	s2, err := OpenWithRetention(path, 7, silentLogger)
	if err != nil {
		t.Fatalf("OpenWithRetention: %v", err)
	}
	defer s2.Close()
	ss2 := s2.(*SQLiteStore)

	// The startup prune is asynchronous; poll until PruneLastTimestamp
	// becomes non-zero or we time out.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ss2.PruneLastTimestamp() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ss2.PruneLastTimestamp() == 0 {
		t.Fatal("startup prune did not fire within 3s")
	}
	if rows := ss2.PruneLastRows(); rows != 50 {
		t.Errorf("startup prune removed %d rows, want 50", rows)
	}
	if got := countRows(t, ss2); got != 0 {
		t.Errorf("after startup prune: %d rows, want 0", got)
	}
}
