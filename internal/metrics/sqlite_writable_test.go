package metrics

import (
	"testing"
)

// TestWritable_LiveStoreReturnsTrue verifies that Writable reports
// true on a freshly opened store whose underlying connection is
// healthy. This is the happy path the /status handler exercises
// (cmd/nexus/main.go MetricsDBWritable) to advertise DB liveness.
func TestWritable_LiveStoreReturnsTrue(t *testing.T) {
	s := newTestStore(t)
	if !s.Writable() {
		t.Fatal("Writable() = false on live store, want true")
	}
}

// TestWritable_AfterCloseReturnsFalse verifies that Writable flips
// to false once the store has been closed: db.Ping() must surface
// the closed-handle error so /status stops advertising a dead
// connection. The store is opened directly (not via newTestStore)
// because this test owns the Close lifecycle itself.
func TestWritable_AfterCloseReturnsFalse(t *testing.T) {
	s, err := OpenWithLogger(t.TempDir()+"/metrics.db", silentLogger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*SQLiteStore)
	if !ss.Writable() {
		t.Fatal("Writable() = false before close, want true")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ss.Writable() {
		t.Fatal("Writable() = true after Close, want false")
	}
}

// TestWritable_NilReceiverReturnsFalse exercises the nil-safe guard
// at the top of Writable. It mirrors cmd/nexus/main.go, which holds
// the store behind the metrics.Store interface and type-asserts to
// *SQLiteStore before calling Writable(). When the store is a
// typed-nil pointer boxed in an interface the assertion succeeds,
// yielding a nil concrete receiver — Writable must return false and
// must not panic.
func TestWritable_NilReceiverReturnsFalse(t *testing.T) {
	var concrete *SQLiteStore
	// Box the typed nil in the Store interface exactly as main.go
	// does (metricsStore is declared as metrics.Store).
	var store Store = concrete
	recovered, ok := store.(*SQLiteStore)
	if !ok {
		t.Fatal("type assertion (*SQLiteStore) failed for boxed nil pointer")
	}
	if recovered.Writable() {
		t.Error("nil receiver Writable() = true, want false")
	}
}
