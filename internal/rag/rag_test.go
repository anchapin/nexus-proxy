package rag

import (
	"context"
	"math"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 0.0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1.0},
		{"zero vector", []float64{0, 0}, []float64{1, 2}, 0.0},
		{"both zero", []float64{0, 0}, []float64{0, 0}, 0.0},
		{"45 degrees", []float64{1, 1}, []float64{1, 0}, 1 / math.Sqrt2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CosineSimilarity(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type stubEmbedder struct {
	vecs map[string][]float64
	err  error
}

func (s stubEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.vecs[text]; ok {
		return v, nil
	}
	return []float64{0, 0, 0}, nil
}

func TestRetrieveThreshold(t *testing.T) {
	emb := stubEmbedder{vecs: map[string][]float64{
		"prompt":     {1, 0, 0},
		"matching":   {0.9, 0.1, 0},
		"unrelated":  {0, 1, 0},
		"weak match": {0.4, 0.5, 0},
	}}
	store := NewStore(emb, 0.55)
	store.examples = []FewShotExample{
		{Filename: "matching.go", Content: "matching", Embedding: emb.vecs["matching"]},
		{Filename: "unrelated.go", Content: "unrelated", Embedding: emb.vecs["unrelated"]},
		{Filename: "weak.go", Content: "weak match", Embedding: emb.vecs["weak match"]},
	}

	ex, score, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex == nil {
		t.Fatal("expected a match above threshold")
	}
	if ex.Filename != "matching.go" {
		t.Errorf("matched %s, want matching.go", ex.Filename)
	}
	if score <= 0.55 {
		t.Errorf("score %v should exceed threshold", score)
	}
}

func TestRetrieveEmptyStore(t *testing.T) {
	store := NewStore(stubEmbedder{}, 0.55)
	ex, _, err := store.Retrieve(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex != nil {
		t.Errorf("expected nil from empty store, got %+v", ex)
	}
}

func TestRetrieveBelowThreshold(t *testing.T) {
	emb := stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
		"weak":   {0.3, 0.4, 0},
	}}
	store := NewStore(emb, 0.9)
	store.examples = []FewShotExample{
		{Filename: "weak.go", Content: "weak", Embedding: emb.vecs["weak"]},
	}
	ex, _, err := store.Retrieve(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if ex != nil {
		t.Errorf("expected no match below threshold, got %s", ex.Filename)
	}
}

func TestRetrieveEmbedderError(t *testing.T) {
	store := NewStore(stubEmbedder{err: errSentinel}, 0.55)
	store.examples = []FewShotExample{{Filename: "x.go", Content: "x", Embedding: []float64{1, 0}}}
	if _, _, err := store.Retrieve(context.Background(), "prompt"); err == nil {
		t.Error("expected error from embedder")
	}
}

var errSentinel = errStub("embed down")

type errStub string

func (e errStub) Error() string { return string(e) }
