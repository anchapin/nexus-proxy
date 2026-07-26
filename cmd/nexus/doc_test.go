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
	"strconv"
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

// TestContributingGoVersionMatchesGoMod guards issue #492: the Go
// version documented in CONTRIBUTING.md's Prerequisites must be >= the
// `go` directive in go.mod (which is itself a strict subset of the CI
// pin). A contributor who installs the documented minimum must be able
// to build the tree. Without a test the docs drift the moment go.mod
// is bumped — exactly the regression the issue is trying to prevent.
//
// The documented line is tolerant of suffixes, e.g.
//
//	Install Go 1.25+ (1.26 recommended, matching CI)
//
// We extract the leading "1.<minor>" token and compare it numerically
// against the go.mod `go` directive major.minor. Both are major 1, so
// only the minor is compared.
func TestContributingGoVersionMatchesGoMod(t *testing.T) {
	goModMinor := parseGoDirectiveMinor(t, readDoc(t, "go.mod"))

	doc := readDoc(t, "CONTRIBUTING.md")

	// Locate the Prerequisites line inside Local Setup. Match the
	// documented "Install Go 1.<n>" token specifically.
	setupStart := strings.Index(doc, "### Local Setup")
	if setupStart < 0 {
		t.Fatal("CONTRIBUTING.md has no `### Local Setup` heading")
	}
	setup := doc[setupStart:]
	if next := strings.Index(setup, "\n## "); next >= 0 {
		setup = setup[:next]
	}

	preIdx := strings.Index(setup, "**Prerequisites**")
	if preIdx < 0 {
		t.Fatal("CONTRIBUTING.md Local Setup has no Prerequisites bullet")
	}
	preLine := setup[preIdx:]
	if nl := strings.Index(preLine, "\n"); nl >= 0 {
		preLine = preLine[:nl]
	}

	docMinor, ok := parseDocGoMinor(preLine)
	if !ok {
		t.Fatalf("CONTRIBUTING.md Prerequisites does not quote a Go version: %q", preLine)
	}

	if docMinor < goModMinor {
		t.Errorf("CONTRIBUTING.md documents Go 1.%d but go.mod requires 1.%d — a contributor installing the documented minimum cannot build (issue #492)", docMinor, goModMinor)
	}
}

// parseGoDirectiveMinor extracts the minor version from the `go` line
// of go.mod, e.g. "go 1.25.0" -> 25. A missing or malformed directive
// fails the test rather than silently passing.
func parseGoDirectiveMinor(t *testing.T, goMod string) int {
	t.Helper()
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go ") {
			ver := strings.TrimSpace(strings.TrimPrefix(line, "go "))
			major, minor, ok := splitMajorMinor(ver)
			if !ok || major != 1 {
				t.Fatalf("go.mod `go` directive %q is not a recognised 1.x version", ver)
			}
			return minor
		}
	}
	t.Fatal("go.mod has no `go` directive")
	return 0
}

// parseDocGoMinor extracts the minor version from a documented Go
// version line, tolerating suffixes such as "+" or
// "(1.26 recommended, matching CI)". Returns the minor of the FIRST
// "1.<minor>" token encountered (the documented minimum).
func parseDocGoMinor(line string) (int, bool) {
	// Scan for the first "1.<digits>" run.
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '1' {
			continue
		}
		if i+1 >= len(runes) || runes[i+1] != '.' {
			continue
		}
		// Collect digits after the dot.
		j := i + 2
		for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
			j++
		}
		if j == i+2 {
			continue // no digits after dot
		}
		minor, err := strconv.Atoi(string(runes[i+2 : j]))
		if err != nil {
			continue
		}
		return minor, true
	}
	return 0, false
}

// splitMajorMinor splits "1.25.0" / "1.25" into (1, 25, true).
func splitMajorMinor(ver string) (int, int, bool) {
	ver = strings.TrimSpace(ver)
	// Trim any toolchain-style suffix.
	if sp := strings.IndexAny(ver, " \t"); sp >= 0 {
		ver = ver[:sp]
	}
	parts := strings.Split(ver, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
