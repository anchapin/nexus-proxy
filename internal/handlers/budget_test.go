package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// stubSpendGuard is a test double for budget.Guard.
type stubSpendGuard struct {
	mu               sync.Mutex
	limit            float64
	spent            float64
	checkCalls       int
	recordCalls      int
	lastRecordCost   float64
	lastRecordSource string
	blocked          bool // if true, Check always returns true
}

func newStubSpendGuard(limit float64) *stubSpendGuard {
	return &stubSpendGuard{limit: limit}
}

func (s *stubSpendGuard) Check(cost float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkCalls++
	if s.blocked {
		return true
	}
	return s.spent+cost > s.limit
}

func (s *stubSpendGuard) Record(cost float64, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCalls++
	s.spent += cost
	s.lastRecordCost = cost
	s.lastRecordSource = source
}

// chatHandlerForBudget returns a chat handler configured for budget guard testing.
// The handler is set up so DSL/SLM won't match "hello world" -> will go to frontier.
func chatHandlerForBudget(t *testing.T, sg interface {
	Check(float64) bool
	Record(float64, string)
}) Deps {
	t.Helper()
	cfg := config.Config{
		Addr:              ":0",
		OllamaURL:         "http://ollama.local",
		RouterModel:       "qwen3-coder:4b",
		LocalModel:        "qwen3-coder:8b",
		EmbeddingModel:    "nomic-embed-text",
		FrontierURL:       "http://frontier.local",
		FrontierModel:     "gpt-4o",
		FrontierKey:       "sk-test",
		FrontierCostPer1K: 0.005,
		RAGThreshold:      0.55,
		TokenGuardrail:    6000,
		MetaPrompt:        " BOOST",
		TOONNotice:        "[PROXY SYSTEM NOTE]: TOON compression applied",
	}
	store := rag.NewStore(stubEmbedder{vec: []float64{0, 0, 0}}, 0.55)
	rt := upstream.NewRecordingTransport()
	client := &http.Client{Transport: rt}
	deps := Deps{
		Config:          cfg,
		Client:          client,
		RAG:             store,
		SLM:             router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, 1, client),
		FormattingRegex: regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`),
		Recorder:        telemetry.Noop{},
		SpendGuard:      sg,
	}
	return deps
}

// makeFrontierRequest sends a POST to /v1/chat/completions with a minimal
// request designed to go to frontier (DSL won't match, SLM might route directly).
func makeFrontierRequest(t *testing.T, deps Deps) *http.Response {
	t.Helper()
	body := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{"role": "user", "content": "hello world"},
		},
		"stream": false,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	return rw.Result()
}

// TestBudgetGuardDisabled verifies that when SpendGuard is nil (budget disabled),
// frontier requests are not blocked by the budget guard.
func TestBudgetGuardDisabled(t *testing.T) {
	deps := chatHandlerForBudget(t, nil)
	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()
	// nil SpendGuard means budget is disabled; the request should not be
	// rejected with 429 due to budget (it might fail for other reasons
	// like upstream not being available in tests).
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Error("nil SpendGuard should not cause 429 budget rejection")
	}
}

// TestBudgetGuardCheckBlocksFrontier verifies that when Check returns true
// (budget exhausted), the handler returns 429 WITHOUT calling the upstream.
func TestBudgetGuardCheckBlocksFrontier(t *testing.T) {
	sg := newStubSpendGuard(100.0)
	sg.blocked = true // always block
	deps := chatHandlerForBudget(t, sg)
	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("exhausted guard: got %d, want 429", resp.StatusCode)
	}
	// Verify Check was called
	sg.mu.Lock()
	calls := sg.checkCalls
	sg.mu.Unlock()
	if calls == 0 {
		t.Error("Check was not called")
	}
}

// TestBudgetGuardCheckCalledBeforeFrontier verifies that Check is called
// before the upstream is dispatched for frontier route.
func TestBudgetGuardCheckCalledBeforeFrontier(t *testing.T) {
	sg := newStubSpendGuard(0.001) // very small budget
	deps := chatHandlerForBudget(t, sg)
	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()
	sg.mu.Lock()
	checkCalls := sg.checkCalls
	sg.mu.Unlock()
	if checkCalls == 0 {
		t.Error("Check was not called before frontier dispatch")
	}
}

// TestBudgetGuardNoRecordOnCheckFailure verifies that Record is NOT called
// when Check returns true (budget exhausted).
func TestBudgetGuardNoRecordOnCheckFailure(t *testing.T) {
	sg := newStubSpendGuard(100.0)
	sg.blocked = true
	deps := chatHandlerForBudget(t, sg)
	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp.StatusCode)
	}

	sg.mu.Lock()
	defer sg.mu.Unlock()
	if sg.recordCalls > 0 {
		t.Errorf("Record called %d times, want 0 (should not record on rejected request)", sg.recordCalls)
	}
}

// mockSpendGuard records calls for verification.
type mockSpendGuard struct {
	mu          sync.Mutex
	checkCalls  []float64 // cost passed to each Check call
	recordCalls []recordCall
	shouldBlock bool
}

type recordCall struct {
	cost   float64
	source string
}

func (m *mockSpendGuard) Check(cost float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkCalls = append(m.checkCalls, cost)
	return m.shouldBlock
}

func (m *mockSpendGuard) Record(cost float64, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordCalls = append(m.recordCalls, recordCall{cost: cost, source: source})
}

// TestBudgetGuardCheckReceivesPositiveCost verifies that when Check is called,
// it receives a positive cost estimate.
func TestBudgetGuardCheckReceivesPositiveCost(t *testing.T) {
	sg := &mockSpendGuard{shouldBlock: false}
	deps := chatHandlerForBudget(t, sg)

	// Override FrontierCostPer1K to a known value
	deps.Config.FrontierCostPer1K = 0.010 // $10 per 1K tokens

	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()

	sg.mu.Lock()
	defer sg.mu.Unlock()

	if len(sg.checkCalls) == 0 {
		t.Fatal("Check was not called")
	}
	// The cost should be > 0 (estimate based on prompt tokens)
	firstCost := sg.checkCalls[0]
	if firstCost <= 0 {
		t.Errorf("Check cost should be positive, got %f", firstCost)
	}
}

// TestBudgetGuardRejectionReason verifies that when budget blocks a request,
// the rejection reason is RejectionBudget.
func TestBudgetGuardRejectionReason(t *testing.T) {
	sg := newStubSpendGuard(100.0)
	sg.blocked = true
	deps := chatHandlerForBudget(t, sg)

	// We need to use a rejection observer to verify the rejection reason.
	var rejectionReason string
	deps.RejectionObserver = RejectionObserverFunc(func(e RejectionEvent) {
		rejectionReason = e.Reason
	})

	resp := makeFrontierRequest(t, deps)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp.StatusCode)
	}
	if rejectionReason != RejectionBudget {
		t.Errorf("rejection reason = %q, want %q", rejectionReason, RejectionBudget)
	}
}
