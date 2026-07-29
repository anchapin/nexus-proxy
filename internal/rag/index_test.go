package rag

import (
	"math"
	"testing"
)

// --- Test helpers ---

// buildTestIndex creates an HNSW index with n unit vectors in dim dimensions.
// Each vector is placed near a distinct axis with a small deterministic
// perturbation so nearest-neighbour relationships are unambiguous and
// reproducible.
func buildTestIndex(tb testing.TB, cfg HNSWConfig, n, dim int) *HNSWIndex {
	tb.Helper()
	idx := NewHNSWIndex(cfg)
	for i := 0; i < n; i++ {
		vec := make([]float64, dim)
		vec[i%dim] = 1.0
		// Deterministic perturbation so vectors aren't identical on shared axes.
		for d := 0; d < dim; d++ {
			vec[d] += 0.01 * math.Sin(float64(i*(d+1)+1))
		}
		normalizeVec(vec)
		idx.Add(i, vec)
	}
	if got := idx.Size(); got != n {
		tb.Fatalf("index size = %d, want %d", got, n)
	}
	return idx
}

func normalizeVec(v []float64) {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return
	}
	for i := range v {
		v[i] /= norm
	}
}

// --- Acceptance-criteria tests (issue #592) ---

// TestHNSWIndexSerializeRoundTrip verifies that Serialize + Deserialize
// produces an index returning the same Search results as the original.
//
// Because Deserialize rebuilds the graph by re-inserting vectors through
// addImpl with the same seed, the deserialized graph is structurally
// identical to the original, so Search results match exactly.
func TestHNSWIndexSerializeRoundTrip(t *testing.T) {
	cfg := HNSWConfig{M: 8, efConstruction: 50, efSearch: 20, seed: 99}
	const n, dim, k = 30, 16, 5

	original := buildTestIndex(t, cfg, n, dim)

	data, err := original.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("Serialize returned empty data")
	}

	restored, err := DeserializeHNSWIndex(data, HNSWConfig{})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex: %v", err)
	}
	if restored.Size() != original.Size() {
		t.Fatalf("restored size = %d, want %d", restored.Size(), original.Size())
	}

	// Run multiple queries from different directions.
	queries := make([][]float64, 0, 4)
	for _, axis := range []int{0, 1, 5, 15} {
		q := make([]float64, dim)
		q[axis] = 0.8
		q[(axis+1)%dim] = 0.2
		normalizeVec(q)
		queries = append(queries, q)
	}

	for qi, query := range queries {
		want := original.Search(query, k)
		got := restored.Search(query, k)

		if len(want) == 0 {
			t.Fatalf("query %d: original Search returned 0 results, want %d", qi, k)
		}
		if len(got) != len(want) {
			t.Fatalf("query %d: restored Search returned %d ids, want %d (got=%v want=%v)",
				qi, len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("query %d: Search result[%d] = %d, want %d (got=%v want=%v)",
					qi, i, got[i], want[i], got, want)
				break
			}
		}
	}
}

// TestHNSWIndexDeserializeTruncated verifies that corrupted or truncated
// data returns an error rather than panicking or producing a silently
// broken index.
func TestHNSWIndexDeserializeTruncated(t *testing.T) {
	cfg := HNSWConfig{M: 8, efConstruction: 50, efSearch: 20, seed: 7}
	idx := buildTestIndex(t, cfg, 5, 8)

	full, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"one-byte", []byte{0x00}},
		{"header-minus-one", full[:serializeHeaderSize-1]},
		{"header-only-no-entries-read", full[:serializeHeaderSize]},
		{"truncated-after-first-id", full[:serializeHeaderSize+4]},
		{"truncated-mid-vector-data", full[:serializeHeaderSize+8]},
		{"one-byte-short-of-full", full[:len(full)-1]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeserializeHNSWIndex(tc.data, cfg)
			if err == nil {
				t.Fatal("expected error for truncated data, got nil")
			}
		})
	}
}

// TestHNSWIndexDeserializeMismatchedDims verifies that cfg M,
// efConstruction, and efSearch are preserved across a serialize →
// deserialize round-trip (acceptance criterion from issue #592).
func TestHNSWIndexDeserializeMismatchedDims(t *testing.T) {
	cfg := HNSWConfig{M: 12, efConstruction: 77, efSearch: 33, seed: 55}
	idx := buildTestIndex(t, cfg, 10, 8)

	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	// Deserialize with an empty cfg so the blob's values are used.
	restored, err := DeserializeHNSWIndex(data, HNSWConfig{})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex: %v", err)
	}

	if restored.cfg.M != cfg.M {
		t.Errorf("M = %d, want %d", restored.cfg.M, cfg.M)
	}
	if restored.cfg.efConstruction != cfg.efConstruction {
		t.Errorf("efConstruction = %d, want %d", restored.cfg.efConstruction, cfg.efConstruction)
	}
	if restored.cfg.efSearch != cfg.efSearch {
		t.Errorf("efSearch = %d, want %d", restored.cfg.efSearch, cfg.efSearch)
	}
	if restored.cfg.seed != cfg.seed {
		t.Errorf("seed = %d, want %d", restored.cfg.seed, cfg.seed)
	}
}

// --- Supplementary tests ---

// TestHNSWIndexDeserializeCallerCfgOverride verifies that the caller can
// override individual config fields (e.g. bump efSearch at load time)
// while the remaining fields fall back to the serialized values.
func TestHNSWIndexDeserializeCallerCfgOverride(t *testing.T) {
	origCfg := HNSWConfig{M: 8, efConstruction: 50, efSearch: 20, seed: 11}
	idx := buildTestIndex(t, origCfg, 8, 8)

	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	restored, err := DeserializeHNSWIndex(data, HNSWConfig{efSearch: 99})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex: %v", err)
	}
	if restored.cfg.efSearch != 99 {
		t.Errorf("efSearch = %d, want 99 (caller override)", restored.cfg.efSearch)
	}
	if restored.cfg.M != origCfg.M {
		t.Errorf("M = %d, want %d (from blob)", restored.cfg.M, origCfg.M)
	}
	if restored.cfg.efConstruction != origCfg.efConstruction {
		t.Errorf("efConstruction = %d, want %d (from blob)", restored.cfg.efConstruction, origCfg.efConstruction)
	}
	if restored.cfg.seed != origCfg.seed {
		t.Errorf("seed = %d, want %d (from blob)", restored.cfg.seed, origCfg.seed)
	}
}

// TestHNSWIndexSerializeEmptyIndex verifies that an empty index serializes
// and deserializes without error.
func TestHNSWIndexSerializeEmptyIndex(t *testing.T) {
	idx := NewHNSWIndex(DefaultHNSWConfig())
	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize empty index: %v", err)
	}
	restored, err := DeserializeHNSWIndex(data, HNSWConfig{})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex empty: %v", err)
	}
	if restored.Size() != 0 {
		t.Errorf("restored size = %d, want 0", restored.Size())
	}
}

// TestHNSWIndexSerializeDefaultConfig verifies round-trip with the default
// config (seed=42) used by the RAG store in production.
func TestHNSWIndexSerializeDefaultConfig(t *testing.T) {
	cfg := DefaultHNSWConfig()
	idx := buildTestIndex(t, cfg, 20, 32)

	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	restored, err := DeserializeHNSWIndex(data, HNSWConfig{})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex: %v", err)
	}
	if restored.Size() != idx.Size() {
		t.Fatalf("size mismatch: %d vs %d", restored.Size(), idx.Size())
	}

	query := make([]float64, 32)
	query[0] = 0.9
	query[1] = 0.1
	normalizeVec(query)

	want := idx.Search(query, 3)
	got := restored.Search(query, 3)
	if len(got) != len(want) {
		t.Fatalf("Search len mismatch: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Search[%d] = %d, want %d (got=%v want=%v)", i, got[i], want[i], got, want)
			break
		}
	}
}

// TestHNSWIndexSerializeIdempotent verifies that serializing the same index
// twice produces identical output (deterministic format).
func TestHNSWIndexSerializeIdempotent(t *testing.T) {
	cfg := HNSWConfig{M: 8, efConstruction: 50, efSearch: 20, seed: 3}
	idx := buildTestIndex(t, cfg, 10, 8)

	first, err := idx.Serialize()
	if err != nil {
		t.Fatalf("first Serialize: %v", err)
	}
	second, err := idx.Serialize()
	if err != nil {
		t.Fatalf("second Serialize: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("Serialize output length differs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Serialize output differs at byte %d: %x vs %x", i, first[i], second[i])
		}
	}
}

// TestDeserializeHNSWIndex_DimensionMismatch verifies that DeserializeHNSWIndex
// returns an error when the stored vector dimensions do not match the
// expected dimension configured in HNSWConfig (issue #964).
func TestDeserializeHNSWIndex_DimensionMismatch(t *testing.T) {
	t.Parallel()

	cfg := HNSWConfig{M: 8, efConstruction: 50, efSearch: 20, seed: 42}
	const n, dim = 10, 16
	idx := buildTestIndex(t, cfg, n, dim)

	data, err := idx.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	// Deserializing with matching dimension should succeed.
	_, err = DeserializeHNSWIndex(data, HNSWConfig{Dimension: dim})
	if err != nil {
		t.Fatalf("DeserializeHNSWIndex with matching dimension: %v", err)
	}

	// Deserializing with wrong dimension should fail.
	wrongDim := dim + 4
	_, err = DeserializeHNSWIndex(data, HNSWConfig{Dimension: wrongDim})
	if err == nil {
		t.Fatalf("DeserializeHNSWIndex with wrong dimension: expected error, got nil")
	}
}
