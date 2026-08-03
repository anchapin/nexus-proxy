// dispatch.go — subcommand routing extracted from main() so it can be
// unit-tested without os.Exit. Returns (exitCode, handled); when handled
// is false the caller proceeds to start the proxy server (issue #1141).
package main

import (
	"fmt"
	"io"
	"os"
)

// dispatch inspects args[1:] and routes to the appropriate subcommand
// handler. It returns the process exit code and a boolean indicating
// whether a subcommand was handled. When handled is false the caller
// should proceed with the default proxy-server boot path.
//
// stdout/stderr are parameters so tests can capture output without
// redirecting os.Stdout/os.Stderr.
func dispatch(args []string, stdout, stderr io.Writer) (exitCode int, handled bool) {
	if len(args) <= 1 {
		return 0, false
	}
	switch args[1] {
	case "check", "doctor":
		return runCheck(args[2:], stdout, stderr), true
	case "dashboard":
		return runDashboard(args[2:], stdout, stderr), true
	case "config":
		return runConfig(args[2:], stdout, stderr), true
	case "init":
		return runInit(args[2:], stdout, stderr), true
	case "judge":
		return runJudgeStats(args[2:], stdout, stderr), true
	case "route":
		return runRoute(args[2:], stdout, stderr), true
	case "routing-preview":
		return runRoutingPreview(args[2:], stdout, stderr), true
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, "Usage: nexus [init|check|doctor|config|dashboard|judge|route|routing-preview]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Run with no arguments to start the proxy.")
		fmt.Fprintln(stderr, "Run `nexus init` to launch the interactive config wizard.")
		fmt.Fprintln(stderr, "Run `nexus check` to validate boot-time configuration.")
		fmt.Fprintln(stderr, "Run `nexus dashboard` to view the daily savings summary.")
		fmt.Fprintln(stderr, "Run `nexus config validate <file>` to validate a config file.")
		fmt.Fprintln(stderr, "Run `nexus config migrate <file>` to upgrade deprecated config keys.")
		fmt.Fprintln(stderr, "Run `nexus judge stats` to view adaptive routing confidence.")
		fmt.Fprintln(stderr, "Run `nexus route patterns` to view auto-promoted DSL patterns.")
		fmt.Fprintln(stderr, "Run `nexus routing-preview \"prompt\"` to preview routing decisions.")
		fmt.Fprintln(stderr, "Run `nexus --version` to print the build version.")
		return 0, true
	case "-v", "--version", "version":
		printVersion(stdout)
		return 0, true
	default:
		fmt.Fprintf(stderr, "nexus: unknown subcommand %q\n\n", args[1])
		fmt.Fprintln(stderr, "Usage: nexus [init|check|doctor|config|dashboard|judge|route|routing-preview]")
		return 2, true
	}
}

// dispatchFromOS is the thin shim main() calls; it wires dispatch to
// os.Stdout/os.Stderr and calls os.Exit when a subcommand was handled.
// When no subcommand was matched it returns so the caller proceeds
// with the default proxy-server boot path.
func dispatchFromOS() {
	code, handled := dispatch(os.Args, os.Stdout, os.Stderr)
	if handled {
		os.Exit(code)
	}
}
