package rag

import (
	"errors"
	"fmt"
	"testing"
)

// TestCircuitKindOllama verifies that CircuitKind extracts the "ollama"
// kind from a circuit error produced for the Ollama embedder. This is the
// path operators rely on to diagnose which embedder's breaker tripped.
func TestCircuitKindOllama(t *testing.T) {
	err := newCircuitError(circuitKindOllama)
	if got := CircuitKind(err); got != "ollama" {
		t.Errorf("CircuitKind(ollama) = %q, want %q", got, "ollama")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("ollama circuit error must satisfy errors.Is(ErrCircuitOpen)")
	}
}

// TestCircuitKindOpenAI verifies CircuitKind returns "openai" for an
// OpenAI embedder circuit error.
func TestCircuitKindOpenAI(t *testing.T) {
	err := newCircuitError(circuitKindOpenAI)
	if got := CircuitKind(err); got != "openai" {
		t.Errorf("CircuitKind(openai) = %q, want %q", got, "openai")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("openai circuit error must satisfy errors.Is(ErrCircuitOpen)")
	}
}

// TestCircuitKindCohere verifies CircuitKind returns "cohere" for a
// Cohere embedder circuit error.
func TestCircuitKindCohere(t *testing.T) {
	err := newCircuitError(circuitKindCohere)
	if got := CircuitKind(err); got != "cohere" {
		t.Errorf("CircuitKind(cohere) = %q, want %q", got, "cohere")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("cohere circuit error must satisfy errors.Is(ErrCircuitOpen)")
	}
}

// TestCircuitKindNonCircuitError verifies that CircuitKind returns "" for
// an error that is not a *circuitError, rather than panicking or returning
// a misleading kind.
func TestCircuitKindNonCircuitError(t *testing.T) {
	other := fmt.Errorf("other")
	if got := CircuitKind(other); got != "" {
		t.Errorf("CircuitKind(non-circuit error) = %q, want %q", got, "")
	}
	if errors.Is(other, ErrCircuitOpen) {
		t.Errorf("a plain error must not satisfy errors.Is(ErrCircuitOpen)")
	}
}

// TestCircuitKindNilError verifies that CircuitKind does not panic when
// handed a nil error and returns "".
func TestCircuitKindNilError(t *testing.T) {
	if got := CircuitKind(nil); got != "" {
		t.Errorf("CircuitKind(nil) = %q, want %q", got, "")
	}
}

// TestNewCircuitError_KindIsPreserved is a small guard ensuring the kind
// passed into newCircuitError round-trips through the *circuitError value,
// independent of the string comparison done by CircuitKind.
func TestNewCircuitError_KindIsPreserved(t *testing.T) {
	for kind, want := range map[embedderCircuitKind]string{
		circuitKindOllama: "ollama",
		circuitKindOpenAI: "openai",
		circuitKindCohere: "cohere",
	} {
		ce, ok := newCircuitError(kind).(*circuitError)
		if !ok {
			t.Fatalf("newCircuitError(%q) did not return *circuitError", kind)
		}
		if ce.kind != kind {
			t.Errorf("kind field = %q, want %q", ce.kind, kind)
		}
		if string(ce.kind) != want {
			t.Errorf("string(kind) = %q, want %q", string(ce.kind), want)
		}
	}
}
