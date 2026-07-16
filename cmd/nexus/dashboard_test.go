package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunDashboardHelp verifies the -h flag prints usage and returns 0.
func TestRunDashboardHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDashboard([]string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runDashboard(-h) = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "nexus dashboard") {
		t.Errorf("stderr does not contain 'nexus dashboard' usage")
	}
}

// TestRunDashboardInvalidFlag verifies bad flags return 1.
func TestRunDashboardInvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDashboard([]string{"--not-a-flag"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runDashboard(--not-a-flag) = %d, want 1", code)
	}
}

// TestResolveCostPer1kFlag validates the flag parsing path.
func TestResolveCostPer1kFlag(t *testing.T) {
	v, err := resolveCostPer1k("0.001")
	if err != nil {
		t.Fatalf("resolveCostPer1k(0.001) err = %v", err)
	}
	if v != 0.001 {
		t.Errorf("resolveCostPer1k(0.001) = %v, want 0.001", v)
	}
}

// TestResolveCostPer1kInvalid validates invalid rate is rejected.
func TestResolveCostPer1kInvalid(t *testing.T) {
	_, err := resolveCostPer1k("not-a-number")
	if err == nil {
		t.Error("resolveCostPer1k(not-a-number) err = nil, want error")
	}
	_, err = resolveCostPer1k("-1")
	if err == nil {
		t.Error("resolveCostPer1k(-1) err = nil, want error")
	}
}

// TestResolveRange verifies date range parsing.
func TestResolveRange(t *testing.T) {
	start, end, err := resolveRange("2026-07-10", 3)
	if err != nil {
		t.Fatalf("resolveRange(2026-07-10, 3) err = %v", err)
	}
	if start.Day() != 10 || end.Day() != 12 {
		t.Errorf("range = %v..%v, want 10..12", start.Day(), end.Day())
	}
}

// TestResolveRangeBadDate verifies bad date format is rejected.
func TestResolveRangeBadDate(t *testing.T) {
	_, _, err := resolveRange("not-a-date", 0)
	if err == nil {
		t.Error("resolveRange(not-a-date, 0) err = nil, want error")
	}
}
