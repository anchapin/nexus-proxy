// Package config — file.go implements a YAML config file loader using
// gopkg.in/yaml.v3, which properly handles YAML anchors and aliases
// (issue #424).
//
// The output is a flat map of "section.key" -> string so that the
// existing resolve* helpers in config.go can parse the string values
// (strconv for int/float, time.ParseDuration for durations, etc.)
// without needing to know the YAML type system.
//
// String values undergo env-variable expansion (${VAR}, $VAR) at parse
// time so secrets can stay in env vars without committing them to the
// config file.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadFile parses the YAML config file at path into a flat map of
// "section.key" -> string. A missing file returns (nil, nil) so callers
// can treat absence as "no file configured" without sprinkling os.Stat
// checks around the package. Malformed input returns a parse error.
func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return ParseYAML(string(data))
}

// ParseYAML exposes the parser for tests that want to feed a string
// directly instead of writing a temp file.
func ParseYAML(content string) (map[string]string, error) {
	// First pass: unmarshal into a generic map.
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(content), &raw); err != nil {
		return nil, fmt.Errorf("YAML parse error: %w", err)
	}

	// Second pass: pre-validate indentation constraints before flattening.
	// We re-parse line-by-line to enforce the one-level-of-indentation rule
	// that the old parser enforced: no nested sections and no indented KVs
	// without a section header above them.
	if err := validateIndentation(content); err != nil {
		return nil, err
	}

	// Third pass: flatten "section.key" -> string with env expansion.
	out := make(map[string]string)
	flatten("", raw, out)

	// Env-variable expansion on string values.
	for k, v := range out {
		out[k] = expandEnv(v)
	}

	return out, nil
}

// validateIndentation enforces the one-level-of-indentation rule:
// - At most one level of section nesting is allowed (section: -> key:).
// - Indented keys without a section header above them are rejected.
func validateIndentation(content string) error {
	type state int
	const (
		stateTop state = iota
		stateSection
	)

	var (
		currentState = stateTop
		lineNum      int
	)

	for _, raw := range strings.Split(content, "\n") {
		lineNum++
		// Strip trailing CR but preserve leading whitespace for indent detection.
		line := strings.TrimRight(raw, "\r")

		// Skip blank lines and full-line comments.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Compute indentation level BEFORE trimming.
		leading := len(line) - len(strings.TrimLeft(line, " \t"))

		switch leading {
		case 0:
			// Top-level line.
			if strings.HasSuffix(trimmed, ":") {
				// Section header at top level.
				currentState = stateSection
			} else {
				// Top-level key-value is fine.
				currentState = stateTop
			}
		case 1:
			// Indented under a section (1 space is always valid indentation under a section).
			if currentState != stateSection {
				return fmt.Errorf("line %d: indented key has no section header above it", lineNum)
			}
		default:
			// leading >= 2: could be deeply indented key under a section (valid)
			// or a nested section header (invalid). Reject only if this is a section header
			// (ends with ":") because that would be nesting a section inside a section.
			if currentState == stateSection && strings.HasSuffix(trimmed, ":") {
				return fmt.Errorf("line %d: nested sections are not supported", lineNum)
			}
			// Leading >= 2 with no active section is also invalid.
			if currentState != stateSection {
				return fmt.Errorf("line %d: indented key has no section header above it", lineNum)
			}
		}
	}

	return nil
}

// flatten recurses through the raw YAML map and produces flat
// "section.key" -> string entries in out. Section headers (maps with
// no non-key sub-keys) are treated as section prefixes.
func flatten(prefix string, node map[string]any, out map[string]string) {
	for k, v := range node {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]any:
			// Check if this is a "section header" (all values are scalar)
			// or a nested config block. A nested block has at least one
			// non-map value; a section header is all maps.
			isSection := true
			for _, child := range val {
				if _, ok := child.(map[string]any); ok {
					isSection = false
					break
				}
			}
			if isSection && len(val) > 0 {
				// Section header: descend with prefix but do NOT emit a
				// string entry for this key (it has no scalar value).
				flatten(key, val, out)
			} else {
				// Nested config block: flatten into the current prefix.
				flatten(key, val, out)
			}
		case []any:
			// Lists (e.g., dsl_formatted_patterns) are serialised as
			// comma-separated strings so the existing getEnvRegexps helper
			// can parse them identically to comma-separated env var values.
			out[key] = joinYAMLList(val)
		case nil:
			// Null values are skipped.
		default:
			// Scalar types: bool, int, float64, string.
			out[key] = fmt.Sprintf("%v", v)
		}
	}
}

// joinYAMLList converts a YAML list to a comma-separated string,
// quoting elements that contain commas themselves.
func joinYAMLList(list []any) string {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		s := fmt.Sprintf("%v", item)
		if strings.Contains(s, ",") {
			s = `"` + s + `"`
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

// expandEnv expands ${VAR} and $VAR references in value. Unknown
// variables expand to the empty string. Escaping with \$ keeps a literal
// dollar sign in the output.
func expandEnv(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	i := 0
	for i < len(value) {
		c := value[i]
		// \$ -> literal $
		if c == '\\' && i+1 < len(value) && value[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// ${VAR}
		if i+1 < len(value) && value[i+1] == '{' {
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				b.WriteByte(c)
				i++
				continue
			}
			name := value[i+2 : i+2+end]
			b.WriteString(os.Getenv(name))
			i += 2 + end + 1
			continue
		}
		// $VAR — name is [A-Za-z0-9_]+
		j := i + 1
		for j < len(value) && isEnvNameByte(value[j]) {
			j++
		}
		if j == i+1 {
			b.WriteByte(c)
			i++
			continue
		}
		name := value[i+1 : j]
		b.WriteString(os.Getenv(name))
		i = j
	}
	return b.String()
}

func isEnvNameByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// DiscoverConfigFile returns the first existing config file path in the
// current working directory, checking nexus.yaml, nexus.yml, and
// nexus.json in that order. Returns "" when none of them is present so
// callers can treat the result as "no file configured" without an
// extra os.Stat.
//
// The function is intentionally CWD-only — production deployments
// should pass an explicit path via --config / NEXUS_CONFIG so the boot
// is deterministic across shells.
func DiscoverConfigFile() string {
	for _, name := range []string{"nexus.yaml", "nexus.yml", "nexus.json"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return ""
}

// configPathFlag is set by main.go when --config is on the command
// line. Load() prefers this over NEXUS_CONFIG and CWD auto-discovery so
// operators can pin a config file regardless of where the binary was
// launched from.
//
// It is intentionally a package-level variable rather than a Load()
// parameter: the project's existing call sites (and external embedders
// of this package) stay unchanged, and the surface area is one line.
var configPathFlag string

// SetConfigPathOverride pins the config file path that Load() will
// read. Call once from main() before Load() when --config is set. Pass
// "" to clear an earlier override.
func SetConfigPathOverride(path string) { configPathFlag = path }

// ConfigPathOverride returns the path previously set with
// SetConfigPathOverride, or "" if none.
func ConfigPathOverride() string { return configPathFlag }
