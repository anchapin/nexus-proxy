package ioutils

import (
	"bytes"
	"testing"
)

// BenchmarkReadAllLimited measures the un-pooled baseline: io.ReadAll
// allocates a fresh buffer per call. This is the path the cascade used
// before issue #1177.
func BenchmarkReadAllLimited(b *testing.B) {
	cases := []struct {
		name string
		kb   int
	}{
		{"1KB", 1},
		{"4KB", 4},
		{"16KB", 16},
		{"64KB", 64},
	}
	for _, tc := range cases {
		content := bytes.Repeat([]byte("x"), tc.kb*1024)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(content)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := ReadAllLimited(bytes.NewReader(content), 64*1024*1024)
				if err != nil {
					b.Fatal(err)
				}
				_ = body
			}
		})
	}
}

// BenchmarkReadAllLimitedPooled measures the pooled path: the underlying
// *bytes.Buffer is recycled via sync.Pool, so allocs/op should drop to
// near-zero after the first iteration (issue #1177 acceptance criteria).
func BenchmarkReadAllLimitedPooled(b *testing.B) {
	origMax := PoolBufferMaxBytes()
	b.Cleanup(func() { SetPoolBufferMaxBytes(origMax) })
	SetPoolBufferMaxBytes(1 << 20) // 1 MiB retention cap

	cases := []struct {
		name string
		kb   int
	}{
		{"1KB", 1},
		{"4KB", 4},
		{"16KB", 16},
		{"64KB", 64},
	}
	for _, tc := range cases {
		content := bytes.Repeat([]byte("x"), tc.kb*1024)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(content)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf, err := ReadAllLimitedPooled(bytes.NewReader(content), 64*1024*1024)
				if err != nil {
					b.Fatal(err)
				}
				PutBuffer(buf)
			}
		})
	}
}
