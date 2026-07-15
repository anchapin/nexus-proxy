package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/router"
)

// stubConfidenceStore is a ConfidenceStore test double: LocalConfidence
// returns a fixed value and records the categories it was queried with so
// the handler wiring can be asserted.
type stubConfidenceStore struct {
	mu         sync.Mutex
	confidence float64
	queried    []string
}

func (s *stubConfidenceStore) RecordOutcome(_ string, _ router.Route, _ int) {}

func (s *stubConfidenceStore) LocalConfidence(category string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queried = append(s.queried, category)
	return s.confidence
}

func (s *stubConfidenceStore) queriedCategories() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.queried))
	copy(out, s.queried)
	return out
}

// TestChatConfidenceLowBiasesSLMPromptToFrontier is the handler-level
// integration of issue #47: with a confidence store reporting a low
// score for the debugging category, the SLM must receive the
// negative-bias system prompt augmentation.
func TestChatConfidenceLowBiasesSLMPromptToFrontier(t *testing.T) {
	deps, rt := baseDeps(t)
	store := &stubConfidenceStore{confidence: 0.1}
	deps.Confidence = store

	var slmBody string
	rt.On("POST", "http://ollama.local/api/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		slmBody = string(b)
		_, _ = io.WriteString(w, `{"message":{"content":"{\"route\":\"frontier\"}"}}`)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	// "analyze this exception" is not a DSL/guardrail hit, so it reaches the SLM.
	body := `{"messages":[{"role":"user","content":"analyze this exception that keeps happening"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if !strings.Contains(slmBody, "ADAPTIVE ROUTING CONTEXT") || !strings.Contains(slmBody, "POORLY") {
		t.Errorf("SLM request missing negative-bias augmentation: %s", slmBody)
	}
	cats := store.queriedCategories()
	if len(cats) != 1 || cats[0] != router.CategoryDebugging {
		t.Errorf("confidence queried categories = %v, want [debugging]", cats)
	}
}

// TestChatNilConfidenceUsesNeutralPrompt confirms the byte-for-byte
// identical path when no confidence store is wired: the SLM request must
// NOT contain any adaptive-routing augmentation.
func TestChatNilConfidenceUsesNeutralPrompt(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Confidence = nil

	var slmBody string
	rt.On("POST", "http://ollama.local/api/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		slmBody = string(b)
		_, _ = io.WriteString(w, `{"message":{"content":"{\"route\":\"frontier\"}"}}`)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	body := `{"messages":[{"role":"user","content":"debug why this test keeps failing with an exception"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if strings.Contains(slmBody, "ADAPTIVE ROUTING CONTEXT") {
		t.Errorf("nil confidence must not augment SLM prompt: %s", slmBody)
	}
}

// TestChatConfidenceNeutralDoesNotAugment confirms that a confidence store
// returning the neutral value produces no augmentation — the insufficient
// -samples path is routing-identical to today.
func TestChatConfidenceNeutralDoesNotAugment(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.Confidence = &stubConfidenceStore{confidence: router.NeutralConfidence}

	var slmBody string
	rt.On("POST", "http://ollama.local/api/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		slmBody = string(b)
		_, _ = io.WriteString(w, `{"message":{"content":"{\"route\":\"frontier\"}"}}`)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})

	body := `{"messages":[{"role":"user","content":"debug why this test keeps failing with an exception"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if strings.Contains(slmBody, "ADAPTIVE ROUTING CONTEXT") {
		t.Errorf("neutral confidence must not augment SLM prompt: %s", slmBody)
	}
}
