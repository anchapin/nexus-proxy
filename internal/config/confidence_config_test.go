package config

import (
	"testing"
	"time"
)

func TestLoadRoutingConfidenceDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingConfidenceFloor != 0.4 {
		t.Errorf("RoutingConfidenceFloor = %v, want 0.4", cfg.RoutingConfidenceFloor)
	}
	if cfg.RoutingConfidenceCeiling != 0.85 {
		t.Errorf("RoutingConfidenceCeiling = %v, want 0.85", cfg.RoutingConfidenceCeiling)
	}
	if cfg.RoutingConfidenceMinSamples != 5 {
		t.Errorf("RoutingConfidenceMinSamples = %d, want 5", cfg.RoutingConfidenceMinSamples)
	}
	if cfg.RoutingConfidenceWindow != 168*time.Hour {
		t.Errorf("RoutingConfidenceWindow = %v, want 168h", cfg.RoutingConfidenceWindow)
	}
}

func TestLoadRoutingConfidenceOverrides(t *testing.T) {
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "/tmp/rc.db")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_FLOOR", "0.3")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_CEILING", "0.9")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", "10")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_WINDOW", "24h")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingConfidenceDB != "/tmp/rc.db" {
		t.Errorf("RoutingConfidenceDB = %q", cfg.RoutingConfidenceDB)
	}
	if cfg.RoutingConfidenceFloor != 0.3 {
		t.Errorf("RoutingConfidenceFloor = %v, want 0.3", cfg.RoutingConfidenceFloor)
	}
	if cfg.RoutingConfidenceCeiling != 0.9 {
		t.Errorf("RoutingConfidenceCeiling = %v, want 0.9", cfg.RoutingConfidenceCeiling)
	}
	if cfg.RoutingConfidenceMinSamples != 10 {
		t.Errorf("RoutingConfidenceMinSamples = %d, want 10", cfg.RoutingConfidenceMinSamples)
	}
	if cfg.RoutingConfidenceWindow != 24*time.Hour {
		t.Errorf("RoutingConfidenceWindow = %v, want 24h", cfg.RoutingConfidenceWindow)
	}
}

func TestRoutingConfidenceEnabledRequiresJudge(t *testing.T) {
	// Judge enabled + DB path set => enabled.
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0.1")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "/tmp/rc.db")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.JudgeEnabled {
		t.Fatal("precondition: JudgeEnabled should be true")
	}
	if !cfg.RoutingConfidenceEnabled() {
		t.Error("RoutingConfidenceEnabled = false, want true (judge on + db set)")
	}
}

func TestRoutingConfidenceDisabledWhenJudgeOff(t *testing.T) {
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "/tmp/rc.db")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingConfidenceEnabled() {
		t.Error("RoutingConfidenceEnabled = true, want false when judge disabled")
	}
}

func TestRoutingConfidenceDisabledWhenDBEmpty(t *testing.T) {
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0.1")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingConfidenceEnabled() {
		t.Error("RoutingConfidenceEnabled = true, want false when DB path empty")
	}
}

func TestLoadRoutingConfidenceInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad floor", "NEXUS_ROUTING_CONFIDENCE_FLOOR", "low"},
		{"bad ceiling", "NEXUS_ROUTING_CONFIDENCE_CEILING", "high"},
		{"bad min samples", "NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", "five"},
		{"bad window", "NEXUS_ROUTING_CONFIDENCE_WINDOW", "a week"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}
