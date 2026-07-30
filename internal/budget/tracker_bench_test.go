package budget

import (
	"testing"
)

func BenchmarkRecord_SmallEntries(b *testing.B) {
	st := NewSpendTracker(10000.0)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		st.Record(0.001)
	}
}

func BenchmarkRecord_LargeWindow(b *testing.B) {
	st := NewSpendTracker(10000.0)

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		st.Record(0.001)
	}
}

func BenchmarkWouldExceed_SmallEntries(b *testing.B) {
	st := NewSpendTracker(10.0)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		st.WouldExceed(0.01)
	}
}

func BenchmarkWouldExceed_LargeWindow(b *testing.B) {
	st := NewSpendTracker(10.0)

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		st.WouldExceed(0.01)
	}
}

func BenchmarkCurrentSpend_LargeWindow(b *testing.B) {
	st := NewSpendTracker(10000.0)

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = st.CurrentSpend()
	}
}

func BenchmarkConcurrentRecord(b *testing.B) {
	st := NewSpendTracker(10000.0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			st.Record(0.001)
		}
	})
}

func BenchmarkConcurrentWouldExceed(b *testing.B) {
	st := NewSpendTracker(10.0)
	for i := 0; i < 1000; i++ {
		st.Record(0.001)
	}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			st.WouldExceed(0.01)
		}
	})
}

func BenchmarkRecord_10kPerMinute(b *testing.B) {
	st := NewSpendTracker(10000.0)

	b.SetBytes(10 * 1024)
	b.StartTimer()

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	b.StopTimer()
}

func BenchmarkWouldExceed_10kPerMinute(b *testing.B) {
	st := NewSpendTracker(10000.0)

	for i := 0; i < 10000; i++ {
		st.Record(0.001)
	}

	b.SetBytes(10 * 1024)
	b.StartTimer()

	for i := 0; i < 10000; i++ {
		_ = st.WouldExceed(0.001)
	}

	b.StopTimer()
}

func BenchmarkMixed_10kEntries(b *testing.B) {
	st := NewSpendTracker(10000.0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			st.Record(0.001)
			_ = st.WouldExceed(0.001)
		}
	})
}
