package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/router"
)

const routingPreviewUsage = `nexus routing-preview — test routing decisions without starting the server.

Usage:
  nexus routing-preview [flags] [prompt...]
  nexus routing-preview [flags] --stdin

Flags:
  --stdin    Read prompt(s) from stdin, one per line.
  --explain  Append the bias note that would be injected into the SLM
             system prompt (if any).

Exit code is always 0. Errors are printed to stderr and the command
continues to the next prompt.

Examples:
  nexus routing-preview "refactor this CSS"
  nexus routing-preview --stdin < prompts.txt
  echo "fix bug" | nexus routing-preview --stdin
  nexus routing-preview --explain "debug this crash"
`

func runRoutingPreview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus routing-preview", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, routingPreviewUsage) }

	var fromStdin bool
	var explain bool
	fs.BoolVar(&fromStdin, "stdin", false, "read prompt(s) from stdin")
	fs.BoolVar(&explain, "explain", false, "append bias note explanation")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "nexus routing-preview: config: %v\n", err)
		return 0
	}

	httpClient := &http.Client{}
	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, httpClient)
	slm.ConfidenceFloor = cfg.RoutingConfidenceFloor
	slm.ConfidenceCeiling = cfg.RoutingConfidenceCeiling

	planner := &router.Planner{
		SLM:                  slm,
		FusionPatterns:       cfg.DSLFusionPatterns,
		FormattingRegex:      cfg.DSLFormattingPatterns,
		LocalPatternsRegex:   cfg.DSLLocalPatterns,
		UnicodePatternsRegex: cfg.DSLUnicodePatterns,
		ConfidenceThreshold:  cfg.SLMConfidenceThreshold,
		SLMTokenHint:         cfg.SLMTokenHint,
	}

	var prompts []string
	if fromStdin {
		prompts, err = readStdinLines(os.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "nexus routing-preview: stdin: %v\n", err)
			return 0
		}
		if len(prompts) == 0 {
			return 0
		}
	} else {
		prompts = fs.Args()
		if len(prompts) == 0 {
			fs.Usage()
			return 1
		}
	}

	ctx := context.Background()

	for _, prompt := range prompts {
		if prompt == "" {
			continue
		}
		dec := planner.Plan(router.PlanRequest{
			Prompt:          prompt,
			GuardrailBudget: cfg.TokenGuardrail,
			GuardrailSource: "static-fallback",
			Context:         ctx,
		})
		reason := decisionReason(dec, cfg.DSLFusionPatterns, cfg.DSLFormattingPatterns, cfg.DSLLocalPatterns, cfg.DSLUnicodePatterns, prompt)
		line := formatDecision(dec, reason)
		if explain {
			if note := explainDecision(dec, slm); note != "" {
				line = line + " " + note
			}
		}
		fmt.Fprintln(stdout, line)
	}

	return 0
}

func readStdinLines(r io.Reader) ([]string, error) {
	var lines []string
	bufr := new(bytes.Buffer)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			bufr.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	for _, line := range strings.Split(bufr.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func decisionReason(dec router.Decision, fusion, formatting, local, unicode []*regexp.Regexp, prompt string) string {
	switch dec.Source {
	case router.SourceGuardrail:
		return "guardrail:vram"
	case router.SourceDSL:
		kw := matchedDSLKeyword(prompt, dec.Route, fusion, formatting, local, unicode)
		return "dsl:" + kw
	case router.SourceSLM:
		return "slm"
	case router.SourceSLMError:
		return "slm-error:" + dec.Reason
	case router.SourceEscalation:
		return "slm-no-client"
	case router.SourceSLMEscalation:
		return "slm-low-confidence"
	default:
		return "slm"
	}
}

func fusionPatterns(cfg []*regexp.Regexp) []*regexp.Regexp {
	if len(cfg) == 0 {
		return router.DefaultFusionPatterns
	}
	return cfg
}

func formattingPatterns(cfg []*regexp.Regexp) []*regexp.Regexp {
	if len(cfg) == 0 {
		return router.DefaultFormattingPatterns
	}
	return cfg
}

func localPatterns(cfg []*regexp.Regexp) []*regexp.Regexp {
	if len(cfg) == 0 {
		return router.DefaultLocalPatterns
	}
	return cfg
}

func unicodePatterns(cfg []*regexp.Regexp) []*regexp.Regexp {
	if len(cfg) == 0 {
		return router.DefaultUnicodePatterns
	}
	return cfg
}

func matchedDSLKeyword(prompt string, route router.Route, fusion, formatting, local, unicode []*regexp.Regexp) string {
	lower := toUnicodeLower(prompt)

	if route == router.RouteFusion {
		kw := findMatchedKeyword(lower, fusionPatterns(fusion))
		if kw != "" {
			return kw
		}
	}
	kw := findMatchedKeyword(lower, formattingPatterns(formatting))
	if kw != "" {
		return kw
	}
	kw = findMatchedKeyword(lower, localPatterns(local))
	if kw != "" {
		return kw
	}
	kw = findMatchedKeyword(lower, unicodePatterns(unicode))
	if kw != "" {
		return kw
	}
	return "unknown"
}

func findMatchedKeyword(lower string, patterns []*regexp.Regexp) string {
	for _, re := range patterns {
		if re == nil {
			continue
		}
		m := re.FindStringIndex(lower)
		if m != nil {
			start, end := m[0], m[1]
			if start > 0 && !isWordBoundary(lower[start-1]) {
				continue
			}
			if end < len(lower) && !isWordBoundary(lower[end]) {
				continue
			}
			return strings.ToLower(lower[m[0]:m[1]])
		}
	}
	return ""
}

func toUnicodeLower(s string) string {
	if !hasUpperUnicode(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func hasUpperUnicode(s string) bool {
	for _, r := range s {
		if r != unicode.ToLower(r) {
			return true
		}
	}
	return false
}

func isWordBoundary(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == '.' || c == '!' || c == '?' || c == ':' || c == ';' || c == '(' || c == ')' || c == '[' || c == ']' || c == '{' || c == '}' || c == '"' || c == '\'' || c == '`' || c == '/' || c == '\\' || c == '|' || c == '&' || c == '+' || c == '-' || c == '*' || c == '%' || c == '^' || c == '=' || c == '<' || c == '>' || c == '@' || c == '#' || c == '$' || c == '~'
}

func formatDecision(dec router.Decision, reason string) string {
	return fmt.Sprintf("ROUTE=%s REASON=%q", dec.Route, reason)
}

func explainDecision(dec router.Decision, slm *router.SLMClient) string {
	if dec.Source != router.SourceSLM && dec.Source != router.SourceSLMEscalation {
		return ""
	}
	floor := slm.ConfidenceFloor
	if floor <= 0 {
		floor = router.DefaultConfidenceFloor
	}
	ceiling := slm.ConfidenceCeiling
	if ceiling <= 0 {
		ceiling = router.DefaultConfidenceCeiling
	}
	switch {
	case dec.Confidence < floor:
		return fmt.Sprintf("BIAS=negative (confidence %.2f < floor %.2f)", dec.Confidence, floor)
	case dec.Confidence > ceiling:
		return fmt.Sprintf("BIAS=positive (confidence %.2f > ceiling %.2f)", dec.Confidence, ceiling)
	default:
		return fmt.Sprintf("BIAS=neutral (confidence %.2f within band)", dec.Confidence)
	}
}
