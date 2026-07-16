package rag

import (
	"context"
	"math"
	"math/rand"
	"testing"
)

// genVector returns a deterministic pseudo-random unit vector of the
// given dimension. Using a fixed seed keeps benchmark results
// reproducible across runs.
func genVector(dims, seed int) []float64 {
	r := rand.New(rand.NewSource(int64(seed)))
	v := make([]float64, dims)
	var norm float64
	for i := range v {
		v[i] = r.NormFloat64()
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for i := range v {
		v[i] /= norm
	}
	return v
}

// constEmbedder always returns the same vector, avoiding the Ollama
// round-trip so the benchmark isolates the cosine-scan cost.
type constEmbedder struct{ vec []float64 }

func (e constEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	return e.vec, nil
}

func (e constEmbedder) IsHealthy(context.Context) bool { return true }
func (e constEmbedder) IsBreakerOpen() bool            { return false }
func (e constEmbedder) RecordBreakerSuccess()          {}

// BenchmarkCosineSimilarity measures the raw dot-product + norm
// computation at the default 768-dimension embedding width. This is
// the inner loop of Retrieve — called once per indexed example.
func BenchmarkCosineSimilarity(b *testing.B) {
	dims := 768
	a := genVector(dims, 1)
	c := genVector(dims, 2)
	b.SetBytes(int64(dims * 8 * 2)) // two float64 slices
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CosineSimilarity(a, c)
	}
}

// BenchmarkRetrieve measures the full brute-force scan at store sizes
// of 10, 50, and 200 examples — the range a developer-curated
// few-shot directory might hold. The 768-dimension vectors match the
// default nomic-embed-text output.
//
// This benchmark answers the question: "does the brute-force O(n×d)
// scan need optimisation?" If the per-call cost stays under ~100µs at
// 200 examples (38,400 float multiplies), the answer is no.
func BenchmarkRetrieve(b *testing.B) {
	dims := 768
	queryVec := genVector(dims, 999)
	emb := constEmbedder{vec: queryVec}

	for _, n := range []int{10, 50, 100, 200} {
		store := NewStore(emb, 0.0) // threshold 0 so we always pick best
		for i := 0; i < n; i++ {
			store.Add(
				"example_"+itoa(i)+".go",
				"// snippet "+itoa(i),
				genVector(dims, i+1),
			)
		}
		b.Run(itoa(n)+"examples", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := store.Retrieve(context.Background(), "test prompt"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// itoa is a tiny strconv.Itoa replacement to avoid pulling strconv
// just for the sub-benchmark labels.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
