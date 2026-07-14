package middleware

import (
	"context"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/rag"
)

// mockStore implements rag.RAGStore for testing.
type mockStore struct {
	retrieveFn func(ctx context.Context, query string) (*rag.FewShotExample, float64, error)
}

func (m *mockStore) Retrieve(ctx context.Context, query string) (*rag.FewShotExample, float64, error) {
	if m.retrieveFn != nil {
		return m.retrieveFn(ctx, query)
	}
	return nil, 0, nil
}
func (m *mockStore) Add(filename, content string, embedding []float64) {}
func (m *mockStore) Size() int                                         { return 0 }
func (m *mockStore) Threshold() float64                                { return 0.5 }

// --- MiddlewareFunc tests ---

func TestMiddlewareFunc_Name(t *testing.T) {
	m := MiddlewareFunc{name: "test-mw", fn: func([]interface{}) ([]interface{}, error) {
		return nil, nil
	}}
	if m.Name() != "test-mw" {
		t.Errorf("Name() = %q, want %q", m.Name(), "test-mw")
	}
}

func TestMiddlewareFunc_Transform(t *testing.T) {
	called := false
	m := MiddlewareFunc{name: "test", fn: func(msgs []interface{}) ([]interface{}, error) {
		called = true
		return []interface{}{"output"}, nil
	}}
	out, err := m.Transform([]interface{}{"input"})
	if err != nil {
		t.Fatalf("Transform() error = %v", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
	if len(out) != 1 || out[0] != "output" {
		t.Errorf("Transform() = %v, want [%v]", out, "output")
	}
}

func TestNewMiddleware(t *testing.T) {
	m := NewMiddleware("foo", func(msgs []interface{}) ([]interface{}, error) {
		return msgs, nil
	})
	if m.Name() != "foo" {
		t.Errorf("Name() = %q, want %q", m.Name(), "foo")
	}
}

// --- ragMiddleware tests ---

func TestRAGMiddleware_Name(t *testing.T) {
	store := &mockStore{}
	r := NewRAGMiddleware(store, 0.5).(*ragMiddleware)
	if r.Name() != "rag" {
		t.Errorf("Name() = %q, want %q", r.Name(), "rag")
	}
}

func TestRAGMiddleware_Transform_ReturnsInput(t *testing.T) {
	store := &mockStore{}
	r := NewRAGMiddleware(store, 0.5)
	msgs := []interface{}{"hello"}
	got, err := r.Transform(msgs)
	if err != nil {
		t.Fatalf("Transform() error = %v", err)
	}
	// Transform is a no-op for RAG; it should return the same messages
	if len(got) != len(msgs) {
		t.Errorf("Transform() len = %d, want %d", len(got), len(msgs))
	}
}

func TestRAGMiddleware_TransformContext_NoMatch(t *testing.T) {
	store := &mockStore{
		retrieveFn: func(ctx context.Context, query string) (*rag.FewShotExample, float64, error) {
			return nil, 0, nil
		},
	}
	r := NewRAGMiddleware(store, 0.5)
	msgs := []interface{}{"hello"}
	got, err := r.TransformContext(context.Background(), msgs)
	if err != nil {
		t.Fatalf("TransformContext() error = %v", err)
	}
	// No match → no injection
	if len(got) != len(msgs) {
		t.Errorf("TransformContext() len = %d, want %d", len(got), len(msgs))
	}
}

func TestRAGMiddleware_TransformContext_WithMatch(t *testing.T) {
	store := &mockStore{
		retrieveFn: func(ctx context.Context, query string) (*rag.FewShotExample, float64, error) {
			return &rag.FewShotExample{
				Filename:  "test.go",
				Content:   "test content",
				Embedding: []float64{0.1, 0.2},
			}, 0.9, nil
		},
	}
	r := NewRAGMiddleware(store, 0.5)
	// InjectRAG looks for a user-role message with a string content field
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
	}
	got, err := r.TransformContext(context.Background(), msgs)
	if err != nil {
		t.Fatalf("TransformContext() error = %v", err)
	}
	// TransformContext calls rag.Retrieve → if example found, InjectRAG
	// appends a context block to the user message content, growing the slice
	// length only when the context block is non-empty (FormatInjection always
	// returns non-empty for valid examples). The example returned by the mock
	// has Filename + Content so FormatInjection is non-empty.
	// Note: InjectRAG modifies msgs in-place by appending to the content field.
	wantContent := "hello" + rag.FormatInjection(&rag.FewShotExample{
		Filename: "test.go",
		Content:  "test content",
	})
	gotContent := got[0].(map[string]interface{})["content"].(string)
	if gotContent != wantContent {
		t.Errorf("TransformContext() content = %q, want %q", gotContent, wantContent)
	}
}

func TestNewRAGMiddleware(t *testing.T) {
	store := &mockStore{}
	mw := NewRAGMiddleware(store, 0.7)
	if mw.Name() != "rag" {
		t.Errorf("Name() = %q, want %q", mw.Name(), "rag")
	}
}

// --- Registry tests ---

func TestRegister_Success(t *testing.T) {
	Init("", "", false)
	m := NewMiddleware("register-test", func([]interface{}) ([]interface{}, error) {
		return nil, nil
	})
	Register(m)
	if Get("register-test") == nil {
		t.Error("Get() returned nil after Register()")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	Init("", "", false)
	m := NewMiddleware("dup-test", func([]interface{}) ([]interface{}, error) {
		return nil, nil
	})
	Register(m)
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register() did not panic on duplicate name")
		}
	}()
	Register(m)
}

func TestGet_NotFound(t *testing.T) {
	Init("", "", false)
	if Get("nonexistent") != nil {
		t.Error("Get() should return nil for unknown name")
	}
}

// --- BuildChain tests ---

func TestBuildChain_EmptySpec(t *testing.T) {
	Init("", "", false)
	chain, err := BuildChain("")
	if err != nil {
		t.Fatalf("BuildChain() error = %v", err)
	}
	if len(chain) != 4 {
		t.Errorf("BuildChain() len = %d, want 4", len(chain))
	}
}

func TestBuildChain_ValidSpec(t *testing.T) {
	Init("", "", false)
	chain, err := BuildChain("promptEngineering,compressJSONBlocks")
	if err != nil {
		t.Fatalf("BuildChain() error = %v", err)
	}
	if len(chain) != 2 {
		t.Errorf("BuildChain() len = %d, want 2", len(chain))
	}
	if chain[0].Name() != "promptEngineering" {
		t.Errorf("chain[0].Name() = %q, want %q", chain[0].Name(), "promptEngineering")
	}
	if chain[1].Name() != "compressJSONBlocks" {
		t.Errorf("chain[1].Name() = %q, want %q", chain[1].Name(), "compressJSONBlocks")
	}
}

func TestBuildChain_UnknownName(t *testing.T) {
	Init("", "", false)
	_, err := BuildChain("promptEngineering,unknownMiddleware,compressJSONBlocks")
	if err == nil {
		t.Fatal("BuildChain() expected error for unknown middleware, got nil")
	}
}

func TestBuildChain_EmptyAfterTrim(t *testing.T) {
	Init("", "", false)
	_, err := BuildChain("promptEngineering, , compressJSONBlocks")
	if err != nil {
		t.Fatalf("BuildChain() should skip empty entries, got error: %v", err)
	}
}

func TestBuildChain_AllEmpty(t *testing.T) {
	Init("", "", false)
	_, err := BuildChain(" , ")
	if err == nil {
		t.Fatal("BuildChain() expected error for all-empty chain")
	}
}

// --- DefaultChain tests ---

func TestDefaultChain_HasFourEntries(t *testing.T) {
	Init("", "", false)
	chain := DefaultChain()
	if len(chain) != 4 {
		t.Errorf("DefaultChain() len = %d, want 4", len(chain))
	}
	names := make([]string, len(chain))
	for i, m := range chain {
		names[i] = m.Name()
	}
	want := []string{"promptEngineering", "rag", "compressJSONBlocks", "appendSystemNote"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("DefaultChain()[%d].Name() = %q, want %q", i, names[i], w)
		}
	}
}

// --- Init tests ---

func TestInit_ReInitializesRegistry(t *testing.T) {
	Init("", "", false)
	// Register a custom middleware under a name not in the built-in set
	Register(NewMiddleware("custom-init-test", func([]interface{}) ([]interface{}, error) {
		return []interface{}{"custom"}, nil
	}))

	// Re-init should clear the registry and re-register only built-ins
	Init("sysprompt", "toon", false)

	// custom-init-test should be gone after re-init
	if Get("custom-init-test") != nil {
		t.Error("After re-init, custom middleware should be removed")
	}
	// promptEngineering should still be present (built-in)
	chain, err := BuildChain("promptEngineering")
	if err != nil {
		t.Fatalf("BuildChain() error = %v", err)
	}
	// After re-init with "sysprompt" as metaPrompt, Transform adds it as system msg
	out, _ := chain[0].Transform([]interface{}{"in"})
	if len(out) != 2 { // [system with "sysprompt", "in"]
		t.Errorf("After re-init with metaPrompt, expected 2 messages, got: %v", out)
	}
}

// --- Integration: full chain ---

func TestBuildChain_CanBuildPartialChain(t *testing.T) {
	Init("", "", false)
	// Operator can drop RAG from the chain
	chain, err := BuildChain("promptEngineering,compressJSONBlocks,appendSystemNote")
	if err != nil {
		t.Fatalf("BuildChain() error = %v", err)
	}
	if len(chain) != 3 {
		t.Errorf("BuildChain() len = %d, want 3", len(chain))
	}
}
