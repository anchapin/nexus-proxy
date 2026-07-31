// Subcommand: `nexus config validate <file>`. Parses a YAML config file,
// validates it against the same rules used by Load(), and exits 0 on success
// or 1 on failure (with a descriptive error message).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// configUsage is shown on -h / bad flags.
const configUsage = `nexus config — configuration file operations.

Usage:
  nexus config validate <file>

Commands:
  validate <file>   Parse and validate a YAML config file, then print a
                    summary of the resolved configuration. Exits 0 on
                    success, 1 if the file is missing, unreadable, or
                    contains invalid syntax / indentation.

Examples:
  nexus config validate ./config.yaml
  nexus config validate /etc/nexus/config.yaml
`

// runConfig is the testable core of the `nexus config` subcommand.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, configUsage)
		return 0
	}

	switch args[0] {
	case "validate":
		return runConfigValidate(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stderr, configUsage)
		return 0
	default:
		// Treat an unknown verb as a file path for ergonomics:
		//   nexus config ./myconfig.yaml  →  nexus config validate ./myconfig.yaml
		return runConfigValidate(args, stdout, stderr)
	}
}

// runConfigValidate implements `nexus config validate <file>`.
func runConfigValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus config validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, configUsage) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	filePath := fs.Arg(0)
	if filePath == "" {
		fmt.Fprintln(stderr, "nexus config validate: no file specified")
		fmt.Fprintln(stderr, "Usage: nexus config validate <file>")
		return 1
	}

	// LoadYAML runs the full validation pipeline (parse YAML → validate fields →
	// toConfig → env override), catching out-of-range values like negative
	// durations or invalid threshold ranges that LoadFile (which only parses
	// indentation) would miss.
	_, err := config.LoadYAML(filePath)
	if err != nil {
		fmt.Fprintf(stderr, "nexus config validate: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "✓ %s is valid\n", filePath)
	return 0
}

// Note: os is not directly used here; retained for future FilePrinter interface.
