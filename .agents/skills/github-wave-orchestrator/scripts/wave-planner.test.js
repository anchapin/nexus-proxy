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
const pending = [];

function test(name, fn) {
  const p = Promise.resolve(fn()).then(
    () => {
      console.log(`  \u2713 ${name}`);
      passed++;
    },
    (err) => {
      console.log(`  \u2717 ${name}`);
      console.log(`    Error: ${err.message}`);
      failed++;
    }
  );
  pending.push(p);
  return p;
}

// Run the planner with optional extra CLI args, piping stdinData as JSON.
function runPlanner(stdinData, extraArgs) {
  return new Promise((resolve, reject) => {
    const proc = spawn("node", [wavePlannerPath, ...(extraArgs || [])], {
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

function runPlannerDryRun(stdinData, extraArgs) {
  return runPlanner(stdinData, ["--dry-run", ...(extraArgs || [])]);
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

test("--dry-run includes HIGH_COLLISION_FILES under legacy strategy", async () => {
  const issues = [
    { number: 1, title: "Issue 1", body: "General issue with no specific files", state: "open", labels: [] },
  ];

  const { stdout } = await runPlannerDryRun({ issues }, ["--collision-strategy", "legacy"]);
  const result = JSON.parse(stdout);

  if (result._meta.collision_strategy !== "legacy") {
    throw new Error(`Expected collision_strategy legacy, got ${result._meta.collision_strategy}`);
  }
  const issue1 = result.issues.find((i) => i.number === 1);
  if (!issue1.affected_files.includes("cmd/nexus/main.go")) {
    throw new Error("Should include cmd/nexus/main.go from HIGH_COLLISION_FILES");
  }
  if (!issue1.affected_files.includes("cmd/nexus/main_test.go")) {
    throw new Error("Should include cmd/nexus/main_test.go from HIGH_COLLISION_FILES");
  }
  // Derivation source column should mark injected files as "collision".
  if (issue1.file_sources["cmd/nexus/main.go"] !== "collision") {
    throw new Error("main.go should be sourced as 'collision'");
  }
});

test("--dry-run exposes file_sources derivation column", async () => {
  const issues = [
    { number: 1, title: "t", body: "Touches internal/foo/bar.go and internal/foo/baz.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlannerDryRun({ issues });
  const result = JSON.parse(stdout);

  const issue1 = result.issues.find((i) => i.number === 1);
  if (!issue1.file_sources) throw new Error("Should expose file_sources column");
  if (issue1.file_sources["internal/foo/bar.go"] !== "explicit") {
    throw new Error("explicit file should be sourced as 'explicit'");
  }
  if (issue1.file_sources["internal/foo/baz.go"] !== "explicit") {
    throw new Error("explicit file should be sourced as 'explicit'");
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
      } else if (!stdout.includes("--collision-strategy")) {
        reject(new Error("--help should document --collision-strategy flag"));
      } else {
        resolve();
      }
    });
  });
});

// ============================================================
// Collision-strategy tests
// ============================================================
console.log("\nCollision-strategy tests:");

test("go-packages (default): different packages → same wave", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/baz/qux.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues);
  const plan = JSON.parse(stdout);

  if (plan._meta.collision_strategy !== "go-packages") {
    throw new Error(`Expected go-packages default, got ${plan._meta.collision_strategy}`);
  }
  if (plan.total_waves !== 1) {
    throw new Error(`Expected 1 wave for independent packages, got ${plan.total_waves}`);
  }
  if (plan.waves[0].issues.length !== 2) {
    throw new Error(`Expected both issues in wave 1, got ${plan.waves[0].issues.length}`);
  }
});

test("go-packages: same package → separate waves", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/foo/baz.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues);
  const plan = JSON.parse(stdout);

  if (plan.total_waves !== 2) {
    throw new Error(`Expected 2 waves for same package, got ${plan.total_waves}`);
  }
});

test("go-packages: test file counts as same package", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/handlers/chat.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/handlers/chat_test.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues);
  const plan = JSON.parse(stdout);

  if (plan.total_waves !== 2) {
    throw new Error(`Expected separate waves for *_test.go in same package, got ${plan.total_waves}`);
  }
});

test("legacy: independent issues still conflict via injected files", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/baz/qux.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues, ["--collision-strategy", "legacy"]);
  const plan = JSON.parse(stdout);

  if (plan._meta.collision_strategy !== "legacy") {
    throw new Error(`Expected legacy, got ${plan._meta.collision_strategy}`);
  }
  if (plan.total_waves !== 2) {
    throw new Error(`Expected 2 waves (main.go injected) under legacy, got ${plan.total_waves}`);
  }
  // Legacy injects main.go into every issue.
  for (const wave of plan.waves) {
    for (const iss of wave.issues) {
      if (!iss.affected_files.includes("cmd/nexus/main.go")) {
        throw new Error("legacy should inject cmd/nexus/main.go");
      }
    }
  }
});

test("none: independent explicit files → same wave", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/baz/qux.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues, ["--collision-strategy", "none"]);
  const plan = JSON.parse(stdout);

  if (plan._meta.collision_strategy !== "none") {
    throw new Error(`Expected none, got ${plan._meta.collision_strategy}`);
  }
  if (plan.total_waves !== 1) {
    throw new Error(`Expected 1 wave under none, got ${plan.total_waves}`);
  }
});

test("none: shared explicit file → separate waves", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/foo/bar.go too", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues, ["--collision-strategy", "none"]);
  const plan = JSON.parse(stdout);

  if (plan.total_waves !== 2) {
    throw new Error(`Expected 2 waves under none for shared file, got ${plan.total_waves}`);
  }
});

test("invalid --collision-strategy exits non-zero", async () => {
  let caught = null;
  try {
    await runPlanner([], ["--collision-strategy", "bogus"]);
  } catch (err) {
    caught = err;
  }
  if (!caught) throw new Error("Expected an error for invalid collision strategy");
});

test("--collision-strategy=value form is accepted", async () => {
  const issues = [
    { number: 1, title: "t", body: "internal/foo/bar.go", state: "open", labels: [] },
    { number: 2, title: "t2", body: "internal/baz/qux.go", state: "open", labels: [] },
  ];

  const { stdout } = await runPlanner(issues, ["--collision-strategy=legacy"]);
  const plan = JSON.parse(stdout);

  if (plan._meta.collision_strategy !== "legacy") {
    throw new Error(`Expected legacy via = form, got ${plan._meta.collision_strategy}`);
  }
});

// ============================================================
// Summary
// ============================================================
Promise.all(pending).then(() => {
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
});
