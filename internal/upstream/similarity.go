package upstream

import (
	"context"
	"log/slog"
	"math"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/rag"
)

// SimilarityMode selects the algorithm for fusion agreement detection
// (issue #1244).
type SimilarityMode string

const (
	// SimilarityModeJaccard uses whitespace-split token-set Jaccard — the
	// original metric. Cheap but blind to synonyms.
	SimilarityModeJaccard SimilarityMode = "jaccard"
	// SimilarityModeSemantic uses cosine similarity via the RAG embedder.
	// Falls back to Jaccard when the embedder is nil or unhealthy.
	SimilarityModeSemantic SimilarityMode = "semantic"
)

// FusionSimilarityConfig bundles the similarity mode and optional embedder
// for Panel / PanelStreaming. A nil Embedder in semantic mode falls back
// to Jaccard at compute time (issue #1244).
type FusionSimilarityConfig struct {
	Mode     SimilarityMode
	Embedder rag.Embedder
}

// ComputeSimilarity dispatches to the configured similarity algorithm.
// It always returns a value in [0, 1] and records a Prometheus-compatible
// metric observation (issue #1244).
func (c FusionSimilarityConfig) ComputeSimilarity(ctx context.Context, a, b string) float64 {
	switch c.Mode {
	case SimilarityModeSemantic:
		score := SemanticSimilarityRatio(ctx, a, b, c.Embedder)
		// Check whether semantic actually ran (embedder available).
		// If the embedder was nil/unhealthy, SemanticSimilarityRatio
		// falls back to Jaccard internally — we still count it as a
		// semantic-mode invocation but note the fallback was needed.
		RecordSemanticSimilarity(score)
		return score
	default:
		RecordJaccardSimilarity()
		return SimilarityRatio(a, b)
	}
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

// SemanticSimilarityRatio computes cosine similarity between two text
// strings using the provided RAG embedder. It embeds both strings and
// uses rag.CosineSimilarity for the computation.
//
// When embedder is nil, unhealthy, or if either embedding call fails,
// it logs a warning and falls back to Jaccard similarity (SimilarityRatio)
// so the fusion agreement check always produces a result.
//
// Returns a value in [0, 1]: 1.0 for identical/empty inputs, 0.0 for
// completely dissimilar content.
func SemanticSimilarityRatio(ctx context.Context, a, b string, embedder rag.Embedder) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}
	if embedder == nil || !embedder.IsHealthy(ctx) {
		slog.Warn("semantic similarity: embedder nil or unhealthy, falling back to Jaccard")
		return SimilarityRatio(a, b)
	}

	vecA, err := embedder.Embed(ctx, a)
	if err != nil {
		slog.Warn("semantic similarity: embed a failed, falling back to Jaccard",
			slog.Any("error", err))
		return SimilarityRatio(a, b)
	}
	vecB, err := embedder.Embed(ctx, b)
	if err != nil {
		slog.Warn("semantic similarity: embed b failed, falling back to Jaccard",
			slog.Any("error", err))
		return SimilarityRatio(a, b)
	}

	return cosineSimilarityClamped(vecA, vecB)
}

// cosineSimilarityClamped computes cosine similarity between two vectors
// and clamps the result to [0, 1]. This is a local reimplementation
// that avoids importing internal/rag from internal/upstream to keep the
// dependency graph clean (the embedder interface already crosses, but
// the concrete math function should stay local).
func cosineSimilarityClamped(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	result := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0
	}
	if result > 1 {
		return 1
	}
	if result < 0 {
		return 0 // clamp negative to 0 for similarity ratio
	}
	return result
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
