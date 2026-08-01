package rag

import (
	"math"
	"testing"
)

// bytesToFloats interprets a byte slice as a sequence of float64 values
// (little-endian). Trailing bytes that don't form a complete 8-byte float
// are ignored. This lets the fuzzer generate arbitrary-length float vectors
// from its native []byte input type.
func bytesToFloats(data []byte) []float64 {
	const size = 8
	n := len(data) / size
	if n == 0 {
		return nil
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		bits := uint64(data[i*size]) | uint64(data[i*size+1])<<8 |
			uint64(data[i*size+2])<<16 | uint64(data[i*size+3])<<24 |
			uint64(data[i*size+4])<<32 | uint64(data[i*size+5])<<40 |
			uint64(data[i*size+6])<<48 | uint64(data[i*size+7])<<56
		out[i] = math.Float64frombits(bits)
	}
	return out
}

// FuzzSimilarity feeds arbitrary float64 slices of varying length into
// CosineSimilarity. The result must always be in [-1, 1], must never be
// NaN or Inf, and must handle zero-length / mismatched-dimension vectors
// without panicking.
func FuzzSimilarity(f *testing.F) {
	// Seed corpus encoded as byte slices (8 bytes per float64, little-endian).

	// Aligned identical: {1.0, 0.0, 0.0}
	one := math.Float64bits(1.0)
	zero := math.Float64bits(0.0)
	aligned := []byte{}
	for _, v := range []uint64{one, zero, zero} {
		aligned = append(aligned, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	f.Add(aligned, aligned)

	// Empty vectors.
	f.Add([]byte{}, []byte{})

	// Mismatched dimensions: 1 element vs 3 elements.
	single := []byte{}
	for _, v := range []uint64{one} {
		single = append(single, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	f.Add(single, aligned)

	// Zero-norm divisor: all zeros, 2 elements.
	zeroPair := []byte{}
	for i := 0; i < 2; i++ {
		v := zero
		zeroPair = append(zeroPair, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	f.Add(zeroPair, aligned)

	// Overflow-risk: large values.
	big := math.Float64bits(1e308)
	bigVec := []byte{}
	for i := 0; i < 2; i++ {
		v := big
		bigVec = append(bigVec, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	f.Add(bigVec, bigVec)

	// Opposite direction: {1,0} vs {-1,0}.
	negOne := math.Float64bits(-1.0)
	negVec := []byte{}
	for _, v := range []uint64{negOne, zero} {
		negVec = append(negVec, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	posVec := []byte{}
	for _, v := range []uint64{one, zero} {
		posVec = append(posVec, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
	f.Add(posVec, negVec)

	f.Fuzz(func(t *testing.T, dataA, dataB []byte) {
		a := bytesToFloats(dataA)
		b := bytesToFloats(dataB)
		result := CosineSimilarity(a, b)

		// Invariant 1: result must be a normal (or zero) number — never NaN or Inf.
		if math.IsNaN(result) {
			t.Fatalf("CosineSimilarity returned NaN for a=%v b=%v", a, b)
		}
		if math.IsInf(result, 0) {
			t.Fatalf("CosineSimilarity returned Inf for a=%v b=%v", a, b)
		}

		// Invariant 2: cosine similarity is bounded in [-1, 1].
		// Allow a small epsilon for floating-point drift on near-overflow inputs.
		if result < -1.000001 || result > 1.000001 {
			t.Fatalf("CosineSimilarity=%v is out of [-1,1] for a=%v b=%v", result, a, b)
		}
	})
}
