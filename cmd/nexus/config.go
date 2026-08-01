// Subcommand: `nexus config validate <file>` and
// `nexus config migrate <file>`. validate parses a YAML config file,
// validates it against the same rules used by Load(), and exits 0 on
// success or 1 on failure. migrate rewrites deprecated keys in place
// (with a .bak backup) using the deprecation registry (issue #1180).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// configUsage is shown on -h / bad flags.
const configUsage = `nexus config — configuration file operations.

Usage:
  nexus config validate <file>
  nexus config migrate <file>

Commands:
  validate <file>   Parse and validate a YAML config file, then print a
                    summary of the resolved configuration. Exits 0 on
                    success, 1 if the file is missing, unreadable, or
                    contains invalid syntax / indentation.

  migrate <file>    Rewrite deprecated env-var / YAML keys to their
                    current names in place. A <file>.bak backup is
                    written before any change. Recognises .env files
                    (by extension) and .yaml/.yml files. Exits 0 when
                    nothing changed or when migrations were applied,
                    1 on read/write errors.

Examples:
  nexus config validate ./config.yaml
  nexus config validate /etc/nexus/config.yaml
  nexus config migrate ./config.yaml
  nexus config migrate ./.env
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
	case "migrate":
		return runConfigMigrate(args[1:], stdout, stderr)
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

// runConfigMigrate implements `nexus config migrate <file>`.
// It reads a .env or .yaml file, rewrites deprecated keys to current
// names, and writes the result back with a .bak backup of the original.
// Exits 0 when nothing changed or migrations were applied, 1 on
// read/write errors.
func runConfigMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus config migrate", flag.ContinueOnError)
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
		fmt.Fprintln(stderr, "nexus config migrate: no file specified")
		fmt.Fprintln(stderr, "Usage: nexus config migrate <file>")
		return 1
	}

	data, err := os.ReadFile(filepath.Clean(filePath))
	if err != nil {
		fmt.Fprintf(stderr, "nexus config migrate: %v\n", err)
		return 1
	}

	content := string(data)
	var migrated string
	var count int

	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".env":
		migrated, count = config.MigrateEnvContent(content)
	case ".yaml", ".yml":
		migrated, count = config.MigrateYAMLContent(content)
	default:
		// Heuristic: treat as .env if it looks like KEY=VALUE lines.
		if looksLikeEnvFile(content) {
			migrated, count = config.MigrateEnvContent(content)
		} else {
			migrated, count = config.MigrateYAMLContent(content)
		}
	}

	if count == 0 {
		fmt.Fprintf(stdout, "✓ %s: no deprecated keys found\n", filePath)
		return 0
	}

	// Write a .bak backup of the original content.
	bakPath := filePath + ".bak"
	if err := os.WriteFile(bakPath, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "nexus config migrate: cannot write backup %s: %v\n", bakPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "  backup written to %s\n", bakPath)

	if err := os.WriteFile(filePath, []byte(migrated), 0o644); err != nil {
		fmt.Fprintf(stderr, "nexus config migrate: cannot write %s: %v\n", filePath, err)
		return 1
	}

	fmt.Fprintf(stdout, "✓ %s: migrated %d deprecated key(s)\n", filePath, count)
	return 0
}

// looksLikeEnvFile returns true when the content resembles a .env file
// (most non-empty, non-comment lines contain KEY=VALUE).
func looksLikeEnvFile(content string) bool {
	envLines, totalLines := 0, 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		totalLines++
		if strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, "-") {
			envLines++
		}
	}
	return totalLines > 0 && envLines*2 >= totalLines
}
