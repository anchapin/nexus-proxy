// docker_build_test.go — drift guard keeping the Dockerfile build-stage
// Go version aligned with go.mod's `go` directive.
//
// Issue #541: the Dockerfile pinned `golang:1.21-alpine` while go.mod
// required `go 1.25.0`. Since Go 1.21 the toolchain treats the `go`
// directive as a hard minimum, so every `docker build .` — and the
// release GHCR image job — failed with "go.mod requires go >= 1.25.0".
// Worse, the release job did not depend on the docker job, so binaries
// + SBOM shipped advertising a `docker pull` of an image that was never
// built.
//
// This test resolves the Go toolchain a plain `docker build .` (no
// --build-arg) compiles with and asserts it is >= the go.mod `go`
// directive. Without it, bumping go.mod without touching the Dockerfile
// (or vice versa) silently reintroduces the breakage. It runs as part
// of `make test` / `make test-race`, so CI catches the drift even on a
// branch where the heavier `docker` smoke job has not run yet.
//
// The companion ci.yml `docker` job performs a real `docker build .`
// to catch Dockerfile-level breakage (syntax, missing COPY sources)
// that a version-number comparison cannot detect.
package main

import (
	"strings"
	"testing"
)

// TestDockerfileGoVersionSatisfiesGoMod is the core drift guard for
// issue #541. It resolves the effective Go minor a local
// `docker build .` uses and confirms it can compile the module
// (Dockerfile Go >= go.mod `go` directive).
func TestDockerfileGoVersionSatisfiesGoMod(t *testing.T) {
	dockerfile := readDoc(t, "Dockerfile")
	goModMinor := parseGoDirectiveMinor(t, readDoc(t, "go.mod"))

	dockerMinor, ok := parseDockerfileGoMinor(t, dockerfile)
	if !ok {
		t.Fatalf("could not determine Dockerfile Go version — the build stage has no `FROM golang:<ver>` line or resolvable `ARG GO_VERSION` default")
	}

	if dockerMinor < goModMinor {
		t.Errorf("Dockerfile build stage uses Go 1.%d but go.mod requires go 1.%d — "+
			"`docker build .` will fail with \"go.mod requires go >= 1.%d.0\" (issue #541)",
			dockerMinor, goModMinor, goModMinor)
	}
}

// parseDockerfileGoMinor resolves the Go minor version a plain
// `docker build .` (no --build-arg) compiles with. It honours an
// `ARG GO_VERSION=<ver>` default when the FROM line references
// ${GO_VERSION}, and otherwise parses a literal `golang:<ver>` tag.
// Returns ok=false if no version can be resolved so the test fails
// loudly instead of silently passing on a refactored Dockerfile.
func parseDockerfileGoMinor(t *testing.T, dockerfile string) (int, bool) {
	t.Helper()

	argDefault, hasArg := parseArgGoVersionDefault(dockerfile)

	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "FROM") {
			continue
		}
		token, found := extractGolangVersionToken(line)
		if !found {
			continue
		}
		// Resolve a ${GO_VERSION} / $GO_VERSION reference to the ARG
		// default — that is what a build with no --build-arg uses.
		if isVarRef(token) {
			if !hasArg {
				t.Fatalf("Dockerfile FROM references %q but no ARG GO_VERSION default is declared", token)
			}
			token = argDefault
		}
		major, minor, ok := splitMajorMinor(token)
		if !ok || major != 1 {
			t.Fatalf("Dockerfile Go version %q is not a recognised 1.x version", token)
		}
		return minor, true
	}
	return 0, false
}

// extractGolangVersionToken pulls the version segment out of a FROM
// line such as:
//
//	FROM golang:${GO_VERSION}-alpine AS build   -> "${GO_VERSION}"
//	FROM golang:1.26-alpine AS build            -> "1.26"
//	FROM golang:1.26.0                          -> "1.26.0"
//
// It returns found=false for lines that are not a golang base image.
func extractGolangVersionToken(fromLine string) (string, bool) {
	const marker = "golang:"
	idx := strings.Index(fromLine, marker)
	if idx < 0 {
		return "", false
	}
	rest := fromLine[idx+len(marker):]
	// The image ref ends at the first whitespace (`... AS build`).
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		rest = rest[:sp]
	}
	// Strip the variant suffix (e.g. "-alpine", "-bookworm") so only
	// the version segment remains. A bare tag like "1.26" is unchanged.
	if dash := strings.Index(rest, "-"); dash >= 0 {
		rest = rest[:dash]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// parseArgGoVersionDefault extracts the default value of an
// `ARG GO_VERSION=<ver>` directive. Returns found=false when no such
// ARG with a default exists.
func parseArgGoVersionDefault(dockerfile string) (string, bool) {
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ARG ") {
			continue
		}
		arg := strings.TrimSpace(strings.TrimPrefix(line, "ARG "))
		if !strings.HasPrefix(arg, "GO_VERSION") {
			continue
		}
		eq := strings.Index(arg, "=")
		if eq < 0 {
			continue // ARG with no default — not resolvable for a plain build
		}
		val := strings.TrimSpace(arg[eq+1:])
		val = strings.Trim(val, `"'`)
		if val == "" {
			continue
		}
		return val, true
	}
	return "", false
}

// isVarRef reports whether the token is a Dockerfile variable reference
// such as ${GO_VERSION} or $GO_VERSION.
func isVarRef(token string) bool {
	return strings.HasPrefix(token, "$")
}
