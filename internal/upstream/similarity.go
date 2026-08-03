package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Embedder turns text into a vector. Defined here to avoid importing
// internal/rag into internal/upstream. The concrete implementation is
// rag.Embedder; callers pass the concrete type via type assertion.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// SemanticSimilarityRatio returns the cosine similarity between the
// embedding vectors of a and b computed via embedder. It falls back
// to Jaccard ( SimilarityRatio) when embedder is nil or either
// embedding call fails. The fallback makes semantic mode resilient to
// embedder unavailability (circuit breaker open, Ollama down, etc.).
//
// Cosine similarity is computed as: (A·B) / (||A|| * ||B||)
// where A and B are the embedding vectors. The result is in [0, 1]
// for non-negative embeddings (typical for LLM embedders).
//
// When fallback to Jaccard occurs, the returned error is non-nil
// so callers can log the degradation.
func SemanticSimilarityRatio(a, b string, embedder Embedder) (float64, error) {
	if embedder == nil {
		return SimilarityRatio(a, b), fmt.Errorf("semantic mode: embedder is nil, falling back to Jaccard")
	}
	if a == "" && b == "" {
		return 1.0, nil
	}
	if a == "" || b == "" {
		return 0.0, nil
	}

	ctx := context.Background()
	eg, egCtx := errgroup.WithContext(ctx)

	var vecA, vecB []float64
	var errA, errB error

	eg.Go(func() error {
		vecA, errA = embedder.Embed(egCtx, a)
		return errA
	})
	eg.Go(func() error {
		vecB, errB = embedder.Embed(egCtx, b)
		return errB
	})

	if err := eg.Wait(); err != nil {
		return SimilarityRatio(a, b), fmt.Errorf("semantic mode: embed failed: %w, falling back to Jaccard", err)
	}

	if len(vecA) == 0 || len(vecB) == 0 {
		return SimilarityRatio(a, b), fmt.Errorf("semantic mode: empty embedding returned, falling back to Jaccard")
	}

	if len(vecA) != len(vecB) {
		return SimilarityRatio(a, b), fmt.Errorf("semantic mode: embedding dimension mismatch (%d vs %d), falling back to Jaccard", len(vecA), len(vecB))
	}

	dot := cosineDot(vecA, vecB)
	normA := cosineNorm(vecA)
	normB := cosineNorm(vecB)

	if normA == 0 || normB == 0 {
		return SimilarityRatio(a, b), fmt.Errorf("semantic mode: zero norm, falling back to Jaccard")
	}

	return dot / (normA * normB), nil
}

// cosineDot returns the dot product of two vectors.
func cosineDot(a, b []float64) float64 {
	var sum float64
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// cosineNorm returns the L2 norm of a vector.
func cosineNorm(vec []float64) float64 {
	var sum float64
	for _, v := range vec {
		sum += v * v
	}
	return sqrt(sum)
}

// sqrt returns the square root using math.Sqrt.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return math.Sqrt(x)
}

// computeSimilarity returns the agreement score between a and b. When
// mode is "semantic" it uses SemanticSimilarityRatio (cosine similarity
// via embedder); otherwise it falls back to SimilarityRatio (Jaccard).
// The embedder may be nil when mode is "jaccard".
func computeSimilarity(a, b string, embedder Embedder, mode string) float64 {
	if mode == "semantic" {
		ratio, err := SemanticSimilarityRatio(a, b, embedder)
		if err == nil {
			return ratio
		}
		// Semantic failed (embedder unavailable, dimension mismatch, etc.);
		// fall through to Jaccard so the panel still makes progress.
		slog.Warn("semantic similarity failed, falling back to Jaccard",
			slog.String("error", err.Error()))
	}
	return SimilarityRatio(a, b)
}

// SimilarityRatio returns the Jaccard similarity of the token sets of a
// and b. Tokens are produced by strings.Fields (whitespace splitting),
// then normalized (lowercased, punctuation trimmed, JSON escapes unwrapped)
// before set insertion.
//
//   - 1.0 when both inputs are empty (vacuously identical)
//   - 0.0 when exactly one is empty
//   - |A ∩ B| / |A ∪ B| otherwise
//
// The implementation is stdlib-only and intentionally cheaper than
// Levenshtein / edit-distance: progressive fusion (issue #48) only
// needs a yes/no answer for "did these two models give roughly the
// same answer?" so the proxy can skip the arbiter. Jaccard is
// symmetric in [0,1] and tolerant of the small surface-level
// differences (extra/missing trailing punctuation, capitalization,
// JSON escaping) two LLMs produce when their answers semantically
// agree.
func SimilarityRatio(a, b string) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}
	setA := tokenSet(a)
	setB := tokenSet(b)
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	// |A ∩ B|: iterate the smaller set to keep this O(min(|A|,|B|)).
	var inter int
	smaller, larger := setA, setB
	if len(setB) < len(setA) {
		smaller, larger = setB, setA
	}
	for k := range smaller {
		if _, ok := larger[k]; ok {
			inter++
		}
	}
	// |A ∪ B| = |A| + |B| - |A ∩ B|.
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

// tokenSet splits s on whitespace (per strings.Fields) and returns the
// deduplicated token set after normalization (lowercase, punctuation
// trim, JSON-escape unwrapping).
func tokenSet(s string) map[string]struct{} {
	out := make(map[string]struct{}, 16)
	for _, tok := range strings.Fields(s) {
		norm := normalizeToken(tok)
		if norm != "" {
			out[norm] = struct{}{}
		}
	}
	return out
}

// normalizeToken applies surface-form normalization: lowercase, trim
// ASCII punctuation and surrounding whitespace, and unwrap JSON escape
// sequences.
func normalizeToken(tok string) string {
	tok = strings.ToLower(tok)
	tok = strings.TrimSpace(tok)
	tok = strings.Trim(tok, ".,;:!?()[]{}\"'")
	tok = strings.ReplaceAll(tok, `\"`, `"`)
	tok = strings.ReplaceAll(tok, `\n`, " ")
	tok = strings.ReplaceAll(tok, `\\`, `\`)
	return tok
}
