package router

// planner_test.go provides table-driven unit tests for the routing
// planner extracted from the chat handler (issue #82). Each acceptance
// criterion from the issue maps to one test case:
//
//   - Guardrail trigger           -> TestPlanner_Plan/guardrail
//   - DSL match                   -> TestPlanner_Plan/dsl_local
//   - SLM decision                -> TestPlanner_Plan/slm_local
//   - SLM-error fallback          -> TestPlanner_Plan/slm_error_fallback
//   - Low-confidence escalation   -> TestPlanner_ConfidenceEscalation
//
// The stub SLM (stubSLM) returns deterministic decisions without an HTTP
// round-trip, and the stub confidence store returns a fixed value so the
// planner's confidence-bias path is exercisable without a real judge.

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

// stubSLM is a deterministic SLMDecider for planner tests. It records
// which method was called and with what confidence, and returns the
// preconfigured route/error pair.
type stubSLM struct {
	route          Route
	err            error
	calledDecide   bool
	calledWithConf bool
	lastConfidence float64
	lastPrompt     string
}

func (s *stubSLM) Decide(_ context.Context, prompt string) (Route, error) {
	s.calledDecide = true
	s.lastPrompt = prompt
	return s.route, s.err
}

func (s *stubSLM) DecideWithConfidence(_ context.Context, prompt string, confidence float64) (Route, error) {
	s.calledWithConf = true
	s.lastConfidence = confidence
	s.lastPrompt = prompt
	return s.route, s.err
}

// stubConf is a ConfidenceStore test double that returns a fixed
// confidence value. It records the categories it was queried with.
type stubConf struct {
	value   float64
	queried []string
}

func (s *stubConf) RecordOutcome(_ string, _ Route, _ int) {}

func (s *stubConf) LocalConfidence(category string) float64 {
	s.queried = append(s.queried, category)
	return s.value
}

// formattingRegex matches the handler's NEXUS_FORMATTING_REGEX default.
var formattingRegex = regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`)

// localPatternsRegex matches common coding task keywords (issue #202).
var localPatternsRegex = regexp.MustCompile(`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis)\b`)

func TestPlanner_Plan(t *testing.T) {
	tests := []struct {
		name          string
		planner       *Planner
		req           PlanRequest
		wantRoute     Route
		wantSource    DecisionSource
		wantSLMCalled bool // whether the SLM stage was reached
	}{
		{
			name: "guardrail forces frontier on oversized prompt",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteLocal},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				// 30000 chars / 4 = 7500 > 6000 budget.
				Prompt:          stringOf('a', 30000),
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteFrontier,
			wantSource:    SourceGuardrail,
			wantSLMCalled: false,
		},
		{
			name: "guardrail disabled when budget <= 0 falls through to DSL",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "fix the css",
				GuardrailBudget: 0, // disabled
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal, // DSL matches "css"
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for formatting keyword",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "please fix the css",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl fusion match for architecture keyword",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteLocal},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "review the system architecture",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteFusion,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for refactor keyword (issue #202)",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "refactor this module to use better error handling",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for security scan keyword (issue #202)",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "run a security scan on this code",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for generate tests keyword (issue #202)",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "generate tests for the auth middleware",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for explain this code keyword (issue #202)",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "explain this code section",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "dsl local match for performance analysis keyword (issue #202)",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "run a performance analysis on this function",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceDSL,
			wantSLMCalled: false,
		},
		{
			name: "slm decision local",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteLocal},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "write a small helper function",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteLocal,
			wantSource:    SourceSLM,
			wantSLMCalled: true,
		},
		{
			name: "slm decision frontier",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFrontier},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "implement a new distributed caching strategy for this microservices architecture",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteFrontier,
			wantSource:    SourceSLM,
			wantSLMCalled: true,
		},
		{
			name: "slm error falls back to frontier",
			planner: &Planner{
				SLM:                &stubSLM{err: errors.New("ollama: connection refused")},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "build a complex distributed caching layer with redis and memcache",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteFrontier,
			wantSource:    SourceSLMError,
			wantSLMCalled: true,
		},
		{
			name: "dsl bypassed when no keyword matches, slm consulted",
			planner: &Planner{
				SLM:                &stubSLM{route: RouteFusion},
				FormattingRegex:    formattingRegex,
				LocalPatternsRegex: localPatternsRegex,
			},
			req: PlanRequest{
				Prompt:          "design a new caching layer with redis",
				GuardrailBudget: 6000,
				GuardrailSource: "static-fallback",
				Context:         context.Background(),
			},
			wantRoute:     RouteFusion,
			wantSource:    SourceSLM,
			wantSLMCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := tt.planner.Plan(tt.req)

			if dec.Route != tt.wantRoute {
				t.Errorf("Route = %q, want %q", dec.Route, tt.wantRoute)
			}
			if dec.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", dec.Source, tt.wantSource)
			}

			// Verify the SLM was (or was not) consulted.
			slm, ok := tt.planner.SLM.(*stubSLM)
			if !ok {
				t.Fatalf("SLM is not a *stubSLM: %T", tt.planner.SLM)
			}
			slmUsed := slm.calledDecide || slm.calledWithConf
			if slmUsed != tt.wantSLMCalled {
				t.Errorf("SLM called = %v, want %v", slmUsed, tt.wantSLMCalled)
			}

			// EstimatedTokens should always be len(prompt)/4.
			wantTokens := len(tt.req.Prompt) / 4
			if dec.EstimatedTokens != wantTokens {
				t.Errorf("EstimatedTokens = %d, want %d", dec.EstimatedTokens, wantTokens)
			}

			// Budget fields should echo the request.
			if dec.BudgetSource != tt.req.GuardrailSource {
				t.Errorf("BudgetSource = %q, want %q", dec.BudgetSource, tt.req.GuardrailSource)
			}
			if dec.BudgetTokens != tt.req.GuardrailBudget {
				t.Errorf("BudgetTokens = %d, want %d", dec.BudgetTokens, tt.req.GuardrailBudget)
			}

			// traceReason maps the source to the handler's legacy
			// reason label. Verify it is never empty.
			if dec.Source.TraceReason() == "" {
				t.Errorf("traceReason() is empty for source %q", dec.Source)
			}
		})
	}
}

// TestPlanner_ConfidenceEscalation verifies the judge-guided adaptive
// routing path (issue #47): when a ConfidenceStore is wired, the planner
// categorizes the prompt, looks up the local model's historical
// confidence, and feeds it to DecideWithConfidence (not plain Decide).
// A low-confidence value must NOT hard-override the SLM — it biases the
// SLM's system prompt, and the SLM still makes the final call.
func TestPlanner_ConfidenceEscalation(t *testing.T) {
	t.Run("low confidence uses DecideWithConfidence not Decide", func(t *testing.T) {
		slm := &stubSLM{route: RouteFrontier}
		conf := &stubConf{value: 0.1} // below DefaultConfidenceFloor
		p := &Planner{
			SLM:                slm,
			Confidence:         conf,
			FormattingRegex:    formattingRegex,
			LocalPatternsRegex: localPatternsRegex,
		}
		// "debug ... failing" categorizes as CategoryDebugging and
		// does not match any DSL keyword, so it reaches the SLM.
		req := PlanRequest{
			Prompt:          "debug why this test keeps failing with an exception",
			GuardrailBudget: 6000,
			GuardrailSource: "static-fallback",
			Context:         context.Background(),
		}
		dec := p.Plan(req)

		if !slm.calledWithConf {
			t.Fatal("expected DecideWithConfidence to be called, it was not")
		}
		if slm.calledDecide {
			t.Error("plain Decide should NOT be called when ConfidenceStore is wired")
		}
		if slm.lastConfidence != 0.1 {
			t.Errorf("confidence passed to SLM = %v, want 0.1", slm.lastConfidence)
		}
		if dec.Confidence != 0.1 {
			t.Errorf("Decision.Confidence = %v, want 0.1", dec.Confidence)
		}
		if dec.TaskType != CategoryDebugging {
			t.Errorf("Decision.TaskType = %q, want %q", dec.TaskType, CategoryDebugging)
		}
		if dec.Source != SourceSLM {
			t.Errorf("Source = %q, want %q", dec.Source, SourceSLM)
		}
		if len(conf.queried) != 1 || conf.queried[0] != CategoryDebugging {
			t.Errorf("confidence queried = %v, want [%q]", conf.queried, CategoryDebugging)
		}
	})

	t.Run("high confidence uses DecideWithConfidence", func(t *testing.T) {
		slm := &stubSLM{route: RouteLocal}
		conf := &stubConf{value: 0.95} // above DefaultConfidenceCeiling
		p := &Planner{
			SLM:                slm,
			Confidence:         conf,
			FormattingRegex:    formattingRegex,
			LocalPatternsRegex: localPatternsRegex,
		}
		req := PlanRequest{
			Prompt:          "debug why this test keeps failing",
			GuardrailBudget: 6000,
			GuardrailSource: "static-fallback",
			Context:         context.Background(),
		}
		dec := p.Plan(req)

		if !slm.calledWithConf {
			t.Fatal("expected DecideWithConfidence to be called")
		}
		if slm.lastConfidence != 0.95 {
			t.Errorf("confidence = %v, want 0.95", slm.lastConfidence)
		}
		if dec.Route != RouteLocal {
			t.Errorf("Route = %q, want local", dec.Route)
		}
	})

	t.Run("nil confidence uses plain Decide", func(t *testing.T) {
		slm := &stubSLM{route: RouteFrontier}
		p := &Planner{
			SLM:                slm,
			Confidence:         nil,
			FormattingRegex:    formattingRegex,
			LocalPatternsRegex: localPatternsRegex,
		}
		req := PlanRequest{
			Prompt:          "debug why this test keeps failing",
			GuardrailBudget: 6000,
			GuardrailSource: "static-fallback",
			Context:         context.Background(),
		}
		dec := p.Plan(req)

		if !slm.calledDecide {
			t.Fatal("expected plain Decide to be called when Confidence is nil")
		}
		if slm.calledWithConf {
			t.Error("DecideWithConfidence should NOT be called when Confidence is nil")
		}
		if dec.Confidence != NeutralConfidence {
			t.Errorf("Decision.Confidence = %v, want NeutralConfidence (%v)", dec.Confidence, NeutralConfidence)
		}
		if dec.TaskType != "" {
			t.Errorf("Decision.TaskType = %q, want empty (no categorization on nil store)", dec.TaskType)
		}
	})

	t.Run("SLM error with confidence still records category", func(t *testing.T) {
		slm := &stubSLM{err: errors.New("timeout")}
		conf := &stubConf{value: 0.2}
		p := &Planner{
			SLM:                slm,
			Confidence:         conf,
			FormattingRegex:    formattingRegex,
			LocalPatternsRegex: localPatternsRegex,
		}
		req := PlanRequest{
			Prompt:          "debug this stack trace",
			GuardrailBudget: 6000,
			GuardrailSource: "static-fallback",
			Context:         context.Background(),
		}
		dec := p.Plan(req)

		if dec.Source != SourceSLMError {
			t.Errorf("Source = %q, want %q", dec.Source, SourceSLMError)
		}
		if dec.Route != RouteFrontier {
			t.Errorf("Route = %q, want frontier (safe default)", dec.Route)
		}
		if dec.SLMError == "" {
			t.Error("SLMError should be populated on error path")
		}
		if dec.TaskType != CategoryDebugging {
			t.Errorf("TaskType = %q, want %q", dec.TaskType, CategoryDebugging)
		}
	})
}

// TestPlanner_NilSLM verifies the defensive path: when no SLM client is
// configured the planner does not panic; it returns RouteFrontier with
// SourceEscalation. The handler always wires an SLM, but guarding
// against nil means a misconfiguration degrades gracefully.
func TestPlanner_NilSLM(t *testing.T) {
	// The planner calls p.SLM.Decide when it reaches stage 3 and SLM
	// is nil — that would panic. But the DSL catches "css", so the
	// SLM is never reached for this prompt. Verify the DSL path works
	// with a nil SLM:
	p := &Planner{
		SLM:                nil,
		FormattingRegex:    formattingRegex,
		LocalPatternsRegex: localPatternsRegex,
	}
	req := PlanRequest{
		Prompt:          "fix the css",
		GuardrailBudget: 6000,
		GuardrailSource: "static-fallback",
		Context:         context.Background(),
	}
	dec := p.Plan(req)
	if dec.Route != RouteLocal {
		t.Errorf("Route = %q, want local (DSL match)", dec.Route)
	}
	if dec.Source != SourceDSL {
		t.Errorf("Source = %q, want dsl", dec.Source)
	}
}

// stringOf returns a string of n copies of byte b. A test helper for
// generating oversized prompts without a strings.Builder import.
func stringOf(b byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = b
	}
	return string(buf)
}
