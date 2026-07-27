package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/router"
)

func TestRunJudgeStatsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runJudgeStats([]string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runJudgeStats(-h) = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "nexus judge stats") {
		t.Errorf("stderr does not contain 'nexus judge stats' usage")
	}
}

func TestRunJudgeStatsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runJudgeStats([]string{"--not-a-flag"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runJudgeStats(--not-a-flag) = %d, want 1", code)
	}
}

func TestRunJudgeStatsNoDB(t *testing.T) {
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_DB",
	} {
		t.Setenv(k, "")
	}
	var stdout, stderr bytes.Buffer
	code := runJudgeStats(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runJudgeStats(no DB) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "NEXUS_ROUTING_CONFIDENCE_DB is not set") {
		t.Errorf("expected DB-not-set error; got: %s", stderr.String())
	}
}

func TestRunJudgeStatsDBOpenError(t *testing.T) {
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "/nonexistent/path/to/db.sqlite")
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_FLOOR",
		"NEXUS_ROUTING_CONFIDENCE_CEILING",
		"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES",
		"NEXUS_ROUTING_CONFIDENCE_WINDOW",
		"NEXUS_JUDGE_SAMPLE_RATE",
		"NEXUS_METRICS_DB",
		"NEXUS_RAG_DB",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_RAG_DB", "")

	var stdout, stderr bytes.Buffer
	code := runJudgeStats(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runJudgeStats(missing DB) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "open confidence store") {
		t.Errorf("expected open error; got: %s", stderr.String())
	}
}

func TestRunJudgeStatsEmptyStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "confidence.db")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", dbPath)
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_FLOOR",
		"NEXUS_ROUTING_CONFIDENCE_CEILING",
		"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES",
		"NEXUS_ROUTING_CONFIDENCE_WINDOW",
		"NEXUS_JUDGE_SAMPLE_RATE",
		"NEXUS_METRICS_DB",
		"NEXUS_RAG_DB",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_RAG_DB", "")

	var stdout, stderr bytes.Buffer
	code := runJudgeStats(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runJudgeStats(empty store) = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "CATEGORY") {
		t.Errorf("missing CATEGORY header; got:\n%s", out)
	}
	if !strings.Contains(out, "CONFIDENCE") {
		t.Errorf("missing CONFIDENCE header; got:\n%s", out)
	}
	if !strings.Contains(out, "SAMPLES") {
		t.Errorf("missing SAMPLES header; got:\n%s", out)
	}
	// Empty categories should show 0 samples. tabwriter pads columns with spaces.
	if !strings.Contains(out, "—  0  ") {
		t.Errorf("expected 0 samples for empty category; got:\n%s", out)
	}
	// Global header should print floor/ceiling/min-samples/window.
	if !strings.Contains(out, "Floor:") {
		t.Errorf("missing Floor in header; got:\n%s", out)
	}
}

func TestRunJudgeStatsPopulatedStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "confidence.db")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", dbPath)
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_FLOOR",
		"NEXUS_ROUTING_CONFIDENCE_CEILING",
		"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES",
		"NEXUS_ROUTING_CONFIDENCE_WINDOW",
		"NEXUS_JUDGE_SAMPLE_RATE",
		"NEXUS_METRICS_DB",
		"NEXUS_RAG_DB",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_RAG_DB", "")

	store, err := router.OpenConfidenceStore(router.ConfidenceConfig{
		Path:       dbPath,
		MinSamples: 2,
		Window:     168 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open confidence store: %v", err)
	}
	defer store.Close()

	// Seed some outcomes for "css" and "refactoring".
	for i := 0; i < 3; i++ {
		if err := store.RecordOutcome("css", router.RouteLocal, 4); err != nil {
			t.Fatalf("record css outcome: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := store.RecordOutcome("refactoring", router.RouteLocal, 2); err != nil {
			t.Fatalf("record refactoring outcome: %v", err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := runJudgeStats(nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runJudgeStats(populated) = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// css should show confidence (3 samples >= minSamples of 2)
	if !strings.Contains(out, "css") {
		t.Errorf("missing css row; got:\n%s", out)
	}
	// refactoring has 2 samples which equals minSamples, so it should show confidence
	if !strings.Contains(out, "refactoring") {
		t.Errorf("missing refactoring row; got:\n%s", out)
	}
	// debugging has 0 samples — should still appear
	if !strings.Contains(out, "debugging") {
		t.Errorf("missing debugging row (empty category); got:\n%s", out)
	}
}

func TestRunJudgeStatsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "confidence.db")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", dbPath)
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_FLOOR",
		"NEXUS_ROUTING_CONFIDENCE_CEILING",
		"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES",
		"NEXUS_ROUTING_CONFIDENCE_WINDOW",
		"NEXUS_JUDGE_SAMPLE_RATE",
		"NEXUS_METRICS_DB",
		"NEXUS_RAG_DB",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_RAG_DB", "")

	var stdout, stderr bytes.Buffer
	code := runJudgeStats([]string{"--json"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runJudgeStats(--json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("JSON parse error: %v\n%s", err, stdout.String())
	}
	if out["floor"] == nil {
		t.Error("JSON output missing 'floor' field")
	}
	if out["ceiling"] == nil {
		t.Error("JSON output missing 'ceiling' field")
	}
	if out["min_samples"] == nil {
		t.Error("JSON output missing 'min_samples' field")
	}
	if out["window"] == nil {
		t.Error("JSON output missing 'window' field")
	}
	cats, ok := out["categories"].([]interface{})
	if !ok {
		t.Fatalf("categories is not a []interface{}: %T", out["categories"])
	}
	if len(cats) == 0 {
		t.Error("categories is empty")
	}
}
