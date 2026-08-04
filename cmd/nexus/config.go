// Subcommand: `nexus config validate <file>`, `nexus config migrate <file>`,
// and `nexus config show`. validate parses a YAML config file, validates it
// against the same rules used by Load(), and exits 0 on success or 1 on
// failure. migrate rewrites deprecated keys in place (with a .bak backup)
// using the deprecation registry (issue #1180). show prints the resolved
// effective configuration as a table (issue #1237).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// configUsage is shown on -h / bad flags.
const configUsage = `nexus config — configuration file operations.

Usage:
  nexus config validate <file>
  nexus config migrate <file>
  nexus config show [--diff <file>] [flags]

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

  show [flags]      Print the resolved effective configuration as a
                     table: KEY, VALUE, SOURCE, RELOAD. SOURCE is one
                     of env (set in environment), file (set in config
                     file), or default (hard-coded). RELOAD is yes when
                     the knob is hot-reloadable via SIGHUP, no otherwise.
                     By default all knobs are shown; use --reloadable to
                     filter to the 15 hot-reloadable ones.

Flags:
  --json            Emit configuration as a single JSON object keyed by
                    env var name (each value is {value, source, reload}).
  --reloadable      Show only hot-reloadable knobs (those readable via
                    SIGHUP without a restart).
  --diff <file>     Compare the resolved effective config against the
                    values in <file> (a .yaml/.yml or .env file) and
                    print only the keys that differ. The resolved config
                    (env overrides file) is the baseline; lines from
                    <file> that match the baseline are omitted.

Examples:
  nexus config validate ./config.yaml
  nexus config validate /etc/nexus/config.yaml
  nexus config migrate ./config.yaml
  nexus config migrate ./.env
  nexus config show
  nexus config show --json
  nexus config show --reloadable
  nexus config show --diff ./config.yaml
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
	case "show":
		return runConfigShow(args[1:], stdout, stderr)
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

// runConfigShow implements `nexus config show [flags]`.
func runConfigShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, configUsage) }

	var (
		showJSON       = fs.Bool("json", false, "emit JSON")
		showReloadable = fs.Bool("reloadable", false, "show only hot-reloadable knobs")
		diffFile       = fs.String("diff", "", "compare resolved config against a config file")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Load the resolved config and the file config map.
	filePath := configFilePath()
	fileCfg, err := config.LoadFile(filePath)
	if err != nil {
		fmt.Fprintf(stderr, "nexus config show: cannot read config file %s: %v\n", filePath, err)
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "nexus config show: %v\n", err)
		return 1
	}

	fields := cfg.ShowFields(fileCfg)

	// Filter to reloadable only if requested.
	if *showReloadable {
		var filtered []config.ConfigField
		for _, f := range fields {
			if f.HotReloadable {
				filtered = append(filtered, f)
			}
		}
		fields = filtered
	}

	// If --diff is set, load the diff file and filter to differing keys.
	if *diffFile != "" {
		diffFileCfg, diffErr := config.LoadFile(*diffFile)
		if diffErr != nil {
			fmt.Fprintf(stderr, "nexus config show: cannot load diff file %s: %v\n", *diffFile, diffErr)
			return 1
		}
		fields = diffFields(fields, diffFileCfg)
	}

	// Emit output.
	if *showJSON {
		return emitShowJSON(fields, stdout, stderr)
	}
	return emitShowTable(fields, stdout, stderr)
}

// configFilePath returns the effective config file path used by Load().
// Duplicated here so runConfigShow can display the file it used.
func configFilePath() string {
	if f := os.Getenv("NEXUS_CONFIG_FILE"); f != "" {
		return f
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "nexus-proxy", "config.yaml")
	}
	return "./config.yaml"
}

// diffFields returns only the fields whose resolved value differs from the
// corresponding value in the diff file (map[string]string from LoadFile).
// Fields with no EnvToYAMLKey mapping are included with Source "<env-only>".
func diffFields(fields []config.ConfigField, diffFileCfg map[string]string) []config.ConfigField {
	var out []config.ConfigField
	for _, f := range fields {
		yamlKey := config.EnvToYAMLKey[f.Key]
		if yamlKey == "" {
			// No YAML key mapping — include with "<env-only>" source so the
			// operator can see the discrepancy instead of silent omission.
			f.Source = "<env-only>"
			out = append(out, f)
			continue
		}
		diffVal, ok := diffFileCfg[yamlKey]
		if !ok {
			// Key not in diff file → it matches (is default/env, not in file).
			continue
		}
		// The diff file value vs the resolved value.
		// We compare string representations; for secrets the resolved
		// value may be redacted, so we compare field-by-field via the
		// env key lookup in the diff cfg's raw form.
		if diffVal == f.Value {
			continue
		}
		out = append(out, f)
	}
	return out
}

// emitShowTable prints the human-readable table to stdout.
func emitShowTable(fields []config.ConfigField, stdout, stderr io.Writer) int {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE\tRELOAD")
	for _, f := range fields {
		reload := "no"
		if f.HotReloadable {
			reload = "yes"
		}
		// Truncate long values for readability.
		val := f.Value
		if len(val) > 60 {
			val = val[:57] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Key, val, f.Source, reload)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "nexus config show: write error: %v\n", err)
		return 1
	}
	return 0
}

// showJSONEntry is the per-field shape emitted when --json is used.
type showJSONEntry struct {
	Value         string `json:"value"`
	Source        string `json:"source"`
	HotReloadable bool   `json:"reloadable"`
}

// emitShowJSON prints the JSON object to stdout.
func emitShowJSON(fields []config.ConfigField, stdout, stderr io.Writer) int {
	obj := make(map[string]showJSONEntry)
	for _, f := range fields {
		obj[f.Key] = showJSONEntry{
			Value:         f.Value,
			Source:        f.Source,
			HotReloadable: f.HotReloadable,
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		fmt.Fprintf(stderr, "nexus config show: json encode error: %v\n", err)
		return 1
	}
	return 0
}
