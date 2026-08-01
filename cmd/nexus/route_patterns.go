package main

import (
	"fmt"
	"io"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/router"
)

const routeUsage = `nexus route — inspect DSL routing patterns.

Usage:
  nexus route patterns    List auto-promoted DSL patterns with sample counts and confidence.

Exit code is always 0.
`

func runRoute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, routeUsage)
		return 0
	}
	switch args[0] {
	case "patterns":
		return runRoutePatterns(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "nexus route: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(stderr, routeUsage)
		return 0
	}
}

func runRoutePatterns(_ []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "nexus route patterns: config: %v\n", err)
		return 0
	}

	if cfg.DSLPromotionMinSamples <= 0 && cfg.DSLPromotionConfidence <= 0 && cfg.DSLPromotionInterval <= 0 {
		fmt.Fprintln(stdout, "DSL auto-promotion is disabled (all thresholds are zero).")
		return 0
	}

	promoterCfg := router.PromoterConfig{
		Path:       config.DefaultRoutingConfidenceDBPath(),
		MinSamples: cfg.DSLPromotionMinSamples,
		Confidence: cfg.DSLPromotionConfidence,
		Interval:   cfg.DSLPromotionInterval,
	}
	promoter, err := router.NewPatternPromoter(promoterCfg)
	if err != nil {
		fmt.Fprintf(stderr, "nexus route patterns: %v\n", err)
		return 0
	}
	defer promoter.Close()

	patterns := promoter.PromotedPatterns()
	if len(patterns) == 0 {
		fmt.Fprintln(stdout, "No promoted DSL patterns.")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Promoted patterns appear when SLM decisions accumulate and")
		fmt.Fprintln(stdout, "n-grams meet the min-samples and confidence thresholds.")
		return 0
	}

	fmt.Fprintf(stdout, "%d promoted DSL pattern(s):\n\n", len(patterns))
	fmt.Fprintf(stdout, "%-50s  %-8s  %7s  %10s\n", "PATTERN", "ROUTE", "SAMPLES", "CONFIDENCE")
	fmt.Fprintln(stdout, "----------------------------------------------------------------------------------------")
	for _, pp := range patterns {
		fmt.Fprintf(stdout, "%-50s  %-8s  %7d  %9.1f%%\n",
			truncate(pp.Pattern, 50), string(pp.Route), pp.Samples, pp.Confidence*100)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
