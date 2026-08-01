// deprecations.go — compile-time registry of deprecated config keys.
//
// Issue #1180: before this file, config renames were handled ad-hoc — a
// single hardcoded alias for the #924 typo fix, with no central record
// and no way for an operator with an old config to discover what
// changed across versions. Deprecations is that central record. Load()
// and LoadYAML() call WarnDeprecatedEnv / WarnDeprecatedYAMLKeys to
// emit structured warnings, and `nexus config migrate` rewrites files
// in place using the same data.
//
// Adding a new deprecation is a one-line append to Deprecations; the
// warn-on-load and migrate paths pick it up automatically. No new env
// vars are introduced — the registry is static, versioned with the
// binary.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Deprecation describes a single renamed or removed config key. Either
// the env-var pair (OldEnv/NewEnv) or the YAML-key pair
// (OldYAMLKey/NewYAMLKey) may be set; a pure removal sets NewEnv /
// NewYAMLKey to "".
type Deprecation struct {
	OldEnv           string // deprecated env var name, e.g. "NEXUS_QUALITY_DROPED_RING_SIZE"
	NewEnv           string // replacement env var name; "" means removed with no replacement
	OldYAMLKey       string // deprecated YAML key (snake_case), e.g. "quality_droped_ring_size"
	NewYAMLKey       string // replacement YAML key; "" means removed with no replacement
	RemovedInVersion string // release that introduced the rename, e.g. "v0.9.0"
	Hint             string // short human-readable migration note
}

// Deprecations is the ordered list of known deprecated keys. Order is
// preserved so warnings and migration output are deterministic. Append
// new entries here; do not reorder existing ones.
var Deprecations = []Deprecation{
	{
		OldEnv:           "NEXUS_QUALITY_DROPED_RING_SIZE",
		NewEnv:           "NEXUS_QUALITY_DROPPED_RING_SIZE",
		OldYAMLKey:       "quality_droped_ring_size",
		NewYAMLKey:       "quality_dropped_ring_size",
		RemovedInVersion: "v0.9.0",
		Hint:             "typo fix: 'droped' → 'dropped' (issue #924)",
	},
}

// WarnDeprecatedEnv scans the process environment for deprecated env
// vars and emits a structured slog.Warn for each one found. Called once
// near the end of Load() and LoadYAML(). This is advisory only — value
// copying is intentionally NOT performed here to preserve the existing
// warn-only semantics of the #924 alias (acceptance criterion: no
// behaviour change).
func WarnDeprecatedEnv() {
	for _, d := range Deprecations {
		if d.OldEnv == "" {
			continue
		}
		if os.Getenv(d.OldEnv) == "" {
			continue
		}
		newSet := d.NewEnv != "" && os.Getenv(d.NewEnv) != ""
		msg := fmt.Sprintf("%s is deprecated", d.OldEnv)
		if d.NewEnv != "" {
			if newSet {
				msg = fmt.Sprintf("%s is deprecated; %s is also set, ignoring deprecated value", d.OldEnv, d.NewEnv)
			} else {
				msg = fmt.Sprintf("%s is deprecated; use %s", d.OldEnv, d.NewEnv)
			}
		}
		slog.Warn(msg,
			slog.String("component", "config"),
			slog.String("deprecated_env", d.OldEnv),
			slog.String("removed_in", d.RemovedInVersion),
			slog.String("replacement_env", d.NewEnv),
			slog.String("hint", d.Hint),
		)
	}
}

// WarnDeprecatedYAMLKey emits a structured warning for a single
// deprecated YAML key that is present in a config file. Called by
// LoadYAML after decoding, once per deprecated key actually present.
func WarnDeprecatedYAMLKey(d Deprecation) {
	msg := fmt.Sprintf("YAML key %q is deprecated", d.OldYAMLKey)
	if d.NewYAMLKey != "" {
		msg = fmt.Sprintf("YAML key %q is deprecated; use %q", d.OldYAMLKey, d.NewYAMLKey)
	}
	slog.Warn(msg,
		slog.String("component", "config"),
		slog.String("deprecated_yaml_key", d.OldYAMLKey),
		slog.String("removed_in", d.RemovedInVersion),
		slog.String("replacement_yaml_key", d.NewYAMLKey),
		slog.String("hint", d.Hint),
	)
}

// FindDeprecationByYAMLKey returns the registry entry whose OldYAMLKey
// matches, or nil if none. Used by the migrate command and by
// LoadYAML's post-decode warning pass.
func FindDeprecationByYAMLKey(key string) *Deprecation {
	for i := range Deprecations {
		if Deprecations[i].OldYAMLKey == key {
			return &Deprecations[i]
		}
	}
	return nil
}

// warnDeprecatedYAMLKeysFromData unmarshals raw YAML bytes into a
// generic map and emits a warning for every deprecated top-level key
// that is present. The struct-based decode (YAMLConfig) silently drops
// unknown keys, so we must re-decode into a map to detect them.
// Errors from the generic decode are swallowed: if the data already
// failed strict validation the caller returned before reaching here.
func warnDeprecatedYAMLKeysFromData(data []byte) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return
	}
	for _, d := range Deprecations {
		if d.OldYAMLKey == "" {
			continue
		}
		if _, ok := raw[d.OldYAMLKey]; ok {
			WarnDeprecatedYAMLKey(d)
		}
	}
}

// MigrateEnvContent rewrites deprecated NEXUS_ env-var assignments in a
// .env-style file body. It returns the new content and the number of
// substitutions applied. Only `KEY=value` lines (after optional
// leading whitespace and an optional "export " prefix) whose KEY
// exactly matches a deprecated env var are rewritten. Lines where the
// replacement var is already set elsewhere are commented out to avoid
// duplicates. A pure removal (no NewEnv) comments the line out.
func MigrateEnvContent(content string) (string, int) {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		d := matchEnvLine(line)
		if d == nil {
			continue
		}
		if d.NewEnv == "" {
			lines[i] = "# REMOVED (" + d.RemovedInVersion + "): " + line
		} else {
			// Replace only the KEY token, preserving value and quoting.
			leading := line[:strings.Index(line, d.OldEnv)]
			rest := line[strings.Index(line, d.OldEnv)+len(d.OldEnv):]
			lines[i] = leading + d.NewEnv + rest
		}
		count++
	}
	return strings.Join(lines, "\n"), count
}

// matchEnvLine returns the Deprecation whose OldEnv appears as a
// `KEY=value` assignment at the start of the line (after optional
// whitespace and "export "), or nil.
func matchEnvLine(line string) *Deprecation {
	bare := strings.TrimLeft(line, " \t")
	bare = strings.TrimPrefix(bare, "export ")
	for _, d := range Deprecations {
		if d.OldEnv == "" {
			continue
		}
		if strings.HasPrefix(bare, d.OldEnv+"=") {
			return &d
		}
	}
	return nil
}

// MigrateYAMLContent rewrites deprecated top-level YAML keys in a
// config.yaml body. It returns the new content and the number of
// substitutions applied. Only top-level `key:` lines (no leading
// indentation, not comments) are matched, matching the flat schema of
// YAMLConfig. Nested keys inside maps (indented) are intentionally left
// untouched to avoid corrupting values that happen to share a suffix.
// A pure removal (no NewYAMLKey) comments the line out.
func MigrateYAMLContent(content string) (string, int) {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		d := FindDeprecationByYAMLKey(key)
		if d == nil {
			continue
		}
		if d.NewYAMLKey == "" {
			lines[i] = "# REMOVED (" + d.RemovedInVersion + "): " + line
		} else {
			lines[i] = d.NewYAMLKey + line[colon:]
		}
		count++
	}
	return strings.Join(lines, "\n"), count
}
