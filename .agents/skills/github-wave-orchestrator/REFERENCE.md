# GitHub Wave Orchestrator — Reference

## Implementation Sub-agent Template

When spawning a Task sub-agent to implement an issue, use this prompt:

```
You are implementing a fix for GitHub issue #{NUMBER}: {TITLE}

Repository: {OWNER}/{REPO}
Branch: fix/issue-{NUMBER}-{SLUG} (already checked out)
Workdir: ../worktrees/issue-{NUMBER}-{SLUG}

Steps:
1. Read the full issue: gh issue view {NUMBER}
2. Read the issue comments for additional context: gh issue view {NUMBER} --comments
3. Analyze what code needs to change
4. Implement the fix/feature with tests
5. Run local checks if available (make test-fast, make lint)
 6. Commit: git add -A && git commit -m "{fix|feat}: resolve #{NUMBER} — {brief description}"
 7. Push: git push -u origin fix/issue-{NUMBER}-{SLUG} --force-with-lease
    NOTE: If push fails (e.g., remote branch exists with newer commits), use
    `git pull --rebase origin develop` first, then push again with --force-with-lease.
 8. Open PR with an EXPLICIT body (no `--fill` — see PR-body conventions):
   ```
   gh pr create --base develop \
     --title "{fix|feat}: resolve #{NUMBER} — {TITLE}" \
     --body "$(cat <<'EOF'
   Closes #{NUMBER}

   <one-paragraph description of the change>
   EOF
   )"
   ```
   The body must contain exactly one `Closes #N` line. Do NOT include other
   `#NNNN` references in the body or title — see
   `docs/orchestration/pr-body-conventions.md` for the rationale.
 9. Verify closingReferences count is exactly 1:
    ```
    bash scripts/check_pr_closing_refs.sh <PR_NUMBER> 1
    ```
    If the check fails, run `gh pr edit <PR> --body "Closes #N\n\n<minimal body>"`
    to strip the spurious references, then re-run the check.

    NOTE: After this step the orchestrator independently verifies the PR exists.
    If the sub-agent exits before completing step 7 or 8, the orchestrator's
    wave-level recovery sequence (see SKILL.md Phase 3c § Recovery) will push
    the branch and create the PR directly using --force-with-lease.

Rules:
- Work ONLY in your assigned worktree ({WORKDIR})
- Do NOT modify files outside the scope of this issue
- Include tests for the fix/feature if the repo has a test suite
- Follow the repo's AGENTS.md and code style conventions
- If the issue is unclear, add a comment asking for clarification: gh issue comment {NUMBER} -b "..."
- Report back: PR number, files changed, any blockers encountered
```

## CI Sub-agent Template

When spawning a Task sub-agent to monitor CI and merge a PR:

```
You are shepherding PR #{NUMBER} through CI to merge.

Repository: {OWNER}/{REPO}
PR: {PR_URL}
Branch: fix/issue-{NUMBER}-{SLUG}
Workdir: ../worktrees/issue-{NUMBER}-{SLUG}

Steps:
1. Check CI status: gh pr checks {NUMBER}
2. IF CI is green:
   a. Check mergeable: gh pr view {NUMBER} --json mergeable
   b. If CONFLICTING → follow the merge conflict protocol below
   c. If MERGEABLE → merge: gh pr merge {NUMBER} --squash --delete-branch
      Then verify the merge persisted:
      ```
      gh pr view {NUMBER} --json mergedAt --jq '.mergedAt'
      ```
      - If mergedAt is NOT null → merge succeeded, proceed to cleanup
      - If mergedAt IS null → merge did NOT persist. Retry once:
        `gh pr merge {NUMBER} --squash --delete-branch`
        If second attempt also yields null mergedAt → report BLOCKED and STOP
   d. Clean up: git worktree remove ../worktrees/issue-{NUMBER}-{SLUG}
3. IF CI is failing:
   a. Get failing run: gh run list --branch fix/issue-{NUMBER}-{SLUG} --limit 1
   b. Get logs: gh run view {RUN_ID} --log
   c. Diagnose the FIRST failing step
   d. Apply minimal fix in the worktree
   e. Commit and push: git add -A && git commit -m "fix: resolve CI failure" && git push
   f. Wait for CI to re-run (poll every 30s, max 10 iterations)
   g. If still failing after 10 iterations → report blocker and STOP
4. IF merge conflict detected after another PR was merged:
   a. git fetch origin develop
   b. git rebase origin/develop
   c. Resolve conflicts automatically where possible:
      - Lock files: regenerate (npm install, pip install)
      - Import blocks: accept both sides
      - Version bumps: accept higher version
      - Generated files: regenerate
   d. If conflicts cannot be auto-resolved → STOP and report:
      "MERGE CONFLICT on PR #{NUMBER}: {list conflicting files}"
   e. git push --force-with-lease origin fix/issue-{NUMBER}-{SLUG}
   f. Re-trigger CI and return to step 1

Report back: final status (MERGED / BLOCKED / CONFLICT), iterations used, files fixed.
   Note: MERGED status requires mergedAt to be non-null. If mergedAt is null after merge,
   the merge did not persist — report BLOCKED instead.
```

## Merge Ordering Strategy

Within a wave, merge PRs in a specific order to minimize conflicts:

### Ordering Rules (applied in priority order)

1. **Ascending issue number.** Lower issue numbers were filed earlier and
   typically touch more stable code. Merging them first provides a stable
   base for later PRs to rebase onto.

2. **Fewer affected files first (tiebreaker).** If two issues have the same
   number, merge the one touching fewer files first — it's less likely to
   cause conflicts for the other.

3. **Documentation-only PRs first (tiebreaker).** PRs that only touch
   `*.md` files are conflict-free with code changes and should merge first.

### After Each Merge

```bash
# Immediately after merging PR #{N}:
git fetch origin develop
git worktree remove ../worktrees/issue-{N}-{slug}

# For each remaining PR in the wave:
gh pr view {M} --json mergeable --jq '.mergeable'

# If CONFLICTING, the CI sub-agent for that PR handles rebase.
# No orchestrator intervention needed — the sub-agent template covers it.
```

## Resume and Recovery

### State File

The orchestrator writes a state file after each phase transition:

**Location:** `../worktrees/wave-state.json`

```json
{
  "repo": "owner/repo",
  "started_at": "2025-06-10T14:30:00Z",
  "current_wave": 2,
  "total_waves": 4,
  "issues": {
    "42": { "wave": 1, "status": "merged", "pr": 101, "branch": "fix/issue-42-fix-cache" },
    "17": { "wave": 1, "status": "merged", "pr": 102, "branch": "fix/issue-17-nomad-exec" },
    "31": { "wave": 2, "status": "pr_created", "pr": 103, "branch": "fix/issue-31-cache-key" },
    "8":  { "wave": 2, "status": "implementing", "worktree": "../worktrees/issue-8-docs-cli" }
  },
  "last_updated": "2025-06-10T14:45:00Z"
}
```

### Status Values

| Status | Meaning | Resume action |
|---|---|---|
| `pending` | Not started | Create worktree, spawn implementation sub-agent |
| `implementing` | Sub-agent working | Check if worktree has uncommitted changes |
| `pr_created` | PR exists, CI not checked | Spawn CI sub-agent |
| `ci_fixing` | CI sub-agent working | Check PR checks status |
| `conflicted` | Merge conflict, needs resolution | Spawn CI sub-agent with conflict focus |
| `merged` | Complete | Skip |
| `escalated` | Blocked, needs human | Skip, report to user |

### Resume Procedure

```
1. Read ../worktrees/wave-state.json
2. IF file does not exist → start fresh (Phase 0)
3. IF file exists:
   a. Find the current_wave with incomplete issues
   b. For each issue in current_wave:
      - IF status == "implementing":
          Check if worktree exists and has changes
          IF yes → spawn sub-agent to continue
          IF no → recreate worktree from main
      - IF status == "pr_created" or "ci_fixing":
          Verify PR still exists: gh pr view {N}
          IF yes → spawn CI sub-agent
          IF no (PR was closed/deleted) → reset to "pending"
      - IF status == "conflicted":
          Spawn CI sub-agent with conflict resolution focus
      - IF status == "escalated":
          Report to user, skip
   c. Continue normal wave execution for remaining phases
```

### State Updates

Write to `wave-state.json`:
- After each wave plan is confirmed (initial state)
- After each sub-agent reports PR creation
- After each PR merge or escalation
- After each wave completion

### Cleanup on Successful Completion

```bash
rm ../worktrees/wave-state.json
```

## File-Level Dependency Analysis

The wave planner extracts likely affected files from each issue using these
heuristics, applied in order of priority:

### 1. Explicit file references in issue body

Regex patterns that capture file paths mentioned in the issue text:

```
[`"]([a-zA-Z0-9_/.-]+\.[a-z]{2,4})[`"' ]
([a-zA-Z0-9_/.-]+/src/[a-zA-Z0-9_/.-]+\.[a-z]{2,4})
```

Common path patterns: `src/`, `lib/`, `test/`, `tests/`, `pkg/`, `cmd/`,
`internal/`, `osimflow/`, `bin/`.

### 2. Module and import references

```
import .+ from ['"](.+)['"]
from (.+) import
require\(['"](.+)['"]\)
use (.+::\w+)
```

Map module names to file paths using the repo's module structure.

### 3. Code symbol matching

Extract class names, function names, or constants from the issue body.
Search the codebase for definitions:

```bash
grep -rn "def {symbol}\|class {symbol}\|fn {symbol}\|func {symbol}" --include="*.{py,ts,js,rs,go}"
```

### 4. Label-based hints

Labels like `area:cache`, `component:executor`, `module:work` map to
directory prefixes configured per-repo. If no explicit mapping exists,
the label name is used as a fuzzy directory filter.

### 5. Fallback

If no files can be determined from the issue, mark it as `unknown_deps`.
Issues with `unknown_deps` are placed in single-issue waves (no parallelism)
to avoid silent conflicts.

## Merge Conflict Resolution Protocol

### Detection

After each PR merge in a wave, check remaining PRs:

```bash
gh pr view {N} --json mergeable --jq '.mergeable'
```

Values: `MERGEABLE`, `CONFLICTING`, `UNKNOWN` (still calculating).

Poll `UNKNOWN` every 15s for up to 2 minutes before treating as `CONFLICTING`.

### Resolution Flow

```
1. cd /path/to/worktree/issue-{N}-{slug}
2. git fetch origin develop
3. git rebase origin/develop

   IF rebase succeeds (no conflicts):
     → git push --force-with-lease origin fix/issue-{N}-{slug}
     → Re-trigger CI
     → Done

   IF rebase has conflicts:
     → Check conflicted files
     → Apply auto-resolution strategy (see below)
     → IF all conflicts resolved:
         git add -A && git rebase --continue
         git push --force-with-lease origin fix/issue-{N}-{slug}
         → Re-trigger CI
     → IF any conflict unresolvable:
         git rebase --abort
         → ESCALATE to user with conflict details
         → Do NOT skip silently
```

### Auto-Resolution Strategies

| Conflict type | Strategy |
|---|---|
| **Non-overlapping hunks** | Git resolves automatically on `rebase --continue` |
| **Generated files** (lock files, dist/, .generated) | Regenerate: `npm install`, `pip compile`, rebuild |
| **Import blocks** | Accept both sides (union of imports) |
| **Version bumps** (pyproject.toml, package.json) | Accept the higher version |
| **Test fixtures** | Accept the sub-agent's version (newer) |
| **Documentation** (README, CHANGELOG) | Accept the sub-agent's version |
| **Unrelated logic in same file** | Manual resolution required → escalate |

### Escalation Message Format

```
MERGE CONFLICT — Requires human resolution
PR #{N}: {title}
Branch: fix/issue-{N}-{slug}
Conflicting files:
  - {file_path} ({conflict_type})

Conflicted sections:
{git diff --name-only --diff-filter=U output}

Action needed: Rebase onto origin/develop and resolve conflicts manually.
Worktree: ../worktrees/issue-{N}-{slug}
```

## CI Failure Retry Protocol

### Iteration limits

| Level | Max | Action on exhaustion |
|---|---|---|
| Per-job fix iterations | 10 | Skip job, try next |
| Per-PR total iterations | 30 | Escalate to user |
| Conflict resolution attempts | 2 | Escalate to user |
| Overall wave timeout | 30 min | Report progress, ask to continue |

### Failure type routing

| Failure | Sub-agent action |
|---|---|
| Test failure | Read test, fix source or update assertion |
| Lint error | Auto-fix (`ruff --fix`, `eslint --fix`) |
| Type error | Fix annotations |
| Build error | Check deps, imports, config |
| Merge conflict | Apply conflict protocol (above) |
| Timeout | Increase test timeout or optimize |
| Flaky test | Re-run once; if passes → continue; if fails → investigate |
| Env/secret missing | Escalate (cannot fix without repo admin) |

## Worktree Safety

### Naming convention

```
../worktrees/issue-{NUMBER}-{SLUG}
```

Where `{SLUG}` is the first 3 dash-separated words of the issue title,
lowercased, non-alphanumeric chars stripped.

### Pre-creation checks

Before creating a worktree:

1. Verify the branch doesn't already exist: `git branch --list fix/issue-{N}-*`
2. Verify the worktree directory doesn't exist: `ls ../worktrees/`
3. Verify `develop` is up to date: `git fetch origin develop`

### Cleanup

After PR merge:
```bash
git worktree remove ../worktrees/issue-{N}-{slug}
git branch -d fix/issue-{N}-{slug}
git worktree prune
```

On abort (user cancellation or critical failure):
```bash
# List all worktrees
git worktree list

# Remove all wave worktrees
git worktree list --porcelain | grep "^worktree " | grep "worktrees/issue-" | \
  awk '{print $2}' | xargs -I{} git worktree remove {}

# Clean up branches
git branch --list 'fix/issue-*' | xargs -I{} git branch -D {}

git worktree prune
rm -f ../worktrees/wave-state.json
```

## Composition Points

### With `ci-iterative-fix`

CI sub-agents follow the `ci-iterative-fix` skill for the core
fetch-diagnose-fix-push-rerun loop. The wave orchestrator adds:

- Merge conflict detection and resolution (backported to `ci-iterative-fix`)
- Wave-aware ordering (merge earlier PRs first to minimize conflicts)
- Worktree lifecycle management

### With `parallel-issue-workflow`

For simple cases (all issues independent, no wave planning needed),
`parallel-issue-workflow` is sufficient. Use the wave orchestrator when:

- Issues have known or suspected dependencies
- There are more than 3 issues (wave cap kicks in)
- Previous runs had merge conflict issues

### With `pr-review-merge`

For CI monitoring of existing PRs (no wave orchestration needed),
use `pr-review-merge` directly. The wave orchestrator composes this
skill's merge logic into its Phase 4.
