// Subcommand: `nexus judge stats`. Reads the routing-confidence SQLite store
// and prints per-category confidence statistics. The tool is read-only and
// safe to run while the proxy is live.
//
// This file is the CLI adapter; the actual store logic lives in
// internal/router/confidence_sqlite.go.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/router"
)

const judgeStatsUsage = `nexus judge stats — adaptive routing confidence statistics.

Usage:
  nexus judge stats [--json]

Flags:
  --json    Emit machine-readable JSON instead of a human-readable table.

The tool reads NEXUS_ROUTING_CONFIDENCE_DB (or its default path) and prints
per-category confidence statistics gathered within the configured sliding
window. Exit code is 0 on success, 1 on error.
`

func runJudgeStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexus judge stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, judgeStatsUsage) }

	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of a human-readable table")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "nexus judge stats: config: %v\n", err)
		return 1
	}

	if cfg.RoutingConfidenceDB == "" {
		fmt.Fprintf(stderr, "nexus judge stats: NEXUS_ROUTING_CONFIDENCE_DB is not set\n")
		return 1
	}

	store, err := router.OpenConfidenceStore(router.ConfidenceConfig{
		Path:       cfg.RoutingConfidenceDB,
		MinSamples: cfg.RoutingConfidenceMinSamples,
		Window:     cfg.RoutingConfidenceWindow,
	})
	if err != nil {
		fmt.Fprintf(stderr, "nexus judge stats: open confidence store %q: %v\n", cfg.RoutingConfidenceDB, err)
		return 1
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			slog.Warn("judge stats: close store", "err", cerr)
		}
	}()

	stats := store.WindowStats()

	if asJSON {
		if err := renderJudgeStatsJSON(stats, cfg, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus judge stats: %v\n", err)
			return 1
		}
	} else {
		if err := renderJudgeStatsTable(stats, cfg, stdout); err != nil {
			fmt.Fprintf(stderr, "nexus judge stats: %v\n", err)
			return 1
		}
	}
	return 0
}

type judgeStatsRow struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Samples    int     `json:"samples"`
}

type judgeStatsOutput struct {
	Floor      float64         `json:"floor"`
	Ceiling    float64         `json:"ceiling"`
	MinSamples int             `json:"min_samples"`
	Window     string          `json:"window"`
	Categories []judgeStatsRow `json:"categories"`
}

func renderJudgeStatsJSON(stats []router.CategoryStats, cfg config.Config, w io.Writer) error {
	rows := make([]judgeStatsRow, len(stats))
	for i, s := range stats {
		conf := s.Confidence
		if s.Samples < cfg.RoutingConfidenceMinSamples {
			conf = 0
		}
		rows[i] = judgeStatsRow{
			Category:   s.Category,
			Confidence: conf,
			Samples:    s.Samples,
		}
	}
	out := judgeStatsOutput{
		Floor:      cfg.RoutingConfidenceFloor,
		Ceiling:    cfg.RoutingConfidenceCeiling,
		MinSamples: cfg.RoutingConfidenceMinSamples,
		Window:     cfg.RoutingConfidenceWindow.String(),
		Categories: rows,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderJudgeStatsTable(stats []router.CategoryStats, cfg config.Config, w io.Writer) error {
	fmt.Fprintln(w, "Nexus Proxy — Adaptive Routing Confidence")
	fmt.Fprintf(w, "Floor: %.2f   Ceiling: %.2f   Min-samples: %d   Window: %s\n",
		cfg.RoutingConfidenceFloor,
		cfg.RoutingConfidenceCeiling,
		cfg.RoutingConfidenceMinSamples,
		cfg.RoutingConfidenceWindow,
	)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "CATEGORY\tCONFIDENCE\tSAMPLES\tWINDOW")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	minSamples := cfg.RoutingConfidenceMinSamples
	for _, s := range stats {
		var confStr string
		if s.Samples < minSamples {
			confStr = "—"
		} else {
			confStr = fmt.Sprintf("%.2f", s.Confidence)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n",
			s.Category, confStr, s.Samples, cfg.RoutingConfidenceWindow)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("judge stats: flush table: %w", err)
	}
	return nil
}
