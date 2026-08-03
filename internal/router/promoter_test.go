package router

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"write a unit test", []string{"write", "a", "unit", "test"}},
		{"Fix the CSS layout.", []string{"fix", "the", "css", "layout"}},
		{"parse JSON data", []string{"parse", "json", "data"}},
		{"  ", []string{}},
		{"word_with_underscores and-hyphens", []string{"word_with_underscores", "and-hyphens"}},
	}
	for _, tc := range cases {
		got := tokenize(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestNgramToRegex(t *testing.T) {
	got := ngramToRegex("write test")
	if got != `(?i)\bwrite test\b` {
		t.Errorf("ngramToRegex = %q, want %q", got, `(?i)\bwrite test\b`)
	}
	re := regexp.MustCompile(got)
	if !re.MatchString("Please write test for this") {
		t.Error("regex should match")
	}
	if re.MatchString("writetest") {
		t.Error("regex should not match without word boundary")
	}
}

func TestExtractPromotableNGrams(t *testing.T) {
	decisions := []slmDecisionRecord{
		{"write a unit test for auth", RouteLocal, time.Now()},
		{"write a unit test for login", RouteLocal, time.Now()},
		{"write a unit test for payment", RouteLocal, time.Now()},
		{"write a unit test for profile", RouteLocal, time.Now()},
		{"write a unit test for settings", RouteLocal, time.Now()},
		{"design the database schema", RouteFusion, time.Now()},
		{"design the database schema for users", RouteFusion, time.Now()},
		{"design the database schema for orders", RouteFusion, time.Now()},
		{"design the database schema for logs", RouteFusion, time.Now()},
		{"design the database schema for audit", RouteFusion, time.Now()},
		{"random prompt about nothing", RouteFrontier, time.Now()},
		{"another random prompt", RouteFrontier, time.Now()},
	}

	// minSamples=5, confidence=0.9
	patterns := extractPromotableNGrams(decisions, 5, 0.9)
	if len(patterns) == 0 {
		t.Fatal("expected at least one promoted pattern")
	}

	// "unit test" bigram should appear 5 times all routing to local.
	foundUnitTest := false
	for _, p := range patterns {
		if p.Route == RouteLocal && regexp.MustCompile(p.Pattern).MatchString("write unit test") {
			foundUnitTest = true
			if p.Samples < 5 {
				t.Errorf("expected samples >= 5, got %d", p.Samples)
			}
			if p.Confidence < 0.9 {
				t.Errorf("expected confidence >= 0.9, got %f", p.Confidence)
			}
		}
	}
	if !foundUnitTest {
		t.Error("expected a promoted pattern matching 'unit test' → local")
	}
}

func TestExtractPromotableNGrams_ThresholdGating(t *testing.T) {
	// Only 2 decisions — below the minSamples threshold of 5.
	decisions := []slmDecisionRecord{
		{"write test foo", RouteLocal, time.Now()},
		{"write test bar", RouteLocal, time.Now()},
	}
	patterns := extractPromotableNGrams(decisions, 5, 0.9)
	if len(patterns) != 0 {
		t.Errorf("expected 0 patterns below minSamples, got %d", len(patterns))
	}
}

func TestExtractPromotableNGrams_LowConfidence(t *testing.T) {
	// 6 decisions, but split 50/50 between routes → confidence < 0.9.
	decisions := make([]slmDecisionRecord, 0, 6)
	for i := 0; i < 3; i++ {
		decisions = append(decisions, slmDecisionRecord{"write test foo", RouteLocal, time.Now()})
	}
	for i := 0; i < 3; i++ {
		decisions = append(decisions, slmDecisionRecord{"write test bar", RouteFrontier, time.Now()})
	}
	patterns := extractPromotableNGrams(decisions, 5, 0.9)
	if len(patterns) != 0 {
		t.Errorf("expected 0 patterns with low confidence, got %d", len(patterns))
	}
}

func TestExtractPromotableNGrams_DisabledWhenZero(t *testing.T) {
	decisions := []slmDecisionRecord{
		{"write test", RouteLocal, time.Now()},
	}
	if p := extractPromotableNGrams(decisions, 0, 0.9); len(p) != 0 {
		t.Error("minSamples=0 should disable")
	}
	if p := extractPromotableNGrams(decisions, 5, 0); len(p) != 0 {
		t.Error("confidence=0 should disable")
	}
}

func TestPatternPromoter_RecordAndMatch(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 3,
		Confidence: 0.90,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	// Record 3 identical local decisions.
	for i := 0; i < 3; i++ {
		p.RecordDecision("write unit test for module", RouteLocal)
	}

	// Recompute.
	p.Recompute()

	patterns := p.PromotedPatterns()
	if len(patterns) == 0 {
		t.Fatal("expected promoted patterns after recompute")
	}

	// Should match.
	route, _, hit := p.Match("please write unit test for auth")
	if !hit {
		t.Fatal("expected match for promoted pattern")
	}
	if route != RouteLocal {
		t.Errorf("expected route local, got %s", route)
	}
}

func TestPatternPromoter_NoMatch(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 3,
		Confidence: 0.90,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	_, _, hit := p.Match("unrelated prompt")
	if hit {
		t.Error("expected no match on empty promoter")
	}
}

func TestPatternPromoter_PromotedTotal(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 2,
		Confidence: 0.90,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	p.RecordDecision("write test one", RouteLocal)
	p.RecordDecision("write test two", RouteLocal)
	p.Recompute()

	if p.PromotedTotal() != 0 {
		t.Errorf("expected 0 promoted total, got %d", p.PromotedTotal())
	}

	p.IncPromotedTotal()
	p.IncPromotedTotal()

	if p.PromotedTotal() != 2 {
		t.Errorf("expected 2 promoted total, got %d", p.PromotedTotal())
	}
}

func TestPatternPromoter_ShouldRecompute(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 1,
		Confidence: 0.5,
		Interval:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	if !p.ShouldRecompute() {
		t.Error("expected should recompute initially")
	}

	p.Recompute()

	if p.ShouldRecompute() {
		t.Error("expected should not recompute immediately after recompute")
	}

	time.Sleep(60 * time.Millisecond)

	if !p.ShouldRecompute() {
		t.Error("expected should recompute after interval")
	}
}

func TestPatternPromoter_Persistence(t *testing.T) {
	dbPath := t.TempDir() + "/test_promoter.db"

	// Create promoter, record decisions, recompute, close.
	p1, err := NewPatternPromoter(PromoterConfig{
		Path:       dbPath,
		MinSamples: 3,
		Confidence: 0.90,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter (1): %v", err)
	}

	for i := 0; i < 3; i++ {
		p1.RecordDecision("write unit test for module", RouteLocal)
	}
	p1.Recompute()

	patterns1 := p1.PromotedPatterns()
	if len(patterns1) == 0 {
		p1.Close()
		t.Fatal("expected promoted patterns before close")
	}
	p1.Close()

	// Reopen — should load persisted patterns.
	p2, err := NewPatternPromoter(PromoterConfig{
		Path:       dbPath,
		MinSamples: 3,
		Confidence: 0.90,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter (2): %v", err)
	}
	defer p2.Close()

	patterns2 := p2.PromotedPatterns()
	if len(patterns2) == 0 {
		t.Fatal("expected promoted patterns loaded from SQLite")
	}

	// The loaded patterns should match the original.
	route, _, hit := p2.Match("please write unit test for auth")
	if !hit || route != RouteLocal {
		t.Errorf("expected match from persisted pattern: hit=%v route=%s", hit, route)
	}
}

func TestPatternPromoter_NilSafe(t *testing.T) {
	var p *PatternPromoter

	p.RecordDecision("prompt", RouteLocal) // should not panic

	_, _, hit := p.Match("prompt")
	if hit {
		t.Error("nil promoter should not match")
	}

	if p.PromotedTotal() != 0 {
		t.Error("nil promoter should report 0")
	}

	p.IncPromotedTotal() // should not panic

	if p.ShouldRecompute() {
		t.Error("nil promoter should not recompute")
	}

	p.Recompute() // should not panic

	if err := p.Close(); err != nil {
		t.Errorf("nil promoter close: %v", err)
	}

	if patterns := p.PromotedPatterns(); patterns != nil {
		t.Error("nil promoter should return nil patterns")
	}
}

func TestPatternPromoter_MaxHistoryBounded(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 1,
		Confidence: 0.5,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	// Record more than maxDecisionHistory to verify the ring buffer trims.
	for i := 0; i < maxDecisionHistory+100; i++ {
		p.RecordDecision("some prompt", RouteLocal)
	}

	p.mu.RLock()
	count := len(p.decisions)
	p.mu.RUnlock()

	if count > maxDecisionHistory {
		t.Errorf("decisions not bounded: %d > %d", count, maxDecisionHistory)
	}
}

func TestPatternPromoter_ConcurrentAccess(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 5,
		Confidence: 0.80,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	var wg sync.WaitGroup
	// Concurrent writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.RecordDecision("write unit test", RouteLocal)
			}
		}()
	}
	// Concurrent readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.Match("write unit test")
				p.PromotedPatterns()
				p.PromotedTotal()
			}
		}()
	}
	wg.Wait()

	// Recompute should produce a pattern.
	p.Recompute()
	patterns := p.PromotedPatterns()
	if len(patterns) == 0 {
		t.Fatal("expected promoted patterns after concurrent writes")
	}
}

func TestPatternPromoter_EmptyPrompt(t *testing.T) {
	p, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 1,
		Confidence: 0.5,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer p.Close()

	p.RecordDecision("", RouteLocal)
	p.Recompute()

	if len(p.PromotedPatterns()) != 0 {
		t.Error("empty prompt should not produce promoted patterns")
	}
}

// TestPlanner_PromoterIntegration verifies the planner checks promoted
// patterns before the manual DSL fast-pass.
func TestPlanner_PromoterIntegration(t *testing.T) {
	promoter, err := NewPatternPromoter(PromoterConfig{
		Path:       ":memory:",
		MinSamples: 1,
		Confidence: 0.5,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPatternPromoter: %v", err)
	}
	defer promoter.Close()

	// Manually inject a promoted pattern.
	promoter.mu.Lock()
	re := regexp.MustCompile(`(?i)\bcustom promoted keyword\b`)
	promoter.promoted = []PromotedPattern{
		{Pattern: `(?i)\bcustom promoted keyword\b`, Route: RouteLocal, Samples: 100, Confidence: 0.99, re: re},
	}
	promoter.compiled[`(?i)\bcustom promoted keyword\b`] = re
	promoter.mu.Unlock()

	stub := &stubSLM{route: RouteFrontier}
	planner := &Planner{
		SLM:      stub,
		Promoter: promoter,
	}

	dec := planner.Plan(PlanRequest{Prompt: "this has custom promoted keyword in it"})
	if dec.Source != SourceDSLPromoted {
		t.Errorf("expected SourceDSLPromoted, got %s", dec.Source)
	}
	if dec.Route != RouteLocal {
		t.Errorf("expected RouteLocal, got %s", dec.Route)
	}
	if dec.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestPlanner_NilPromoter ensures nil promoter is backward compatible.
func TestPlanner_NilPromoter(t *testing.T) {
	stub := &stubSLM{route: RouteFrontier}
	planner := &Planner{
		SLM: stub,
		// Promoter is nil
	}

	dec := planner.Plan(PlanRequest{Prompt: "some prompt that needs slm"})
	if dec.Source != SourceSLM {
		t.Errorf("expected SourceSLM, got %s", dec.Source)
	}
}

// TestSourceDSLPromoted_TraceReason verifies the trace reason.
func TestSourceDSLPromoted_TraceReason(t *testing.T) {
	if got := SourceDSLPromoted.TraceReason(); got != "dsl-promoted" {
		t.Errorf("expected 'dsl-promoted', got %q", got)
	}
}

// TestPromoterConfig_Enabled verifies the disable logic.
func TestPromoterConfig_Enabled(t *testing.T) {
	c := PromoterConfig{MinSamples: 0, Confidence: 0, Interval: 0}
	if c.Enabled() {
		t.Error("all-zero config should not be enabled")
	}
	c = PromoterConfig{MinSamples: 20, Confidence: 0.9, Interval: time.Hour}
	if !c.Enabled() {
		t.Error("non-zero config should be enabled")
	}
}
