package metrics

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
)

// TestRecentArbiterSyntheses_Empty verifies the query returns no rows on a
// fresh store (issue #1176).
func TestRecentArbiterSyntheses_Empty(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.RecentArbiterSyntheses(context.Background(), 256, time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("RecentArbiterSyntheses: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows on empty store, want 0", len(rows))
	}
}

// TestRecentArbiterSyntheses_ReturnsRows verifies that arbiter synthesis
// data written via RecordRequest is returned by the query (issue #1176).
func TestRecentArbiterSyntheses_ReturnsRows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	keyHex := hex.EncodeToString(make([]byte, 32))
	if err := s.RecordRequest(Request{
		Timestamp:          now,
		RequestID:          "req-1",
		Route:              "fusion",
		Model:              "test-model",
		ArbiterCacheKeyHex: keyHex,
		ArbiterSynthesis:   "synthesis text",
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	// Close flushes the async write channel; reopen to query.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	reader := s2.(*SQLiteStore)
	rows, err := reader.RecentArbiterSyntheses(context.Background(), 256, now.Add(-1*time.Minute))
	if err != nil {
		t.Fatalf("RecentArbiterSyntheses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CacheKeyHex != keyHex {
		t.Errorf("CacheKeyHex = %q, want %q", rows[0].CacheKeyHex, keyHex)
	}
	if rows[0].Synthesis != "synthesis text" {
		t.Errorf("Synthesis = %q, want %q", rows[0].Synthesis, "synthesis text")
	}
}

// TestRecentArbiterSyntheses_SkipsEmptyKeys verifies that requests without
// arbiter synthesis data are excluded from the query (issue #1176).
func TestRecentArbiterSyntheses_SkipsEmptyKeys(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// Write a normal request (no arbiter data)
	if err := s.RecordRequest(Request{
		Timestamp: now,
		RequestID: "req-normal",
		Route:     "local",
		Model:     "test-model",
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	// Write a request WITH arbiter data
	keyHex := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if err := s.RecordRequest(Request{
		Timestamp:          now,
		RequestID:          "req-arbiter",
		Route:              "fusion",
		Model:              "test-model",
		ArbiterCacheKeyHex: keyHex,
		ArbiterSynthesis:   "arbiter output",
	}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	reader := s2.(*SQLiteStore)
	rows, _ := reader.RecentArbiterSyntheses(context.Background(), 256, now.Add(-1*time.Minute))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only the arbiter request)", len(rows))
	}
}

// TestRecentArbiterSyntheses_RespectsSinceFilter verifies that entries
// older than the since parameter are excluded (issue #1176).
func TestRecentArbiterSyntheses_RespectsSinceFilter(t *testing.T) {
	s := newTestStore(t)

	old := time.Now().UTC().Add(-10 * time.Minute)
	keyHexOld := hex.EncodeToString([]byte("old-old-old-old-old-old-old-old-old"))

	if err := s.RecordRequest(Request{
		Timestamp:          old,
		RequestID:          "req-old",
		Route:              "fusion",
		Model:              "test",
		ArbiterCacheKeyHex: keyHexOld,
		ArbiterSynthesis:   "old synthesis",
	}); err != nil {
		t.Fatalf("RecordRequest old: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	reader := s2.(*SQLiteStore)
	// since = 5 minutes ago: the old entry (10 min ago) should be excluded
	since := time.Now().Add(-5 * time.Minute)
	rows, _ := reader.RecentArbiterSyntheses(context.Background(), 256, since)
	for _, r := range rows {
		if r.CacheKeyHex == keyHexOld {
			t.Error("old entry appeared despite since filter")
		}
	}
}

// TestRecentArbiterSyntheses_LimitCap verifies the limit parameter caps
// the number of rows returned (issue #1176).
func TestRecentArbiterSyntheses_LimitCap(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	for i := 0; i < 5; i++ {
		key := make([]byte, 32)
		key[0] = byte(i)
		if err := s.RecordRequest(Request{
			Timestamp:          now.Add(time.Duration(i) * time.Second),
			RequestID:          "req-" + string(rune('a'+i)),
			Route:              "fusion",
			Model:              "test",
			ArbiterCacheKeyHex: hex.EncodeToString(key),
			ArbiterSynthesis:   "synth",
		}); err != nil {
			t.Fatalf("RecordRequest %d: %v", i, err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenWithLogger(s.Path(), silentLogger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	reader := s2.(*SQLiteStore)
	rows, _ := reader.RecentArbiterSyntheses(context.Background(), 3, now.Add(-1*time.Minute))
	if len(rows) != 3 {
		t.Errorf("got %d rows with limit=3, want 3", len(rows))
	}
}

// TestArbiterSynthesisReader_Interface verifies the compile-time interface
// satisfaction is also checkable at runtime (issue #1176).
func TestArbiterSynthesisReader_Interface(t *testing.T) {
	s := newTestStore(t)
	var store Store = s
	if _, ok := store.(ArbiterSynthesisReader); !ok {
		t.Error("SQLiteStore does not satisfy ArbiterSynthesisReader")
	}
}
