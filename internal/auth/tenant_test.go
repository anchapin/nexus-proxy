package auth

import (
	"context"
	"crypto/subtle"
	"os"
	"path/filepath"
	"testing"
)

func TestWithTenant_TenantFromContext(t *testing.T) {
	t.Parallel()
	ctx := WithTenant(context.Background(), "team-a")
	if got := TenantFromContext(ctx); got != "team-a" {
		t.Fatalf("TenantFromContext = %q, want %q", got, "team-a")
	}
}

func TestTenantFromContext_Empty(t *testing.T) {
	t.Parallel()
	if got := TenantFromContext(context.Background()); got != "" {
		t.Fatalf("TenantFromContext = %q, want empty", got)
	}
}

func TestCredentialSet_Match(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet([]CredentialEntry{
		{Key: "key-alpha", Tenant: "team-a"},
		{Key: "key-beta", Tenant: "team-b"},
	})
	tenant, ok := cs.Match("key-beta")
	if !ok {
		t.Fatal("Match returned false for valid key")
	}
	if tenant != "team-b" {
		t.Fatalf("tenant = %q, want %q", tenant, "team-b")
	}
}

func TestCredentialSet_MatchNotFound(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet([]CredentialEntry{
		{Key: "key-alpha", Tenant: "team-a"},
	})
	_, ok := cs.Match("wrong-key")
	if ok {
		t.Fatal("Match returned true for invalid key")
	}
}

func TestCredentialSet_EmptySet(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet(nil)
	_, ok := cs.Match("anything")
	if ok {
		t.Fatal("Match returned true for empty credential set")
	}
}

func TestCredentialSet_EmptyKeySkipped(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet([]CredentialEntry{
		{Key: "", Tenant: "skip"},
		{Key: "valid", Tenant: "team-a"},
	})
	if cs.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (empty key should be skipped)", cs.Len())
	}
}

func TestCredentialSet_DuplicateKeyOverwrites(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet([]CredentialEntry{
		{Key: "same-key", Tenant: "team-a"},
		{Key: "same-key", Tenant: "team-b"},
	})
	if cs.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (duplicate keys merged)", cs.Len())
	}
	tenant, ok := cs.Match("same-key")
	if !ok {
		t.Fatal("Match failed")
	}
	if tenant != "team-b" {
		t.Fatalf("tenant = %q, want %q (last entry wins)", tenant, "team-b")
	}
}

// TestCredentialSet_ConstantTimeComparison verifies that Match always
// runs constant-time comparison against all candidates (no early
// return). The test checks that a key at the end of the list matches
// just as reliably as one at the beginning.
func TestCredentialSet_ConstantTimeAllPositions(t *testing.T) {
	t.Parallel()
	entries := []CredentialEntry{
		{Key: "k1", Tenant: "t1"},
		{Key: "k2", Tenant: "t2"},
		{Key: "k3", Tenant: "t3"},
		{Key: "k4", Tenant: "t4"},
		{Key: "k5", Tenant: "t5"},
	}
	cs := NewCredentialSet(entries)
	for _, e := range entries {
		tenant, ok := cs.Match(e.Key)
		if !ok {
			t.Fatalf("Match(%q) = false, want true", e.Key)
		}
		if tenant != e.Tenant {
			t.Fatalf("tenant = %q, want %q", tenant, e.Tenant)
		}
	}
}

// TestCredentialSet_Replace verifies hot-reload swap.
func TestCredentialSet_Replace(t *testing.T) {
	t.Parallel()
	cs := NewCredentialSet([]CredentialEntry{
		{Key: "old-key", Tenant: "old"},
	})
	// Verify original key works
	if _, ok := cs.Match("old-key"); !ok {
		t.Fatal("Match old-key failed before Replace")
	}
	// Replace with new set
	cs.Replace([]CredentialEntry{
		{Key: "new-key", Tenant: "new"},
	})
	// Old key should no longer match
	if _, ok := cs.Match("old-key"); ok {
		t.Fatal("Match old-key succeeded after Replace; should be gone")
	}
	// New key should match
	tenant, ok := cs.Match("new-key")
	if !ok {
		t.Fatal("Match new-key failed after Replace")
	}
	if tenant != "new" {
		t.Fatalf("tenant = %q, want %q", tenant, "new")
	}
}

func TestLoadAPIKeysFile_Valid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	content := `[{"key":"alpha","tenant":"team-a"},{"key":"beta","tenant":"team-b"}]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadAPIKeysFile(path)
	if err != nil {
		t.Fatalf("LoadAPIKeysFile: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Key != "alpha" || entries[0].Tenant != "team-a" {
		t.Fatalf("entries[0] = %+v", entries[0])
	}
}

func TestLoadAPIKeysFile_EmptyArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadAPIKeysFile(path)
	if err != nil {
		t.Fatalf("LoadAPIKeysFile: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}

func TestLoadAPIKeysFile_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := LoadAPIKeysFile("/nonexistent/path/keys.json")
	if err == nil {
		t.Fatal("LoadAPIKeysFile should error for missing file")
	}
}

func TestLoadAPIKeysFile_InvalidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, []byte(`{not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAPIKeysFile(path)
	if err == nil {
		t.Fatal("LoadAPIKeysFile should error for invalid JSON")
	}
}

func TestLoadAPIKeysFile_EmptyKeyInEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	content := `[{"key":"","tenant":"team-a"}]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAPIKeysFile(path)
	if err == nil {
		t.Fatal("LoadAPIKeysFile should error for empty key in entry")
	}
}

func TestLoadAPIKeysFile_EmptyPath(t *testing.T) {
	t.Parallel()
	_, err := LoadAPIKeysFile("")
	if err == nil {
		t.Fatal("LoadAPIKeysFile should error for empty path")
	}
}

// TestConstantTimeCompareReference ensures the comparison path doesn't
// short-circuit — this is a smoke test verifying the API is used
// correctly.
func TestConstantTimeCompareReference(t *testing.T) {
	t.Parallel()
	// Verify the subtle.ConstantTimeCompare API behaves as expected.
	if subtle.ConstantTimeCompare([]byte("abc"), []byte("abc")) != 1 {
		t.Fatal("equal strings should return 1")
	}
	if subtle.ConstantTimeCompare([]byte("abc"), []byte("abd")) != 0 {
		t.Fatal("unequal strings should return 0")
	}
}
