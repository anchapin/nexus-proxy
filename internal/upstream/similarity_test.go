package upstream

import "testing"

func TestSimilarityRatioIdentical(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		{"exact", "hello world", "hello world", 1.0},
		{"whitespace normalised", "hello   world\n\nfoo", "hello world foo", 1.0},
		{"punctuation differs", "Hello, world!", "hello world", 1.0},
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

func TestSimilarityRatioCaseInsensitive(t *testing.T) {
	if got := SimilarityRatio("Use JSON", "use json"); got != 1.0 {
		t.Errorf("SimilarityRatio(%q, %q) = %v, want 1.0", "Use JSON", "use json", got)
	}
}

func TestSimilarityRatioPunctuationTolerant(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		min  float64
	}{
		{"case + punctuation", "Hello, world!", "hello world", 0.8},
		{"trailing punctuation", "hello.", "hello", 1.0},
		{"multiple punctuation", "foo!!!", "foo", 1.0},
		{"brackets", "[foo]", "foo", 1.0},
		{"parens", "(bar)", "bar", 1.0},
		{"mixed case + punctuation", "The Quick Brown Fox!", "the quick brown fox", 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SimilarityRatio(tc.a, tc.b); got < tc.min {
				t.Errorf("SimilarityRatio(%q, %q) = %v, want >= %v", tc.a, tc.b, got, tc.min)
			}
		})
	}
}

func TestNormalizeToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Hello", "hello"},
		{"WORLD", "world"},
		{"foo.", "foo"},
		{".bar", "bar"},
		{"hello!", "hello"},
		{"[test]", "test"},
		{"(example)", "example"},
		{`"quoted"`, "quoted"},
		{`"hello"`, "hello"},
		{`hello\nworld`, "hello world"},
		{`a\\b`, "a\\b"},
		{"Already_Lower", "already_lower"},
		{"   spaces   ", "spaces"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeToken(tc.in); got != tc.want {
				t.Errorf("normalizeToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
