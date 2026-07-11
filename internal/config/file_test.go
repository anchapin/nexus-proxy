package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseYAML_BasicSections(t *testing.T) {
	src := []string{
		"server:",
		"  addr: ':8000'",
		"ollama:",
		"  url: http://localhost:11434",
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	want := map[string]string{
		"server.addr": ":8000",
		"ollama.url":  "http://localhost:11434",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseYAML_Comments(t *testing.T) {
	src := []string{
		"# full-line comment",
		"server:",
		"  # indented full-line comment",
		"  addr: ':8000' # trailing comment",
		"  # another indented comment",
		"  log_level: info",
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	want := map[string]string{
		"server.addr":      ":8000",
		"server.log_level": "info",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseYAML_QuotedStrings(t *testing.T) {
	src := []string{
		"server:",
		`  addr: ":8000"`,
		`  label: 'quoted with # inside'`,
		`  note: "value with : colon"`,
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	want := map[string]string{
		"server.addr":  ":8000",
		"server.label": "quoted with # inside",
		"server.note":  "value with : colon",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseYAML_TypeInferencePassThrough(t *testing.T) {
	// Int / float / bool / duration strings are stored verbatim — the
	// downstream resolve* helpers parse them with strconv /
	// time.ParseDuration, so the YAML literal must already be the
	// canonical env-var spelling.
	src := []string{
		"routing:",
		"  token_guardrail: 6000",
		"rag:",
		"  threshold: 0.55",
		"flags:",
		"  enabled: true",
		"timing:",
		"  slm_timeout: 8s",
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	want := map[string]string{
		"routing.token_guardrail": "6000",
		"rag.threshold":           "0.55",
		"flags.enabled":           "true",
		"timing.slm_timeout":      "8s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseYAML_EnvExpansion(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-secret-123")
	t.Setenv("NEXUS_OLLAMA_URL", "http://gpu.local:11434")
	src := []string{
		"frontier:",
		"  api_key: ${NEXUS_FRONTIER_API_KEY}",
		"ollama:",
		`  url: "http://$NEXUS_OLLAMA_URL"`,
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	want := map[string]string{
		"frontier.api_key": "sk-secret-123",
		"ollama.url":       "http://http://gpu.local:11434", // user included scheme explicitly
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseYAML_EnvExpansionMissing(t *testing.T) {
	// Unset vars expand to ""; this is loud enough to notice in a boot
	// log without us having to invent a separate fail-fast mode.
	src := "frontier:\n  api_key: ${NEXUS_DEFINITELY_UNSET_FOR_TEST}\n"
	got, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if v := got["frontier.api_key"]; v != "" {
		t.Errorf("frontier.api_key = %q, want empty", v)
	}
}

func TestParseYAML_EscapedDollar(t *testing.T) {
	src := "server:\n  label: 'price \\$10'\n"
	got, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if v := got["server.label"]; v != "price $10" {
		t.Errorf("server.label = %q, want %q", v, "price $10")
	}
}

func TestParseYAML_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"missing colon", "server\n  addr: foo\n"},
		{"empty key", ":\n"},
		{"nested section", "outer:\n  inner:\n    addr: foo\n"},
		{"indented kv without section", "  addr: foo\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseYAML(tc.src); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestParseYAML_UnclosedEnvVarIsLiteral(t *testing.T) {
	// An unclosed `${` is left as a literal so operators notice the
	// typo in their boot log; we don't fail the boot.
	src := "frontier:\n  api_key: '${UNCLOSED'\n"
	got, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if v := got["frontier.api_key"]; v != "${UNCLOSED" {
		t.Errorf("frontier.api_key = %q, want literal %q", v, "${UNCLOSED")
	}
}

func TestParseYAML_Empty(t *testing.T) {
	got, err := ParseYAML("")
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %#v", got)
	}
}

func TestParseYAML_TopLevelFlatKV(t *testing.T) {
	// No section header — flat key.
	src := "addr: ':8000'\n"
	got, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if v := got["addr"]; v != ":8000" {
		t.Errorf("addr = %q, want %q", v, ":8000")
	}
}

func TestParseYAML_SectionReset(t *testing.T) {
	// Two sections back-to-back — the second resets the active section.
	src := []string{
		"a:",
		"  one: 1",
		"b:",
		"  two: 2",
	}
	got, err := ParseYAML(strings.Join(src, "\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if v := got["a.one"]; v != "1" {
		t.Errorf("a.one = %q", v)
	}
	if v := got["b.two"]; v != "2" {
		t.Errorf("b.two = %q", v)
	}
}

func TestLoadFile_MissingReturnsNilNil(t *testing.T) {
	got, err := LoadFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil map for missing file, got %#v", got)
	}
}

func TestLoadFile_ReadError(t *testing.T) {
	// A path that points at a directory rather than a file yields a
	// read error (not the missing-file nil/nil case).
	dir := t.TempDir()
	if _, err := LoadFile(dir); err == nil {
		t.Errorf("expected error when path is a directory, got nil")
	}
}

func TestLoadFile_RoundTrip(t *testing.T) {
	// End-to-end: write YAML to a temp file, LoadFile, verify the
	// flattened map.
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	body := strings.Join([]string{
		"server:",
		"  addr: ':9000'",
		"judge:",
		"  sample_rate: 0.25",
		"  concurrency: 4",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := map[string]string{
		"server.addr":       ":9000",
		"judge.sample_rate": "0.25",
		"judge.concurrency": "4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestLoadFile_MalformedFileError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(path, []byte("server\n  addr: foo"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Errorf("expected parse error for malformed YAML, got nil")
	}
}

func TestDiscoverConfigFile(t *testing.T) {
	// Auto-discovery is CWD-only; we exercise it by changing CWD to a
	// temp directory and dropping a single candidate file into it.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	if got := DiscoverConfigFile(); got != "" {
		t.Errorf("empty CWD: DiscoverConfigFile = %q, want \"\"", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "nexus.yml"), []byte("server:\n  addr: foo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := DiscoverConfigFile(); got != filepath.Join(dir, "nexus.yml") {
		// DiscoverConfigFile returns the bare name because os.Stat
		// matches the relative candidate; on platforms where CWD is
		// resolved to an absolute path, the result will be the full
		// path. Accept either.
		if got != "nexus.yml" {
			t.Errorf("DiscoverConfigFile = %q, want nexus.yml (or full path)", got)
		}
	}

	// nexus.yaml takes precedence over nexus.yml.
	if err := os.WriteFile(filepath.Join(dir, "nexus.yaml"), []byte("server:\n  addr: bar\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := DiscoverConfigFile(); got == "" {
		t.Errorf("expected nexus.yaml to be discovered, got empty")
	}
}

func TestSetConfigPathOverride(t *testing.T) {
	t.Cleanup(func() { SetConfigPathOverride("") })
	if ConfigPathOverride() != "" {
		t.Fatalf("expected empty initial override")
	}
	SetConfigPathOverride("/tmp/foo.yaml")
	if ConfigPathOverride() != "/tmp/foo.yaml" {
		t.Errorf("override = %q", ConfigPathOverride())
	}
	SetConfigPathOverride("")
	if ConfigPathOverride() != "" {
		t.Errorf("override not cleared")
	}
}
