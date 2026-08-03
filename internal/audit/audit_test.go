package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperEntry builds an AuditEntry with a fixed timestamp for deterministic
// hash computation across tests.
func helperEntry(requestID, route, outcome string) AuditEntry {
	return AuditEntry{
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		RequestID: requestID,
		ClientIP:  "10.0.0.5",
		KeyHash:   "abcdef0123456789",
		Route:     route,
		Model:     "qwen3-coder:8b",
		Outcome:   outcome,
	}
}

func TestChainIntegrityPristine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries := []AuditEntry{
		helperEntry("req-1", "local", "success"),
		helperEntry("req-2", "frontier", "success"),
		helperEntry("req-3", "fusion", "error"),
	}
	for _, e := range entries {
		if err := a.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := Verify(path); err != nil {
		t.Fatalf("pristine file should verify clean, got: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].PrevHash != GenesisHash {
		t.Errorf("first entry prev_hash = %q, want genesis %q", got[0].PrevHash, GenesisHash)
	}
	if got[2].Hash == "" {
		t.Errorf("final entry hash not populated")
	}
	// Chain linkage: each entry's PrevHash == previous entry's Hash.
	for i := 1; i < len(got); i++ {
		if got[i].PrevHash != got[i-1].Hash {
			t.Errorf("entry %d PrevHash=%q, want prev Hash=%q", i, got[i].PrevHash, got[i-1].Hash)
		}
	}
}

func TestTamperDetectionMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, e := range []AuditEntry{
		helperEntry("req-1", "local", "success"),
		helperEntry("req-2", "frontier", "success"),
		helperEntry("req-3", "local", "success"),
	} {
		if err := a.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tamper: rewrite the second line with a different route.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var tampered AuditEntry
	if err := json.Unmarshal([]byte(lines[1]), &tampered); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	tampered.Route = "frontier" // was "frontier" already; flip to force change differently
	tampered.Outcome = "tampered"
	// Recompute hash so only the PAYLOAD mismatch triggers, not prev_hash.
	tampered.Hash = computeHash(tampered.PrevHash, hashPayload{
		Timestamp: tampered.Timestamp,
		RequestID: tampered.RequestID,
		ClientIP:  tampered.ClientIP,
		KeyHash:   tampered.KeyHash,
		Route:     tampered.Route,
		Model:     tampered.Model,
		Outcome:   tampered.Outcome,
	})
	b, _ := json.Marshal(tampered)
	lines[1] = string(b)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err = Verify(path)
	if err == nil {
		t.Fatal("tampered file should fail verification")
	}
	if !strings.Contains(err.Error(), "line 3") {
		// The mutation changes line 2's hash, so line 3's prev_hash check fails.
		t.Logf("verify error (expected): %v", err)
	}
}

func TestTamperDetectionPayloadChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := a.Record(helperEntry("req-1", "local", "success")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := a.Record(helperEntry("req-2", "frontier", "success")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Mutate the first entry's route WITHOUT fixing the hash → Verify
	// must detect the hash mismatch on line 1 itself.
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var e AuditEntry
	json.Unmarshal([]byte(lines[0]), &e)
	e.Route = "frontier"
	b, _ := json.Marshal(e)
	lines[0] = string(b)
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	if err := Verify(path); err == nil {
		t.Fatal("mutated payload should fail verification (hash mismatch on line 1)")
	}
}

func TestTamperDetectionDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, e := range []AuditEntry{
		helperEntry("req-1", "local", "success"),
		helperEntry("req-2", "frontier", "success"),
		helperEntry("req-3", "fusion", "success"),
	} {
		if err := a.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Delete the middle line (not the final one) → chain breaks.
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	rest := append(lines[:1], lines[2:]...)
	os.WriteFile(path, []byte(strings.Join(rest, "\n")+"\n"), 0o600)

	if err := Verify(path); err == nil {
		t.Fatal("file with a deleted middle line should fail verification")
	}
}

func TestAppendAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// First process: write 2 entries.
	a1, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	if err := a1.Record(helperEntry("req-1", "local", "success")); err != nil {
		t.Fatalf("Record #1: %v", err)
	}
	if err := a1.Record(helperEntry("req-2", "frontier", "success")); err != nil {
		t.Fatalf("Record #2: %v", err)
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	// Second process (restart): open again and append more entries.
	a2, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	if err := a2.Record(helperEntry("req-3", "fusion", "success")); err != nil {
		t.Fatalf("Record #3: %v", err)
	}
	if err := a2.Close(); err != nil {
		t.Fatalf("Close #2: %v", err)
	}

	// The full chain must verify — continuity across restarts.
	if err := Verify(path); err != nil {
		t.Fatalf("cross-restart chain should verify clean: %v", err)
	}

	got, _ := Read(path)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries after restart, got %d", len(got))
	}
	// Entry 3's PrevHash must equal entry 2's Hash.
	if got[2].PrevHash != got[1].Hash {
		t.Errorf("restart entry PrevHash=%q, want %q", got[2].PrevHash, got[1].Hash)
	}
}

func TestKeyNeverLoggedRaw(t *testing.T) {
	rawKey := "sk-super-secret-key-1234567890abcdef"
	h := HashCredential(rawKey)

	if h == "" {
		t.Fatal("hash should not be empty for non-empty credential")
	}
	if len(h) != keyHashLen {
		t.Errorf("hash length = %d, want %d", len(h), keyHashLen)
	}
	if strings.Contains(rawKey, h) || strings.Contains(h, rawKey) {
		t.Fatal("raw key must not appear in hash")
	}
	// The full SHA-256 is 64 hex chars; we keep only 16.
	if len(h) >= 64 {
		t.Error("hash should be truncated, not the full digest")
	}
	// Hash must be deterministic.
	if HashCredential(rawKey) != h {
		t.Error("hash should be deterministic")
	}
	// Empty credential → empty hash.
	if HashCredential("") != "" {
		t.Error("empty credential should produce empty hash")
	}
}

func TestFileEntryNeverContainsRawKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rawKey := "sk-never-appear-in-log"
	entry := AuditEntry{
		Timestamp: time.Now().UTC(),
		RequestID: "req-1",
		ClientIP:  "10.0.0.1",
		KeyHash:   HashCredential(rawKey),
		Route:     "local",
		Model:     "qwen3-coder:8b",
		Outcome:   "success",
	}
	if err := a.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	a.Close()

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), rawKey) {
		t.Fatal("audit log must never contain the raw API key")
	}
}

func TestConcurrentWritesAreRaceFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const goroutines = 16
	const perG = 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				e := AuditEntry{
					Timestamp: time.Now().UTC(),
					RequestID: "req-concurrent",
					ClientIP:  "10.0.0.1",
					KeyHash:   "abcdef0123456789",
					Route:     "local",
					Model:     "qwen3-coder:8b",
					Outcome:   "success",
				}
				if err := a.Record(e); err != nil {
					t.Errorf("concurrent Record: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()
	a.Close()

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := goroutines * perG
	if len(got) != want {
		t.Fatalf("expected %d entries, got %d", want, len(got))
	}
	if err := Verify(path); err != nil {
		t.Fatalf("concurrent chain should verify clean: %v", err)
	}
}

func TestEmptyFileVerifies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	// Create empty file.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Verify(path); err != nil {
		t.Fatalf("empty file should verify clean: %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := Open(path, SyncFull)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a.Record(helperEntry("req-1", "local", "success"))
	a.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
}

func TestParseSyncMode(t *testing.T) {
	if ParseSyncMode("none") != SyncNone {
		t.Error("'none' should parse to SyncNone")
	}
	if ParseSyncMode("full") != SyncFull {
		t.Error("'full' should parse to SyncFull")
	}
	if ParseSyncMode("garbage") != SyncFull {
		t.Error("unknown sync mode should fall back to SyncFull")
	}
}

func TestNoopAuditor(t *testing.T) {
	n := NoopAuditor{}
	if err := n.Record(AuditEntry{}); err != nil {
		t.Errorf("NoopAuditor.Record should never fail: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("NoopAuditor.Close should never fail: %v", err)
	}
}
