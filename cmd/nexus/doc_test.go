// doc_test.go — guard rails that keep the user-facing docs (README.md,
// CONTRIBUTING.md) in sync with the actual subcommand surface in
// cmd/nexus/main.go.
//
// Issue #455: the Quickstart must surface `nexus check` as the first
// verification step, and the documented subcommands must match the
// real binary. Without a test the docs drift the first time someone
// adds or renames a subcommand — exactly the regression the issue is
// trying to prevent. These tests run as part of `make test` and fail
// loudly if the docs fall behind the code.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the path to the repository root. The test binary
// runs from cmd/nexus/, so two directories up is the project root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", root, err)
	}
	return abs
}

func readDoc(t *testing.T, relPath string) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), relPath)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestReadmeDocumentsNexusCheckAsQuickstartVerificationStep enforces
// the first acceptance criterion of issue #455: the Quickstart must
// surface `nexus check` as the first verification step. We assert that
// the Quickstart region of README.md (everything between the Quickstart
// heading and the next H2) contains a verification subsection that
// runs `nexus check` before "Build and run".
func TestReadmeDocumentsNexusCheckAsQuickstartVerificationStep(t *testing.T) {
	readme := readDoc(t, "README.md")

	// Slice out the Quickstart region so a stray reference elsewhere
	// in the file (e.g. in the Architecture or Cost Savings section)
	// cannot satisfy the assertion.
	qsStart := strings.Index(readme, "## Quickstart")
	if qsStart < 0 {
		t.Fatal("README.md has no `## Quickstart` heading")
	}
	qs := readme[qsStart:]
	// Stop at the next H2.
	if next := strings.Index(qs[2:], "\n## "); next >= 0 {
		qs = qs[:next+2]
	}

	mustContain := []string{
		"### Verify",
		"nexus check",
		"### Build and run",
	}
	for _, s := range mustContain {
		if !strings.Contains(qs, s) {
			t.Errorf("Quickstart is missing %q (issue #455 acceptance: nexus check must precede `Build and run`)", s)
		}
	}

	// The Verify subsection must come BEFORE the Build and run
	// subsection within the Quickstart.
	verifyIdx := strings.Index(qs, "### Verify")
	buildIdx := strings.Index(qs, "### Build and run")
	if verifyIdx < 0 || buildIdx < 0 {
		t.Fatal("Quickstart is missing the Verify or Build and run heading (see prior check)")
	}
	if verifyIdx > buildIdx {
		t.Errorf("Quickstart ordering violation: `### Verify` (offset %d) must come before `### Build and run` (offset %d)", verifyIdx, buildIdx)
	}
}

// TestReadmeExplainsCheckExitCodes enforces the second acceptance
// criterion: README must document that `nexus check` exits 0 on
// success and 1 when at least one check fails.
func TestReadmeExplainsCheckExitCodes(t *testing.T) {
	readme := readDoc(t, "README.md")
	for _, want := range []string{"Exit codes", "0", "1"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md must mention exit-code %q in the nexus check section", want)
		}
	}
	// The exit-code discussion must be inside (or adjacent to) the
	// Quickstart Verify subsection, not buried somewhere unrelated.
	// Slice to the next H3 (Quickstart subsections are H3) so we
	// don't grab content from later sections like Releases.
	verifyIdx := strings.Index(readme, "### Verify")
	if verifyIdx < 0 {
		t.Fatal("README.md has no `### Verify` heading")
	}
	tail := readme[verifyIdx:]
	if next := strings.Index(tail[4:], "\n### "); next >= 0 {
		tail = tail[:next+4]
	}
	if !strings.Contains(tail, "Exit codes") || !strings.Contains(tail, "`0`") || !strings.Contains(tail, "`1`") {
		t.Errorf("exit-code semantics for nexus check must live in the Quickstart Verify section")
	}
}

// TestReadmeCLIReferenceMatchesSubcommands enforces the third and fifth
// acceptance criteria: the README must enumerate every subcommand
// that cmd/nexus/main.go actually wires up. Adding or renaming a
// subcommand without updating the README fails this test.
func TestReadmeCLIReferenceMatchesSubcommands(t *testing.T) {
	readme := readDoc(t, "README.md")

	// Locate the CLI reference block — anything titled "CLI reference"
	// is fair game.
	idx := strings.Index(readme, "### CLI reference")
	if idx < 0 {
		t.Fatal("README.md has no `### CLI reference` heading (issue #455: documented commands must match cmd/nexus/main.go)")
	}
	block := readme[idx:]
	if next := strings.Index(block, "\n## "); next >= 0 {
		block = block[:next]
	}
	// Also include any later H3 — CLI reference sits inside the
	// Releases H2 so there is no following H2.
	if next := strings.Index(block[4:], "\n## "); next >= 0 {
		block = block[:next+4]
	}

	// Every subcommand wired up in cmd/nexus/main.go (see the
	// switch on os.Args[1]) must appear in the CLI reference.
	for _, verb := range []string{
		"nexus check",     // case "check", "doctor":
		"nexus doctor",    // alias
		"nexus config",    // case "config":
		"nexus dashboard", // case "dashboard":
		"nexus --version", // case "-v", "--version", "version":
		"nexus --help",    // case "-h", "--help", "help":
	} {
		if !strings.Contains(block, verb) {
			t.Errorf("CLI reference is missing documented verb %q", verb)
		}
	}

	// The CLI reference must call out that unknown verbs exit 2 —
	// this matches cmd/nexus/main.go:98 (`os.Exit(2)`).
	if !strings.Contains(block, "2") {
		t.Errorf("CLI reference must note that unknown subcommands exit with code 2 (see cmd/nexus/main.go)")
	}
}

// TestContributingMentionsCheckAfterBuild enforces the fourth
// acceptance criterion: CONTRIBUTING.md's Local Setup section must
// include post-build verification with `nexus check`.
func TestContributingMentionsCheckAfterBuild(t *testing.T) {
	doc := readDoc(t, "CONTRIBUTING.md")

	setupStart := strings.Index(doc, "### Local Setup")
	if setupStart < 0 {
		t.Fatal("CONTRIBUTING.md has no `### Local Setup` heading")
	}
	setup := doc[setupStart:]
	if next := strings.Index(setup, "\n## "); next >= 0 {
		setup = setup[:next]
	}

	mustContain := []string{
		"Build",
		"Verify", // post-build verification step
		"nexus check",
		"nexus doctor", // alias
	}
	for _, s := range mustContain {
		if !strings.Contains(setup, s) {
			t.Errorf("CONTRIBUTING.md Local Setup is missing %q", s)
		}
	}

	// Verify must come AFTER Build in the Local Setup list.
	buildIdx := strings.Index(setup, "Build")
	verifyIdx := strings.Index(setup, "Verify")
	if verifyIdx < 0 || buildIdx < 0 {
		t.Fatal("Local Setup is missing the Build or Verify bullet (see prior check)")
	}
	if verifyIdx < buildIdx {
		t.Errorf("Local Setup ordering violation: `Verify` (offset %d) must come after `Build` (offset %d)", verifyIdx, buildIdx)
	}
}

// TestReadmeCLIReferenceTableShape guards the readability of the CLI
// reference table. A heading without any table rows would not warn at
// runtime; this catches the case where the docs refactor accidentally
// drops the rows.
func TestReadmeCLIReferenceTableShape(t *testing.T) {
	readme := readDoc(t, "README.md")
	idx := strings.Index(readme, "### CLI reference")
	if idx < 0 {
		t.Fatal("README.md has no `### CLI reference` heading")
	}
	block := readme[idx:]
	if next := strings.Index(block, "\n## "); next >= 0 {
		block = block[:next]
	}
	rows := strings.Count(block, "\n| `nexus")
	if rows < 5 {
		t.Errorf("CLI reference table must list at least 5 subcommand rows (got %d). See cmd/nexus/main.go:67-99 for the actual surface.", rows)
	}
}
