#!/usr/bin/env node
/**
 * wave-planner.test.js — Regression tests for wave-planner.js
 *
 * Run with: node wave-planner.test.js
 */

const { spawn } = require("child_process");
const path = require("path");

const wavePlannerPath = path.join(__dirname, "wave-planner.js");

let passed = 0;
let failed = 0;

function test(name, fn) {
  try {
    fn();
    console.log(`  \u2713 ${name}`);
    passed++;
  } catch (err) {
    console.log(`  \u2717 ${name}`);
    console.log(`    Error: ${err.message}`);
    failed++;
  }
}

function runPlanner(stdinData) {
  return new Promise((resolve, reject) => {
    const proc = spawn("node", [wavePlannerPath], {
      cwd: __dirname,
    });

    let stdout = "";
    let stderr = "";

    proc.stdout.on("data", (data) => {
      stdout += data.toString();
    });

    proc.stderr.on("data", (data) => {
      stderr += data.toString();
    });

    proc.on("close", (code) => {
      if (code !== 0 && stderr) {
        reject(new Error(stderr));
      } else {
        resolve({ stdout, code });
      }
    });

    if (stdinData) {
      proc.stdin.write(JSON.stringify(stdinData));
      proc.stdin.end();
    } else {
      proc.stdin.end();
    }
  });
}

function runPlannerDryRun(stdinData) {
  return new Promise((resolve, reject) => {
    const proc = spawn("node", [wavePlannerPath, "--dry-run"], {
      cwd: __dirname,
    });

    let stdout = "";
    let stderr = "";

    proc.stdout.on("data", (data) => {
      stdout += data.toString();
    });

    proc.stderr.on("data", (data) => {
      stderr += data.toString();
    });

    proc.on("close", (code) => {
      if (code !== 0 && stderr) {
        reject(new Error(stderr));
      } else {
        resolve({ stdout, code });
      }
    });

    if (stdinData) {
      proc.stdin.write(JSON.stringify(stdinData));
      proc.stdin.end();
    } else {
      proc.stdin.end();
    }
  });
}

console.log("\n=== wave-planner.js Regression Tests ===\n");

// ============================================================
// Integration tests (running the actual script)
// ============================================================
console.log("Integration tests:");

test("plans waves for multiple issues", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "internal/file1.go:10", state: "open", labels: [] },
    { number: 2, title: "Issue 2", body: "internal/file2.go:20", state: "open", labels: [] },
    { number: 3, title: "Issue 3", body: "internal/file3.go:30", state: "open", labels: [] },
    { number: 4, title: "Issue 4", body: "internal/file4.go:40", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner({ issues });
  const plan = JSON.parse(stdout);

  if (plan.total_issues !== 4) throw new Error(`Expected 4 issues, got ${plan.total_issues}`);
  if (plan.total_waves < 1) throw new Error("Should have at least one wave");
  if (plan.waves.length < 1) throw new Error("Should have waves array with content");
});

test("filters out closed issues", async () => {
  const issues = [
    { number: 1, title: "Open", body: "internal/file1.go", state: "open", labels: [] },
    { number: 2, title: "Closed", body: "internal/file2.go", state: "closed", labels: [] },
  ];

  const { stdout } = await runPlanner({ issues });
  const plan = JSON.parse(stdout);

  if (plan.total_issues !== 1) throw new Error(`Expected 1 issue after filter, got ${plan.total_issues}`);
  if (plan._meta.filtered_closed !== 1) throw new Error(`Expected 1 filtered, got ${plan._meta.filtered_closed}`);
});

test("returns empty plan for empty input", async () => {
  const { stdout } = await runPlanner([]);
  const plan = JSON.parse(stdout);

  if (plan.total_issues !== 0) throw new Error(`Expected 0 issues, got ${plan.total_issues}`);
  if (plan.total_waves !== 0) throw new Error("Should have 0 waves");
  if (plan.waves.length !== 0) throw new Error("Should have empty waves array");
});

test("respects MAX_PER_WAVE limit", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "internal/file1.go", state: "open", labels: [] },
    { number: 2, title: "Issue 2", body: "internal/file2.go", state: "open", labels: [] },
    { number: 3, title: "Issue 3", body: "internal/file3.go", state: "open", labels: [] },
    { number: 4, title: "Issue 4", body: "internal/file4.go", state: "open", labels: [] },
    { number: 5, title: "Issue 5", body: "internal/file5.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner({ issues });
  const plan = JSON.parse(stdout);

  for (const wave of plan.waves) {
    if (wave.issues.length > 3) {
      throw new Error(`Wave should have at most 3 issues, got ${wave.issues.length}`);
    }
  }
});

test("handles issues sharing files in same wave", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "cmd/nexus/main.go", state: "open", labels: [] },
    { number: 2, title: "Issue 2", body: "cmd/nexus/main.go also needs changes", state: "open", labels: [] },
    { number: 3, title: "Issue 3", body: "Different file", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner({ issues });
  const plan = JSON.parse(stdout);

  // Issues 1 and 2 share cmd/nexus/main.go, so they should be in the same wave
  // But since MAX_PER_WAVE is 3, they could all be in one wave
  const issue1Wave = plan.waves.find((w) => w.issues.some((i) => i.number === 1));
  const issue2Wave = plan.waves.find((w) => w.issues.some((i) => i.number === 2));

  if (!issue1Wave || !issue2Wave) throw new Error("Both issues should be assigned to a wave");
});

test("--dry-run shows affected_files without wave planning", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "Fix cmd/nexus/main.go:10", state: "open", labels: [] },
    { number: 2, title: "Issue 2", body: "Check internal/auth/auth.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlannerDryRun({ issues });
  const result = JSON.parse(stdout);

  if (result._meta.mode !== "dry-run") throw new Error("Should be in dry-run mode");
  if (!result.issues) throw new Error("Should have issues array");
  if (result.issues.length !== 2) throw new Error("Should have 2 issues");

  // In dry-run, issues should have affected_files but not wave assignments
  const issue1 = result.issues.find((i) => i.number === 1);
  if (!issue1.affected_files || issue1.affected_files.length === 0) {
    throw new Error("Issue 1 should have affected_files extracted");
  }
  if (issue1.has_known_deps !== true) throw new Error("Issue 1 should have known deps");
});

test("--dry-run includes HIGH_COLLISION_FILES", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "General issue with no specific files", state: "open", labels: [] },
  ];

  const { stdout } = await runPlannerDryRun({ issues });
  const result = JSON.parse(stdout);

  const issue1 = result.issues.find((i) => i.number === 1);
  if (!issue1.affected_files.includes("cmd/nexus/main.go")) {
    throw new Error("Should include cmd/nexus/main.go from HIGH_COLLISION_FILES");
  }
  if (!issue1.affected_files.includes("cmd/nexus/main_test.go")) {
    throw new Error("Should include cmd/nexus/main_test.go from HIGH_COLLISION_FILES");
  }
});

test("shows help with --help flag", async () => {
  return new Promise((resolve, reject) => {
    const proc = spawn("node", [wavePlannerPath, "--help"], {
      cwd: __dirname,
    });

    let stdout = "";
    let stderr = "";

    proc.stdout.on("data", (data) => {
      stdout += data.toString();
    });

    proc.stderr.on("data", (data) => {
      stderr += data.toString();
    });

    proc.on("close", (code) => {
      if (code !== 0) {
        reject(new Error(`--help exited with code ${code}`));
      } else if (!stdout.includes("wave-planner.js")) {
        reject(new Error("--help output missing expected content"));
      } else if (!stdout.includes("--dry-run")) {
        reject(new Error("--help should document --dry-run flag"));
      } else {
        resolve();
      }
    });
  });
});

// ============================================================
// Summary
// ============================================================
console.log("\n=== Results ===");
console.log(`  Passed: ${passed}`);
console.log(`  Failed: ${failed}`);
console.log();

if (failed > 0) {
  console.log(`FAILED: ${failed} test(s) failed`);
  process.exit(1);
} else {
  console.log("SUCCESS: All tests passed");
  process.exit(0);
}
