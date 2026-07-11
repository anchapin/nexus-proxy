package upstream

import "strings"

// SimilarityRatio returns the Jaccard similarity of the token sets of a
// and b. Tokens are produced by strings.Fields (whitespace splitting),
// so word boundaries (spaces, tabs, newlines) define token identity.
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
// deduplicated token set. Caller is responsible for any further
// normalization; the streaming fusion arbiter skip decision does not
// need case folding or stemming.
func tokenSet(s string) map[string]struct{} {
	out := make(map[string]struct{}, 16)
	for _, tok := range strings.Fields(s) {
		out[tok] = struct{}{}
	}
	return out
}
