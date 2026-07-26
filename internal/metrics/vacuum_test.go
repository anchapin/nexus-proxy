package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pageCount returns the current database page count.
func pageCount(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&n); err != nil {
		t.Fatalf("PRAGMA page_count: %v", err)
	}
	return n
}

// autoVacuumMode returns the integer auto_vacuum mode stored in the
// database header: 0 = NONE, 1 = FULL, 2 = INCREMENTAL.
func autoVacuumMode(t *testing.T, db *sql.DB) int {
	t.Helper()
	var mode int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA auto_vacuum: %v", err)
	}
	return mode
}

// TestAutoVacuumIncrementalOnFreshDB verifies that a freshly created
// database has auto_vacuum=INCREMENTAL (issue #595). Without this,
// PRAGMA incremental_vacuum is a no-op and the DB file grows forever
// despite retention deletes.
func TestAutoVacuumIncrementalOnFreshDB(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")
	if mode := autoVacuumMode(t, s.db); mode != autoVacuumIncremental {
		t.Errorf("auto_vacuum = %d, want %d (INCREMENTAL)", mode, autoVacuumIncremental)
	}
}

// TestIncrementalVacuumReclaimsSpace verifies that after deleting rows
// and running incremental_vacuum, the page count actually decreases
// (issue #595 acceptance criterion). This is only possible when
// auto_vacuum=INCREMENTAL is set.
func TestIncrementalVacuumReclaimsSpace(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")

	// Insert rows carrying a large payload to force multi-page
	// allocation (otherwise SQLite packs rows into a single page
	// and the delta is invisible).
	payload := strings.Repeat("x", 2000)
	ts := time.Now().UTC().AddDate(0, 0, -30)
	for i := 0; i < 200; i++ {
		_, err := s.db.Exec(
			`INSERT INTO requests (timestamp, request_id, route, model, error) VALUES (?, ?, 'local', 'm', ?)`,
			ts, fmt.Sprintf("row-%d", i), payload,
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	pagesBefore := pageCount(t, s)
	if pagesBefore <= 1 {
		t.Fatalf("page_count after insert = %d, expected > 1", pagesBefore)
	}

	// Delete all rows — freed pages move to the free-list.
	if _, err := s.db.Exec("DELETE FROM requests"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	// Reclaim free-list pages. Without auto_vacuum=INCREMENTAL this
	// is a no-op and the page count would not change.
	if _, err := s.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		t.Fatalf("PRAGMA incremental_vacuum: %v", err)
	}

	pagesAfter := pageCount(t, s)
	if pagesAfter >= pagesBefore {
		t.Errorf("page_count after vacuum = %d, want < %d (before)", pagesAfter, pagesBefore)
	}
}

// TestMigrateAutoVacuumOnLegacyDB simulates an upgrade from a pre-fix
// build: a database created WITHOUT auto_vacuum=INCREMENTAL is opened
// by the current code, and the migration must flip it to INCREMENTAL
// (issue #595).
func TestMigrateAutoVacuumOnLegacyDB(t *testing.T) {
	path := t.TempDir() + "/legacy.db"

	// Create the database the old way: no auto_vacuum pragma anywhere.
	dsn := "file:" + path + "?mode=rwc&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(requestsSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// Insert a row so there is data.
	if _, err := db.Exec(
		`INSERT INTO requests (timestamp, request_id, route, model) VALUES (?, 'legacy-1', 'local', 'm')`,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Confirm the legacy DB starts at auto_vacuum=NONE.
	if mode := autoVacuumMode(t, db); mode != 0 {
		t.Fatalf("legacy DB auto_vacuum = %d, want 0 (NONE)", mode)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Open with the current code — the DSN pragma and/or
	// migrateAutoVacuum must set INCREMENTAL.
	s := openAtPath(t, path)
	if mode := autoVacuumMode(t, s.db); mode != autoVacuumIncremental {
		t.Errorf("auto_vacuum after open = %d, want %d (INCREMENTAL)", mode, autoVacuumIncremental)
	}
}

// TestMigrateAutoVacuumSetsIncrementalDirectly verifies that
// migrateAutoVacuum flips a NONE-mode database to INCREMENTAL when
// called on a connection that was opened without the auto_vacuum DSN
// pragma (issue #595).
func TestMigrateAutoVacuumSetsIncrementalDirectly(t *testing.T) {
	path := t.TempDir() + "/direct.db"

	// Open without the auto_vacuum pragma so the DB starts at NONE.
	dsn := "file:" + path + "?mode=rwc&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(requestsSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if mode := autoVacuumMode(t, db); mode != 0 {
		t.Fatalf("pre-migration auto_vacuum = %d, want 0 (NONE)", mode)
	}

	// Run the migration function directly.
	migrateAutoVacuum(context.Background(), db, silentLogger)

	if mode := autoVacuumMode(t, db); mode != autoVacuumIncremental {
		t.Errorf("post-migration auto_vacuum = %d, want %d (INCREMENTAL)", mode, autoVacuumIncremental)
	}
}

// TestMigrateAutoVacuumNoopOnAlreadyIncremental verifies that
// migrateAutoVacuum does not touch a database that is already in
// INCREMENTAL mode (idempotent — no unnecessary VACUUM).
func TestMigrateAutoVacuumNoopOnAlreadyIncremental(t *testing.T) {
	s := openAtPath(t, t.TempDir()+"/metrics.db")
	// Fresh DB already has auto_vacuum=INCREMENTAL via DSN pragma.
	before := autoVacuumMode(t, s.db)
	migrateAutoVacuum(context.Background(), s.db, silentLogger)
	after := autoVacuumMode(t, s.db)
	if before != autoVacuumIncremental || after != autoVacuumIncremental {
		t.Errorf("auto_vacuum before=%d after=%d, want %d for both", before, after, autoVacuumIncremental)
	}
}
