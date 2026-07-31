package main

import (
	"bytes"
	"os"
	"path/filepath"
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
