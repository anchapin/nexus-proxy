// Subcommand: `nexus check` (alias `nexus doctor`). Runs the boot-
// time diagnostic suite defined in internal/diag, prints the result
// in either a human-readable table or a JSON array, and exits 0 when
// every check passed (warn + skip are fine) or 1 when at least one
// check failed.
//
// This file is the CLI adapter; the actual check logic lives in
// internal/diag. Keeping the two separate means a future operator
// tool (a TUI, a HTTP /diagz endpoint) can drive the same suite
// without re-implementing the wiring.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/diag"
)

// checkUsage is shown on -h / bad flags. Kept compact because the
// proxy is an operator tool, not an end-user app.
const checkUsage = `nexus check — boot-time configuration diagnostics for the Nexus Proxy.

Usage:
  nexus check [--json]

Flags:
  --json              Emit a JSON array of check results instead of a
                      human-readable table.

Exit code is 0 when every check passed (warn + skip are fine), 1 when
at least one check failed.

Aliases: nexus doctor.
`

// runCheck is the testable core of the `nexus check` subcommand.
// args and stdout/stderr are parameters so a future test can drive
// the full CLI end-to-end. The function returns a process exit code
// (0 or 1) rather than calling os.Exit so tests can assert against
// it without spawning a subprocess.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, checkUsage) }

	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of a human-readable table")

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already printed the error and Usage.
		// flag.ErrHelp is the sentinel for -h/--help: the Usage was
		// rendered, the caller wanted information, so exit 0 rather
		// than 1 (matches every other CLI's behaviour).
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "nexus check: config: %v\n", err)
		return 1
	}

	// Use a bounded timeout so a single hung endpoint cannot stall
	// the whole command past the 10-second acceptance criterion.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res := diag.Run(ctx, cfg, diag.Options{
		Timeout: 5 * time.Second,
	})

	if asJSON {
		if err := renderCheckJSON(res, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus check: %v\n", err)
			return 1
		}
	} else {
		renderCheckTable(res, stdout)
	}

	if res.Failed() > 0 {
		return 1
	}
	return 0
}

// renderCheckTable emits the canonical human-readable report:
//
//	Nexus Proxy — Configuration Check
//	===================================
//	[PASS] Ollama reachable at http://localhost:11434
//	[FAIL] Model 'nomic-embed-text' not pulled → Run: ollama pull nomic-embed-text
//	...
//
//	1 check failed, 1 warning.
func renderCheckTable(r diag.Result, w io.Writer) {
	fmt.Fprintln(w, "Nexus Proxy — Configuration Check")
	fmt.Fprintln(w, "===================================")
	for _, c := range r {
		fmt.Fprintf(w, "[%s] %s\n", statusLabel(c.Status), formatDetail(c))
	}
	failed := r.Failed()
	warned := r.Warned()
	switch {
	case failed > 0 && warned > 0:
		fmt.Fprintf(w, "\n%d check(s) failed, %d warning(s).\n", failed, warned)
	case failed > 0:
		fmt.Fprintf(w, "\n%d check(s) failed.\n", failed)
	case warned > 0:
		fmt.Fprintf(w, "\nAll checks passed, %d warning(s).\n", warned)
	default:
		fmt.Fprintln(w, "\nAll checks passed.")
	}
}

// statusLabel renders the four-state status as the bracketed tag
// used in both the table and (when piped to a terminal) the exit
// status header. Kept uppercase for visual scanning.
func statusLabel(s diag.Status) string {
	switch s {
	case diag.StatusPass:
		return "PASS"
	case diag.StatusFail:
		return "FAIL"
	case diag.StatusWarn:
		return "WARN"
	default:
		return "SKIP"
	}
}

// formatDetail maps a Check onto the "Name — detail" string the
// table prints. Skipped checks get a quieter phrasing so the report
// does not look like every step failed.
func formatDetail(c diag.Check) string {
	if c.Detail == "" {
		return c.Name
	}
	if c.Status == diag.StatusSkip {
		return fmt.Sprintf("%s — %s", c.Name, c.Detail)
	}
	return fmt.Sprintf("%s — %s", c.Name, c.Detail)
}

// renderCheckJSON emits a JSON array of Check objects. The shape is
// stable (matches diag.Check's tagged fields) so CI scripts can
// pipe the output through `jq` without surprises.
func renderCheckJSON(r diag.Result, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
