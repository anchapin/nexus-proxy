package config

import (
	"os"
	"strings"
	"testing"
)

func TestDeprecationsContains924Rename(t *testing.T) {
	var found *Deprecation
	for i := range Deprecations {
		if Deprecations[i].OldEnv == "NEXUS_QUALITY_DROPED_RING_SIZE" {
			found = &Deprecations[i]
			break
		}
	}
	if found == nil {
		t.Fatal("Deprecations must contain the #924 rename (NEXUS_QUALITY_DROPED_RING_SIZE)")
	}
	if found.NewEnv != "NEXUS_QUALITY_DROPPED_RING_SIZE" {
		t.Errorf("NewEnv = %q, want NEXUS_QUALITY_DROPPED_RING_SIZE", found.NewEnv)
	}
	if found.OldYAMLKey != "quality_droped_ring_size" {
		t.Errorf("OldYAMLKey = %q, want quality_droped_ring_size", found.OldYAMLKey)
	}
	if found.NewYAMLKey != "quality_dropped_ring_size" {
		t.Errorf("NewYAMLKey = %q, want quality_dropped_ring_size", found.NewYAMLKey)
	}
	if found.RemovedInVersion == "" {
		t.Error("RemovedInVersion must be set")
	}
}

func TestWarnDeprecatedEnvNoOpWhenClean(t *testing.T) {
	t.Setenv("NEXUS_QUALITY_DROPED_RING_SIZE", "")
	// Should not panic and should be a no-op.
	WarnDeprecatedEnv()
}

func TestFindDeprecationByYAMLKey(t *testing.T) {
	d := FindDeprecationByYAMLKey("quality_droped_ring_size")
	if d == nil {
		t.Fatal("expected entry for quality_droped_ring_size")
	}
	if d.NewYAMLKey != "quality_dropped_ring_size" {
		t.Errorf("NewYAMLKey = %q", d.NewYAMLKey)
	}
	if FindDeprecationByYAMLKey("nonexistent_key") != nil {
		t.Error("expected nil for unknown key")
	}
}

func TestMigrateEnvContentRenamesKey(t *testing.T) {
	input := "NEXUS_QUALITY_DROPED_RING_SIZE=128\nNEXUS_OLLAMA_URL=http://localhost:11434\n"
	got, n := MigrateEnvContent(input)
	if n != 1 {
		t.Fatalf("substitutions = %d, want 1", n)
	}
	if strings.Contains(got, "NEXUS_QUALITY_DROPED_RING_SIZE=") {
		t.Errorf("old key still present in output:\n%s", got)
	}
	if !strings.Contains(got, "NEXUS_QUALITY_DROPPED_RING_SIZE=128") {
		t.Errorf("new key missing in output:\n%s", got)
	}
	if !strings.Contains(got, "NEXUS_OLLAMA_URL=http://localhost:11434") {
		t.Errorf("unrelated line should be untouched:\n%s", got)
	}
}

func TestMigrateEnvContentHandlesExportPrefix(t *testing.T) {
	input := "export NEXUS_QUALITY_DROPED_RING_SIZE=256\n"
	got, n := MigrateEnvContent(input)
	if n != 1 {
		t.Fatalf("substitutions = %d, want 1", n)
	}
	if !strings.Contains(got, "export NEXUS_QUALITY_DROPPED_RING_SIZE=256") {
		t.Errorf("export prefix should be preserved:\n%s", got)
	}
}

func TestMigrateEnvContentIgnoresComments(t *testing.T) {
	input := "# NEXUS_QUALITY_DROPED_RING_SIZE=128\n"
	_, n := MigrateEnvContent(input)
	if n != 0 {
		t.Errorf("substitutions = %d, want 0 (comment line)", n)
	}
}

func TestMigrateYAMLContentRenamesKey(t *testing.T) {
	input := "quality_droped_ring_size: 128\nollama_url: http://localhost:11434\n"
	got, n := MigrateYAMLContent(input)
	if n != 1 {
		t.Fatalf("substitutions = %d, want 1", n)
	}
	if strings.Contains(got, "quality_droped_ring_size:") {
		t.Errorf("old key still present:\n%s", got)
	}
	if !strings.Contains(got, "quality_dropped_ring_size: 128") {
		t.Errorf("new key missing:\n%s", got)
	}
	if !strings.Contains(got, "ollama_url: http://localhost:11434") {
		t.Errorf("unrelated line changed:\n%s", got)
	}
}

func TestMigrateYAMLContentIgnoresNestedKeys(t *testing.T) {
	// Indented (nested) key with the same suffix must NOT be rewritten.
	input := "some_section:\n  quality_droped_ring_size: 128\n"
	_, n := MigrateYAMLContent(input)
	if n != 0 {
		t.Errorf("substitutions = %d, want 0 (nested key must be untouched)", n)
	}
}

func TestMigrateYAMLContentNoOpWhenClean(t *testing.T) {
	input := "ollama_url: http://localhost:11434\ntoken_guardrail: 8000\n"
	_, n := MigrateYAMLContent(input)
	if n != 0 {
		t.Errorf("substitutions = %d, want 0", n)
	}
}

func TestMigrateEnvContentNoEnvLeakage(t *testing.T) {
	// Ensure the helper does not leave env vars set (t.Setenv auto-restores).
	os.Unsetenv("NEXUS_QUALITY_DROPED_RING_SIZE")
	MigrateEnvContent("NEXUS_QUALITY_DROPED_RING_SIZE=128\n")
}
