// Package config — file.go implements a minimal YAML loader for nexus.yaml.
//
// The parser supports a deliberately tiny subset of YAML so we can stay
// stdlib-only per the PRD's "zero-dependency" rule (issue #31):
//
//   - blank lines are skipped
//   - full-line comments ("# ...") are skipped
//   - trailing "# ..." comments after a value are stripped (only outside
//     quoted strings, so `key: "value with # inside"` is preserved)
//   - section headers: lines ending with ":" and no value, e.g. `server:`
//   - top-level key-value pairs: `key: value`
//   - indented key-value pairs under the most recent section:
//     `  key: value`
//   - quoted strings (double or single): surrounding quotes are stripped,
//     interior contents are preserved verbatim
//   - type inference is *implicit*: int / float / bool / duration strings
//     are passed through as their source text because the existing
//     resolve* helpers parse them with strconv/time.ParseDuration. The
//     file's YAML literal "30s" becomes the string "30s" — exactly what
//     env-var form requires.
//
// It does NOT support:
//
//   - YAML anchors / aliases
//   - flow sequences ({a, b} or [a, b])
//   - multi-document streams
//   - nested sections (only one level of `section:` then `  key:`)
//   - list / array values
//   - multi-line scalars (| and >)
//
// The output is a flat map of "section.key" -> "string". String values
// undergo env-variable expansion (${VAR}, $VAR) at parse time so secrets
// can stay in env vars without committing them to nexus.yaml.
package config

import (
	"fmt"
	"os"
	"strings"
)

// LoadFile parses the YAML config file at path into a flat map of
// "section.key" -> string. A missing file returns (nil, nil) so callers
// can treat absence as "no file configured" without sprinkling os.Stat
// checks around the package. Malformed input returns a parse error
// with the offending line number.
//
// The parser does NOT do env expansion for unknown keys — it always
// returns what it parsed. Translation from "section.key" to env-var
// names is handled in config.go (see configKeys).
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
// directly instead of writing a temp file. The public surface here is
// deliberately tiny: LoadFile is the only intended entry point for
// production code.
func ParseYAML(content string) (map[string]string, error) {
	out := make(map[string]string)
	var section string

	for lineNum, raw := range strings.Split(content, "\n") {
		lineNum++ // 1-indexed for friendlier error messages

		// Strip trailing CR for Windows-authored files.
		line := strings.TrimRight(raw, "\r")

		// Skip blank lines and full-line comments (after trim of leading
		// whitespace, since `# indented comment` is also a comment).
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Detect indentation. Any leading whitespace means the line is
		// part of the current section.
		leading := len(line) - len(strings.TrimLeft(line, " \t"))
		isIndented := leading > 0

		// Strip a trailing "# comment" portion, respecting quotes.
		cleaned, _ := stripTrailingComment(line)
		key, value, hasValue, err := splitKV(cleaned)
		if err != nil {
			return nil, fmt.Errorf("config: line %d: %w", lineNum, err)
		}
		if key == "" {
			return nil, fmt.Errorf("config: line %d: empty key", lineNum)
		}

		if !isIndented {
			if !hasValue {
				// Top-level section header. Resets the active section so
				// the next indented KV lands under it.
				section = key
				continue
			}
			// Top-level flat KV pair (rare, but valid).
			section = ""
			out[key] = value
			continue
		}

		// Indented line.
		if !hasValue {
			// Nested section header — we don't support nesting, so
			// reject loudly instead of silently mis-grouping values.
			return nil, fmt.Errorf("config: line %d: nested sections are not supported (got %q)", lineNum, key)
		}
		if section == "" {
			return nil, fmt.Errorf("config: line %d: indented key %q has no section header above it", lineNum, key)
		}
		out[section+"."+key] = value
	}

	// Apply env-variable expansion once, after the full file is parsed,
	// so a single value can reference multiple env vars (e.g. a future
	// user mashing $HOST:$PORT into one field) without us worrying
	// about ordering.
	for k, v := range out {
		out[k] = expandEnv(v)
	}

	return out, nil
}

// expandEnv expands ${VAR} and $VAR references in value. Unknown
// variables expand to the empty string. Escaping with \$ keeps a literal
// dollar sign in the output (handy when the operator wants to write a
// regex like `$1` in a future prompt).
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
				// Unmatched brace — emit the literal so the operator
				// notices in the boot log instead of silently dropping
				// characters.
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
			// Lone "$" — keep literal.
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

// stripTrailingComment removes a "# comment" portion at the end of
// line, but only when the "#" sits outside a quoted string. The bool
// return signals whether anything was stripped (handy for callers that
// want to log it; we currently use it only for tests).
func stripTrailingComment(line string) (string, bool) {
	var inQuote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
		case '#':
			return strings.TrimRight(line[:i], " \t"), true
		}
	}
	return line, false
}

// splitKV parses "key: value" / "key:" / "key" into (key, value,
// hasValue). The colon is the first unquoted ":" in the line. Values
// are unquoted (surrounding " or ' are stripped) but otherwise preserved
// verbatim — type inference happens downstream in the resolve* helpers.
func splitKV(line string) (key, value string, hasValue bool, err error) {
	inQuote := byte(0)
	colonIdx := -1
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
		case ':':
			if colonIdx == -1 {
				colonIdx = i
			}
		}
	}
	if colonIdx == -1 {
		return "", "", false, fmt.Errorf("missing ':' in %q", line)
	}
	key = strings.TrimSpace(line[:colonIdx])
	rest := strings.TrimSpace(line[colonIdx+1:])
	if key == "" {
		return "", "", false, fmt.Errorf("empty key in %q", line)
	}
	if rest == "" {
		return key, "", false, nil
	}
	return key, unquote(rest), true, nil
}

// unquote strips a matched pair of surrounding quote characters (either
// both '"' or both '\”). Mismatched / partial quotes are returned
// verbatim — the parse error surfaces elsewhere (missing closing quote
// doesn't actually break our state machine, but it's also not a valid
// YAML construct, so tests will assert the literal survives).
func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
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
