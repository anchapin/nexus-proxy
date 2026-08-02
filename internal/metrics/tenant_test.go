package metrics

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestTenantColumn_Recorded verifies that the tenant field is written
// to the requests table when set (issue #1154).
func TestTenantColumn_Recorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	req := Request{
		Timestamp:   time.Now().UTC(),
		RequestID:   "test-tenant-1",
		Route:       "local",
		Model:       "test-model",
		InputTokens: 100,
		Tenant:      "team-a",
	}
	if err := store.RecordRequest(req); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	// Close flushes the buffered write channel.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open and read back the tenant column directly.
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer store2.Close()

	ss, ok := store2.(*SQLiteStore)
	if !ok {
		t.Fatal("store is not *SQLiteStore")
	}
	var tenant string
	err = ss.db.QueryRowContext(context.Background(),
		`SELECT tenant FROM requests WHERE request_id = ?`, "test-tenant-1").Scan(&tenant)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if tenant != "team-a" {
		t.Fatalf("tenant = %q, want %q", tenant, "team-a")
	}
}

// TestTenantColumn_EmptyForLegacyPath verifies that the tenant column
// defaults to empty string when not set (legacy single-key path).
func TestTenantColumn_EmptyForLegacyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	req := Request{
		Timestamp:   time.Now().UTC(),
		RequestID:   "test-no-tenant",
		Route:       "frontier",
		Model:       "test-model",
		InputTokens: 50,
		// Tenant intentionally left empty
	}
	if err := store.RecordRequest(req); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	// Close flushes the buffered write channel.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open and read back.
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer store2.Close()

	ss, ok := store2.(*SQLiteStore)
	if !ok {
		t.Fatal("store is not *SQLiteStore")
	}
	var tenant string
	err = ss.db.QueryRowContext(context.Background(),
		`SELECT tenant FROM requests WHERE request_id = ?`, "test-no-tenant").Scan(&tenant)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if tenant != "" {
		t.Fatalf("tenant = %q, want empty", tenant)
	}
}

// TestTenantColumn_AdditiveMigration verifies that an existing database
// without the tenant column gets migrated correctly.
func TestTenantColumn_AdditiveMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")

	// Create a legacy database with the full schema minus the tenant column.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		route TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		toon_savings_tokens INTEGER NOT NULL DEFAULT 0,
		toon_compression_method TEXT NOT NULL DEFAULT '',
		rag_injected INTEGER NOT NULL DEFAULT 0,
		rag_filename TEXT NOT NULL DEFAULT '',
		rag_cache_hit INTEGER NOT NULL DEFAULT 0,
		estimated_cost_usd REAL NOT NULL DEFAULT 0,
		baseline_cost_usd REAL NOT NULL DEFAULT 0,
		savings_usd REAL NOT NULL DEFAULT 0,
		ttft_ms INTEGER NOT NULL DEFAULT 0,
		total_latency_ms REAL NOT NULL DEFAULT 0,
		tps REAL NOT NULL DEFAULT 0,
		streaming INTEGER NOT NULL DEFAULT 0,
		fusion_arbiter_skipped INTEGER NOT NULL DEFAULT 0,
		fusion_jaccard_similarity REAL NOT NULL DEFAULT 0,
		fusion_arbiter_cost_usd REAL NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		route_source TEXT NOT NULL DEFAULT '',
		route_reason TEXT NOT NULL DEFAULT '',
		slm_confidence REAL NOT NULL DEFAULT 0,
		slm_task_type TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	_, err = db.Exec(`INSERT INTO requests (timestamp, request_id) VALUES (?, 'legacy-row')`,
		time.Now().UTC())
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	db.Close()

	// Now open via the metrics store — this should run additive migrations.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migration): %v", err)
	}

	// Verify we can insert a row with tenant.
	if err := store.RecordRequest(Request{
		Timestamp: time.Now().UTC(),
		RequestID: "post-migration",
		Route:     "local",
		Model:     "test-model",
		Tenant:    "team-migrated",
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	// Close flushes the buffered write channel.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open to verify.
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (verify): %v", err)
	}
	defer store2.Close()

	ss := store2.(*SQLiteStore)

	// Verify the legacy row still exists and has default tenant.
	var tenant string
	err = ss.db.QueryRowContext(context.Background(),
		`SELECT tenant FROM requests WHERE request_id = 'legacy-row'`).Scan(&tenant)
	if err != nil {
		t.Fatalf("QueryRow legacy: %v", err)
	}
	if tenant != "" {
		t.Fatalf("legacy row tenant = %q, want empty (default)", tenant)
	}

	// Verify the new row has the correct tenant.
	err = ss.db.QueryRowContext(context.Background(),
		`SELECT tenant FROM requests WHERE request_id = 'post-migration'`).Scan(&tenant)
	if err != nil {
		t.Fatalf("QueryRow new: %v", err)
	}
	if tenant != "team-migrated" {
		t.Fatalf("new row tenant = %q, want %q", tenant, "team-migrated")
	}
}
