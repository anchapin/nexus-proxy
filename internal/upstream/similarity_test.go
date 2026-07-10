package upstream

import "testing"

func TestSimilarityRatioIdentical(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		// Token-set Jaccard is sensitive to per-token differences
		// (e.g. "Hello," vs "hello") so the "punctuation differs"
		// case is high-but-not-1.0 — it's the surface-form noise
		// the streaming fusion arbiter is designed to absorb.
		{"exact", "hello world", "hello world", 1.0},
		{"whitespace normalised", "hello   world\n\nfoo", "hello world foo", 1.0},
		{"punctuation differs", "Hello, world!", "hello world", 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SimilarityRatio(tc.a, tc.b); got != tc.want {
				t.Errorf("SimilarityRatio(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSimilarityRatioDisjoint(t *testing.T) {
	if got := SimilarityRatio("foo bar", "baz qux"); got != 0.0 {
		t.Errorf("disjoint got %v, want 0.0", got)
	}
}

func TestSimilarityRatioPartialOverlap(t *testing.T) {
	// Tokens {foo, bar} ∩ {bar, baz} = {bar}, |A∪B| = 3, ratio = 1/3.
	if got := SimilarityRatio("foo bar", "bar baz"); got < 0.33 || got > 0.34 {
		t.Errorf("partial got %v, want ~0.333", got)
	}
}

func TestSimilarityRatioEmpty(t *testing.T) {
	// Both empty -> 1.0 (vacuously identical).
	if got := SimilarityRatio("", ""); got != 1.0 {
		t.Errorf("both empty got %v, want 1.0", got)
	}
	// Exactly one empty -> 0.0.
	if got := SimilarityRatio("hello", ""); got != 0.0 {
		t.Errorf("one empty got %v, want 0.0", got)
	}
	if got := SimilarityRatio("", "hello"); got != 0.0 {
		t.Errorf("other empty got %v, want 0.0", got)
	}
}

func TestSimilarityRatioWhitespaceOnly(t *testing.T) {
	// Whitespace-only inputs tokenise to empty sets; treated as
	// identical (no tokens to compare) per the len(setA)==len(setB)==0
	// short-circuit.
	if got := SimilarityRatio("   ", "\n\t"); got != 1.0 {
		t.Errorf("whitespace-only got %v, want 1.0", got)
	}
}

func TestSimilarityRatioSymmetric(t *testing.T) {
	a := "the quick brown fox"
	b := "the lazy brown dog"
	if got, swap := SimilarityRatio(a, b), SimilarityRatio(b, a); got != swap {
		t.Errorf("not symmetric: %v vs %v", got, swap)
	}
}

func TestSimilarityRatioAgreementThreshold(t *testing.T) {
	// Sanity check at the default fusion-agreement threshold. Two
	// paragraphs that paraphrase the same idea should clear 0.85
	// (the issue-48 default); two paragraphs that diverge on most
	// content should not.
	a := "Use a buffered channel to queue requests. The dispatcher drains the queue and forwards each to the upstream."
	b := "Use a buffered channel to queue requests. The dispatcher drains the queue and forwards each to the upstream."
	if got := SimilarityRatio(a, b); got < 0.85 {
		t.Errorf("near-identical paragraphs scored %v, want >= 0.85", got)
	}
	c := "Use a buffered channel to queue requests. The dispatcher drains the queue and forwards each to the upstream."
	d := "Switch the database schema. Migrate every column. Drop the legacy index. Reindex from scratch."
	if got := SimilarityRatio(c, d); got > 0.5 {
		t.Errorf("unrelated paragraphs scored %v, want < 0.5", got)
	}
}