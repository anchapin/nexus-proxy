package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunConfigValidate(t *testing.T) {
	// Create a valid temporary YAML file.
	tmp := t.TempDir()
	validFile := filepath.Join(tmp, "valid.yaml")
	if err := os.WriteFile(validFile, []byte(`ollama_url: http://localhost:11434
telemetry_path: /tmp/nexus-telemetry.jsonl
token_guardrail: 8000
`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a file with invalid indentation (nested without section header).
	badFile := filepath.Join(tmp, "bad_indent.yaml")
	if err := os.WriteFile(badFile, []byte(`ollama_url: http://localhost:11434
  nested: true
`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a file with a valid top-level key and valid nested under a section.
	nestedFile := filepath.Join(tmp, "nested.yaml")
	if err := os.WriteFile(nestedFile, []byte(`frontier:
  nested: value
`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a file with an invalid field value: negative shutdown timeout.
	negativeShutdownFile := filepath.Join(tmp, "negative_shutdown.yaml")
	if err := os.WriteFile(negativeShutdownFile, []byte(`shutdown_timeout: -5s
`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a file with an invalid bool value.
	invalidBoolFile := filepath.Join(tmp, "invalid_bool.yaml")
	if err := os.WriteFile(invalidBoolFile, []byte(`toon_unfenced: maybe
`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantSubstr string
	}{
		{
			name:       "valid file prints summary",
			args:       []string{"validate", validFile},
			wantExit:   0,
			wantSubstr: "is valid",
		},
		{
			name:       "missing file exits 1",
			args:       []string{"validate", "/nonexistent/path.yaml"},
			wantExit:   1,
			wantSubstr: "cannot read",
		},
		{
			name:       "bad indentation exits 1",
			args:       []string{"validate", badFile},
			wantExit:   1,
			wantSubstr: "cannot unmarshal",
		},
		{
			name:       "nested without section is valid",
			args:       []string{"validate", nestedFile},
			wantExit:   0,
			wantSubstr: "is valid",
		},
		{
			name:       "negative shutdown_timeout exits 1",
			args:       []string{"validate", negativeShutdownFile},
			wantExit:   1,
			wantSubstr: "shutdown_timeout",
		},
		{
			name:       "invalid bool value exits 1",
			args:       []string{"validate", invalidBoolFile},
			wantExit:   1,
			wantSubstr: "toon_unfenced",
		},
		{
			name:       "no args shows usage",
			args:       []string{},
			wantExit:   0,
			wantSubstr: "nexus config validate",
		},
		{
			name:       "help flag shows usage",
			args:       []string{"-h"},
			wantExit:   0,
			wantSubstr: "nexus config validate",
		},
		{
			name:       "unknown verb falls through to validate (file path)",
			args:       []string{validFile},
			wantExit:   0,
			wantSubstr: "is valid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := runConfig(tt.args, &stdout, &stderr)
			if got != tt.wantExit {
				t.Errorf("runConfig exit = %d, want %d\nstderr: %s", got, tt.wantExit, stderr.String())
				return
			}
			out := stdout.String() + stderr.String()
			if tt.wantSubstr != "" && !bytes.Contains([]byte(out), []byte(tt.wantSubstr)) {
				t.Errorf("runConfig output = %q, want substring %q", out, tt.wantSubstr)
			}
		})
	}
}

func TestRunConfigMigrateYAML(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "config.yaml")
	original := "quality_droped_ring_size: 128\nollama_url: http://localhost:11434\n"
	if err := os.WriteFile(file, []byte(original), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"migrate", file}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	// Backup should exist.
	bak := file + ".bak"
	bakData, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if string(bakData) != original {
		t.Errorf("backup content = %q, want %q", bakData, original)
	}

	// File should have the new key.
	migrated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(migrated, []byte("quality_dropped_ring_size: 128")) {
		t.Errorf("migrated file missing new key:\n%s", migrated)
	}
	if bytes.Contains(migrated, []byte("quality_droped_ring_size:")) {
		t.Errorf("migrated file still has old key:\n%s", migrated)
	}
	if !bytes.Contains(migrated, []byte("ollama_url: http://localhost:11434")) {
		t.Errorf("unrelated line changed:\n%s", migrated)
	}
}

func TestRunConfigMigrateEnv(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, ".env")
	original := "NEXUS_QUALITY_DROPED_RING_SIZE=128\nNEXUS_OLLAMA_URL=http://localhost:11434\n"
	if err := os.WriteFile(file, []byte(original), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"migrate", file}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	migrated, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(migrated, []byte("NEXUS_QUALITY_DROPPED_RING_SIZE=128")) {
		t.Errorf("migrated file missing new key:\n%s", migrated)
	}
	if bytes.Contains(migrated, []byte("NEXUS_QUALITY_DROPED_RING_SIZE=")) {
		t.Errorf("migrated file still has old key:\n%s", migrated)
	}
}

func TestRunConfigMigrateNoOpWhenClean(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "clean.yaml")
	if err := os.WriteFile(file, []byte("ollama_url: http://localhost:11434\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"migrate", file}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "no deprecated keys found") {
		t.Errorf("expected 'no deprecated keys found' message, got: %s", out)
	}

	// No backup should be written when nothing changed.
	if _, err := os.Stat(file + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup should not be written when no changes made")
	}
}

func TestRunConfigMigrateMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"migrate", "/nonexistent/path.yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestRunConfigMigrateNoFileSpecified(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"migrate"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestRunConfigMigrateHelpShownInUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_ = runConfig([]string{}, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "migrate") {
		t.Errorf("usage should document migrate subcommand:\n%s", out)
	}
}
