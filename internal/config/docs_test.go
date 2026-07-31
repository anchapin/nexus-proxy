package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestContributingDocAccuracy (issue #452) locks the factual claims made
// by the "Environment Variables" bullet in CONTRIBUTING.md so the docs
// cannot drift from the code again. The previous instruction pointed
// contributors at a nonexistent `configKeys` map; the corrected text
// names the Config struct field, the getEnv* helper family, and the
// YAML mirror in yaml.go. This test fails if any documented helper is
// removed or renamed, or if a configKeys identifier is reintroduced.
func TestContributingDocAccuracy(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate test file")
	}
	dir := filepath.Dir(here)

	// Every helper named in CONTRIBUTING.md must be a callable
	// package-level function. A rename or accidental removal trips the
	// test before the docs go stale again.
	wantHelpers := map[string]bool{
		"getEnv":           false,
		"getEnvAllowEmpty": false,
		"getEnvInt":        false,
		"getEnvBool":       false,
		"getEnvFloat":      false,
		"getEnvDuration":   false,
		"getEnvRegexps":    false,
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				if _, tracked := wantHelpers[fn.Name.Name]; tracked {
					wantHelpers[fn.Name.Name] = true
				}
				if fn.Name.Name == "configKeys" {
					t.Errorf("internal/config must not declare a package-level configKeys func (CONTRIBUTING.md, issue #452): %s",
						fset.Position(fn.Pos()))
				}
			}
		}
		// Reject any configKeys identifier anywhere else (var decl,
		// map literal, struct field, parameter, …) — the docs
		// explicitly say "no central registry".
		ast.Inspect(file, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "configKeys" {
				t.Errorf("internal/config must not reference a configKeys identifier (CONTRIBUTING.md, issue #452): %s",
					fset.Position(ident.Pos()))
			}
			return true
		})
	}

	for name, seen := range wantHelpers {
		if !seen {
			t.Errorf("documented helper %s is missing from internal/config (CONTRIBUTING.md would be stale)", name)
		}
	}
}
