#!/usr/bin/env node

const { execSync } = require("child_process");
const fs = require("fs");

const MAX_PER_WAVE = 3;

// Patterns that indicate an issue is already resolved in develop.
const RESOLVED_PATTERNS = [
  /already\s+resolved/i,
  /fixed\s+in\s+develop/i,
  /resolved\s+in\s+develop/i,
  /resolved\s+by\s+#[0-9]+/i,
  /no\s+code\s+change\s+needed/i,
  /nothing\s+to\s+do/i,
  /wontfix/i,
];

// Files that are frequently touched by gofmt/struct-alignment even when not
// explicitly mentioned in issue text. Only injected under the `legacy`
// collision strategy. The default strategy (`go-packages`) does NOT inject
// these — it derives conflicts from Go package directories instead, so that
// issues touching independent packages can run in parallel.
const HIGH_COLLISION_FILES = [
  "cmd/nexus/main.go",
  "cmd/nexus/main_test.go",
];

const VALID_STRATEGIES = ["none", "go-packages", "legacy"];

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const args = argv.slice(2);
  const opts = {
    collisionStrategy: "go-packages",
    dryRun: false,
    help: false,
    file: null,
  };

  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === "--dry-run" || a === "-n") {
      opts.dryRun = true;
    } else if (a === "--collision-strategy") {
      opts.collisionStrategy = args[++i];
    } else if (a.startsWith("--collision-strategy=")) {
      opts.collisionStrategy = a.slice("--collision-strategy=".length);
    } else if (a === "--help" || a === "-h") {
      opts.help = true;
    } else if (!a.startsWith("-")) {
      opts.file = a;
    }
  }

  if (!VALID_STRATEGIES.includes(opts.collisionStrategy)) {
    process.stderr.write(
      `Error: invalid --collision-strategy '${opts.collisionStrategy}'. ` +
        `Valid values: ${VALID_STRATEGIES.join(", ")}\n`
    );
    process.exit(2);
  }

  return opts;
}

function readInput(fileArg) {
  if (fileArg) {
    return JSON.parse(fs.readFileSync(fileArg, "utf8"));
  }
  // Read from fd 0 directly — /dev/stdin is unreliable on pipes (Node ≥ 20
  // can raise ENXIO due to a close/open race on the symlinked FIFO).
  return JSON.parse(fs.readFileSync(0, "utf8"));
}

// ---------------------------------------------------------------------------
// File / package reference extraction
// ---------------------------------------------------------------------------

// Returns the Go "package directory" for a file path. For `.go` files the
// package is the containing directory (e.g. internal/handlers/chat.go →
// internal/handlers). For non-Go files the file path itself is returned so
// that exact-file conflicts still apply (docs, scripts, configs).
function goPackageOf(filePath) {
  if (filePath.endsWith(".go")) {
    const idx = filePath.lastIndexOf("/");
    return idx === -1 ? "" : filePath.substring(0, idx);
  }
  return filePath;
}

function extractFileRefs(text, collisionStrategy) {
  const files = new Set();
  // file -> "explicit" (mentioned in issue text) | "collision" (auto-injected)
  const sources = {};

  // legacy strategy: inject high-collision files into every issue so that
  // issues touching the central wiring file are serialised.
  if (collisionStrategy === "legacy") {
    for (const f of HIGH_COLLISION_FILES) {
      files.add(f);
      sources[f] = "collision";
    }
  }

  if (!text) return { files: [...files], sources };

  // Match file paths with optional line numbers, supported extensions,
  // and common delimiters (backticks, quotes, brackets, or whitespace).
  const pathPatterns = [
    // Backtick, quote, or bracket delimited: `dir/subdir/file.ext:123`
    /[`'"\[\s]([a-zA-Z0-9_./-]+\/[a-zA-Z0-9_./-]+\.[a-z]{2,4})(?::\d+)?[`'"\]\s]/g,
    // Colon-separated with line number: internal/auth/auth.go:42
    /(?<![a-zA-Z0-9_/.-])([a-zA-Z0-9_./-]+\/[a-zA-Z0-9_./-]+\.[a-z]{2,4}):(\d+)/g,
    // Bare quoted or backtick path: "cmd/nexus/main.go"
    /[`'"]([a-zA-Z0-9_./-]+\.[a-z]{2,4})[`'"]/g,
    // Bare path containing a directory separator and file extension with no
    // surrounding delimiters (e.g. issue body is just "internal/foo/bar.go").
    /\b([a-zA-Z0-9_-]+\/[a-zA-Z0-9_./-]+\.[a-z]{2,4})\b/g,
    // Known-prefix paths under well-known directories.
    /\b([a-zA-Z0-9_./-]+\/(?:src|lib|test|tests|pkg|cmd|internal|osimflow|bin|docs|scripts|app|modules|components)\/[a-zA-Z0-9_./-]+\.[a-z]{2,4})\b/g,
  ];

  for (const pat of pathPatterns) {
    let m;
    while ((m = pat.exec(text)) !== null) {
      const f = m[1];
      if (!f.includes("http") && !f.includes("://") && f.length > 3) {
        files.add(f);
        sources[f] = "explicit";
      }
    }
  }

  return { files: [...files], sources };
}

function extractModuleRefs(text) {
  if (!text) return [];
  const modules = new Set();

  const patterns = [
    /import\s+.+\s+from\s+['"](\.?\.?\/[^'"]+)['"]/g,
    /from\s+([a-zA-Z0-9_.]+)\s+import/g,
    /require\(['"](\.?\.?\/[^'"]+)['"]\)/g,
    /use\s+([a-zA-Z0-9_:]+::[a-zA-Z0-9_:]+)/g,
  ];

  for (const pat of patterns) {
    let m;
    while ((m = pat.exec(text)) !== null) {
      modules.add(m[1]);
    }
  }

  return [...modules];
}

// ---------------------------------------------------------------------------
// Already-resolved detection
// ---------------------------------------------------------------------------

// Returns true if the issue body mentions it is already resolved, or if the
// develop branch already contains a commit referencing this issue number.
// The git check is opt-in via WAVE_PLANNER_CHECK_GIT_HISTORY to avoid false
// positives in test environments where issue numbers may collide with real
// commit messages.
function isAlreadyResolved(issue) {
  const body = issue.body || "";
  const title = issue.title || "";
  const fullText = `${title}\n${body}`;

  // 1. Lightweight text-pattern check on the issue itself.
  for (const pat of RESOLVED_PATTERNS) {
    if (pat.test(fullText)) {
      return true;
    }
  }

  // 2. Check labels for "wontfix" / "resolved" / "duplicate".
  const labels = (issue.labels || []).map((l) =>
    typeof l === "string" ? l.toLowerCase() : (l.name || "").toLowerCase()
  );
  const resolvedLabels = [
    "wontfix",
    "resolved",
    "duplicate",
    "already-resolved",
    "no-change-required",
  ];
  if (labels.some((l) => resolvedLabels.includes(l))) {
    return true;
  }

  // 3. Git-based check: only run if WAVE_PLANNER_CHECK_GIT_HISTORY is set.
  // Looks for explicit resolution patterns like "fix #N", "close #N", "resolve #N".
  // This avoids false positives from issue numbers appearing in non-resolution contexts.
  if (process.env.WAVE_PLANNER_CHECK_GIT_HISTORY === "1") {
    try {
      // Use extended regex: matches "fix #1", "closes #1", "resolve #1", etc.
      // The pattern requires the keyword before #N, so bare "#1" won't match.
      const n = issue.number;
      const gitOut = execSync(
        `git -c grep.patternType=extended log --oneline -n 100 --grep="(fix|close|resolve|closes|resolves|fixed|resolved)\\s#${n}($|[^0-9])" origin/develop 2>/dev/null`,
        { cwd: process.cwd(), timeout: 5000, stdio: ["ignore", "pipe", "ignore"] }
      );
      if (gitOut && gitOut.toString().includes(`#${n}`)) {
        return true;
      }
    } catch (_) {
      // git log returns non-zero when no matches found — not an error.
    }
  }

  return false;
}

function analyzeIssue(issue, collisionStrategy) {
  const body = issue.body || "";
  const title = issue.title || "";
  const fullText = `${title}\n${body}`;

  const { files: fileRefs, sources: fileSources } = extractFileRefs(
    fullText,
    collisionStrategy
  );
  const moduleRefs = extractModuleRefs(fullText);

  const affectedFiles = [...new Set([...fileRefs, ...moduleRefs])];
  // Module/import references are always explicit (never collision-injected).
  for (const m of moduleRefs) {
    if (!fileSources[m]) fileSources[m] = "explicit";
  }
  const hasKnownDeps = affectedFiles.length > 0;

  return {
    number: issue.number,
    title: title,
    labels: (issue.labels || []).map((l) =>
      typeof l === "string" ? l : l.name || ""
    ),
    affected_files: affectedFiles,
    file_sources: fileSources,
    has_known_deps: hasKnownDeps,
  };
}

// ---------------------------------------------------------------------------
// Conflict graph
// ---------------------------------------------------------------------------

// Compute the set of conflict keys for an issue based on the strategy.
//   go-packages: Go-package directories (collapsed) + exact non-Go files.
//   none/legacy: exact affected_files (file-level granularity).
function conflictKeysFor(analyzed, collisionStrategy) {
  if (collisionStrategy === "go-packages") {
    const keys = new Set();
    for (const f of analyzed.affected_files) {
      keys.add(goPackageOf(f));
    }
    return keys;
  }
  return new Set(analyzed.affected_files);
}

function buildConflictGraph(analyzed, collisionStrategy) {
  const n = analyzed.length;
  const adj = Array.from({ length: n }, () => new Set());
  const keys = analyzed.map((a) => conflictKeysFor(a, collisionStrategy));

  for (let i = 0; i < n; i++) {
    for (let j = i + 1; j < n; j++) {
      const a = analyzed[i];
      const b = analyzed[j];

      const sharesFiles = [...keys[i]].some((k) => keys[j].has(k));
      const bothUnknown = !a.has_known_deps && !b.has_known_deps;

      if (sharesFiles || bothUnknown) {
        adj[i].add(j);
        adj[j].add(i);
      }
    }
  }

  return adj;
}

function graphColoring(adj, n, maxPerColor) {
  const colors = new Array(n).fill(-1);
  const colorCounts = [];

  for (let node = 0; node < n; node++) {
    const usedColors = new Set();
    for (const neighbor of adj[node]) {
      if (colors[neighbor] !== -1) {
        usedColors.add(colors[neighbor]);
      }
    }

    let assigned = -1;
    for (let c = 0; c < colorCounts.length; c++) {
      if (!usedColors.has(c) && colorCounts[c] < maxPerColor) {
        assigned = c;
        break;
      }
    }

    if (assigned === -1) {
      assigned = colorCounts.length;
      colorCounts.push(0);
    }

    colors[node] = assigned;
    colorCounts[assigned]++;
  }

  return colors;
}

function planWaves(issues, collisionStrategy) {
  if (!issues || issues.length === 0) {
    return { waves: [], total_issues: 0, total_waves: 0 };
  }

  const analyzed = issues.map((iss) => analyzeIssue(iss, collisionStrategy));
  const adj = buildConflictGraph(analyzed, collisionStrategy);
  const colors = graphColoring(adj, analyzed.length, MAX_PER_WAVE);

  const maxWave = Math.max(...colors) + 1;
  const waves = [];

  for (let w = 0; w < maxWave; w++) {
    const waveIssues = analyzed.filter((_, i) => colors[i] === w);
    if (waveIssues.length > 0) {
      waves.push({
        wave: w + 1,
        issues: waveIssues,
      });
    }
  }

  return {
    total_issues: analyzed.length,
    total_waves: waves.length,
    waves,
  };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

const USAGE = `wave-planner.js — group GitHub issues into parallelization waves

Usage:
  gh issue list --json number,title,body,labels,state | node wave-planner.js [FILE]
  node wave-planner.js issues.json

Options:
  --collision-strategy <mode>   How file-level conflicts are derived.
                                Modes:
                                  go-packages  (default) Conflict only when
                                               issues touch the same Go
                                               package directory, e.g.
                                               internal/handlers/. No collision
                                               files are auto-injected.
                                  none         No auto-injection. Conflicts are
                                               derived solely from explicit file
                                               references in issue text.
                                  legacy       Inject HIGH_COLLISION_FILES
                                               (cmd/nexus/main.go,
                                               main_test.go) into every issue.
                                               Backward-compatible behaviour.
  --dry-run, -n                 Show affected_files analysis (with per-file
                               derivation source) without generating wave plans.
  --help, -h                    Show this help message.

Reads JSON from stdin or a file (accepts raw array or {issues: [...]} wrapper).
Filters out already-closed issues and issues already resolved in develop
(body patterns, labels, or git history), groups remaining issues by
file-conflict graph, and outputs up to MAX_PER_WAVE (=3) issues per wave.

Examples:
  # Plan waves for all open issues (go-packages strategy, default)
  gh issue list --state open --json number,title,body,labels | node wave-planner.js

  # Review affected_files + derivation source before planning
  gh issue list --state open --json number,title,body,labels | node wave-planner.js --dry-run

  # Force legacy collision behaviour (serialise main.go touchers)
  node wave-planner.js /tmp/my-issues.json --collision-strategy legacy
`;

function main() {
  const opts = parseArgs(process.argv);

  if (opts.help) {
    process.stdout.write(USAGE);
    return;
  }

  const input = readInput(opts.file);
  let issues = Array.isArray(input) ? input : input.issues || [];
  // Filter out already-closed issues
  const beforeClosed = issues.length;
  issues = issues.filter((iss) => {
    const state = typeof iss.state === "string" ? iss.state : "open";
    return state.toLowerCase() !== "closed";
  });
  const filteredClosed = beforeClosed - issues.length;

  // Filter out already-resolved issues (body patterns, labels, or git history).
  const beforeResolved = issues.length;
  const resolvedIssues = [];
  issues = issues.filter((iss) => {
    if (isAlreadyResolved(iss)) {
      resolvedIssues.push(iss);
      return false;
    }
    return true;
  });
  const filteredAlreadyResolved = beforeResolved - issues.length;

  if (opts.dryRun) {
    // --dry-run: output affected_files analysis without generating wave plans.
    const analyzed = issues.map((iss) => analyzeIssue(iss, opts.collisionStrategy));
    const dryRunResult = {
      _meta: {
        filtered_closed: filteredClosed,
        filtered_already_resolved: filteredAlreadyResolved,
        mode: "dry-run",
        collision_strategy: opts.collisionStrategy,
        total_issues: analyzed.length,
      },
      issues: analyzed.map((a) => ({
        number: a.number,
        title: a.title,
        affected_files: a.affected_files,
        file_sources: a.file_sources,
        has_known_deps: a.has_known_deps,
      })),
      // Include resolved issues so the caller can see why they were skipped.
      already_resolved: resolvedIssues.map((iss) => ({
        number: iss.number,
        title: iss.title,
      })),
    };
    console.log(JSON.stringify(dryRunResult, null, 2));
    return;
  }

  const plan = planWaves(issues, opts.collisionStrategy);
  plan._meta = {
    filtered_closed: filteredClosed,
    filtered_already_resolved: filteredAlreadyResolved,
    collision_strategy: opts.collisionStrategy,
  };
  console.log(JSON.stringify(plan, null, 2));
}

main();
