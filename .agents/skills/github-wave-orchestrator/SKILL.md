---
name: github-wave-orchestrator
description: >
  Autonomous agent that resolves all open GitHub issues by planning them into
  dependency-aware parallel execution waves using git worktrees and sub-agents.
  Use when the user asks to fix all issues, resolve open issues, run a wave,
  batch-fix issues, or mentions wave orchestration, parallel issue resolution,
  or worktree-based issue batching.
---

# GitHub Wave Orchestrator

Resolves all open GitHub issues via parallel execution waves. Each wave groups
independent issues (no shared files), spawns sub-agents in isolated worktrees,
monitors CI, merges PRs, then proceeds to the next wave.

## Quick Start

```
0. Pre-flight     →  verify gh auth, worktrees/ writable, skill sync check
1. Discover       →  gh issue list --json number,title,body,labels
2. Plan waves     →  node scripts/wave-planner.js < issues.json
3. Execute waves  →  worktree → implement → PR → CI → merge → cleanup
4. Repeat until all issues are resolved
```

## Phase 0: Pre-flight

**Pre-session stale worktree cleanup (issue #1396):**

Remove all worktrees whose branches no longer exist on origin — these are leftovers from aborted or interrupted prior sessions:
```bash
git fetch origin
for wt in ../worktrees/issue-*; do
  [ -d "$wt" ] || continue
  branch=$(git -C "$wt" branch --show-current 2>/dev/null)
  if [ -n "$branch" ] && ! git branch --list "origin/$branch" > /dev/null 2>&1; then
    echo "Removing stale worktree: $wt (branch $branch)"
    git worktree remove "$wt"
  fi
done
git remote prune origin
```

```bash
gh auth status                              # Must be authenticated
git fetch origin develop                    # Base branch must be current
mkdir -p ../worktrees && touch ../worktrees/.test && rm ../worktrees/.test  # Writable
```

**Skill sync check** — verify all skill resources exist before spawning sub-agents:
```bash
SKILL_DIR=".agents/skills/github-wave-orchestrator"
for f in "$SKILL_DIR/SKILL.md" "$SKILL_DIR/REFERENCE.md" "$SKILL_DIR/scripts/wave-planner.js"; do
  if [ ! -f "$f" ]; then
    echo "FAIL: missing skill resource: $f" >&2
    exit 1
  fi
done
```

If any check fails, stop and report to the user.

**Branch protection requirements (issue #1356):**

The wave orchestrator uses `gh pr merge --admin` to bypass branch protection.
For solo maintainers or repos where `required_approving_review_count > 0` and
no collaborators are available to review, the following branch protection
settings MUST be adjusted BEFORE wave orchestration begins:

```bash
# Verify current branch protection on the target branch (typically main)
gh api repos/{owner}/{repo}/branches/main/protection --jq '.required_approving_review_count,.require_last_push_approval'
```

If `required_approving_review_count > 0` and no collaborators can provide reviews,
set it to 0 before running wave orchestration. This is an operational security
policy change that should be intentional and persistent — the orchestrator does
NOT dynamically modify branch protection as part of the merge workflow.

**The orchestrator will FAIL to merge PRs to protected branches when:**
- `required_approving_review_count: 1` (or higher) and no other collaborators can review
- `require_last_push_approval: true` and the PR author is the only collaborator

Adjust branch protection in GitHub → Settings → Branches before starting wave orchestration.

## Phase 1: Discovery

```bash
gh issue list --state open --json number,title,body,labels,assignees
```

Filter out issues that are:
- Assigned to someone else (unless unassigned)
- Blocked by a label (e.g., `blocked`, `on-hold`)
- Already linked to an open PR (`gh pr list --search "fixes #N"`)

## Phase 2: Wave Planning

```bash
gh issue list --state open --json number,title,body,labels \
  | node .agents/skills/github-wave-orchestrator/scripts/wave-planner.js
```

The planner defaults to the `go-packages` collision strategy, which only
marks two issues as conflicting when they touch the same Go package
directory — enabling parallelism across independent `internal/*` packages.
Override with `--collision-strategy {none|go-packages|legacy}` or audit
file derivation with `--dry-run` (see REFERENCE.md › Collision Strategy).

**Present the plan to the user before executing.** Wait for confirmation.

## Phase 3: Wave Execution (per wave)

For each issue in the current wave:

### 3a. Worktree Setup

```bash
git worktree add ../worktrees/issue-{N}-{slug} -b fix/issue-{N}-{slug} develop
```

### 3a2. Worktree Branch Sanity (issue #1357)

**Before spawning any sub-agent**, verify the worktree branch is cleanly based on `origin/develop` with no diverged commits:

```bash
cd ../worktrees/issue-{N}-{slug}

# Check 1: branch must not have diverged from origin/develop
if ! git merge-base --is-ancestor HEAD origin/develop && ! git merge-base --is-ancestor origin/develop HEAD; then
  echo "ERROR: branch has diverged from origin/develop (worktree: issue-{N}-{slug})" >&2
  echo "This indicates stray commits from a prior wave. Resetting branch." >&2
  git fetch origin develop
  git reset --hard origin/develop
fi

# Check 2: branch must not be ahead of origin/develop (would indicate prior sub-agent push)
AHEAD=$(git log origin/develop..HEAD --oneline 2>/dev/null | wc -l)
if [ "$AHEAD" -gt 0 ]; then
  echo "WARNING: worktree branch is $AHEAD commit(s) ahead of origin/develop (worktree: issue-{N}-{slug})." >&2
  echo "Resetting to origin/develop to prevent stray commit accumulation." >&2
  git fetch origin develop
  git reset --hard origin/develop
fi

echo "Worktree sanity OK: issue-{N}-{slug} is clean on origin/develop"
```

If the reset removes uncommitted changes, they are stashed first (safe because the
worktree is otherwise empty at this point). This check prevents a worktree that
carried a stray commit from a prior wave from polluting a new wave's branch.

### 3b. Spawn Implementation Sub-agents

**Pre-flight check — main repo branch sanity (issue #1275):**

Before spawning any sub-agent, verify the main repo checkout is clean and on `develop`:
```bash
cd /home/alex/AI/nexus-proxy
if [ "$(git branch --show-current)" != "develop" ]; then
  echo "WARNING: Main repo is on branch '$(git branch --show-current)', not 'develop'." >&2
  echo "This indicates a previous sub-agent ran git commands in the main repo." >&2
  echo "Please manually reset: cd /home/alex/AI/nexus-proxy && git checkout develop && git reset --hard origin/develop" >&2
  exit 1
fi
if [ -n "$(git status --porcelain)" ]; then
  echo "WARNING: Main repo has uncommitted changes." >&2
  git status --short >&2
  exit 1
fi
echo "Pre-flight OK: main repo on develop, no uncommitted changes"
```

If the check fails, stop and report to the user — do NOT spawn sub-agents
until the main repo is restored.

### 3b2. Orchestrator Skill File Exclusion (issue #1394)

**Before spawning a sub-agent for any issue**, check whether the issue's affected
files include `.agents/skills/` or its subdirectories. Issues that touch the
orchestrator's own skill files must NOT be handled by a sub-agent — the
orchestrator cannot responsibly modify its own infrastructure as part of its
execution.

This check uses the same file-derivation logic as the wave planner:

```bash
# Check if issue #{N} touches .agents/skills/
# (applies the same heuristics as wave-planner.js file extraction)
ISSUE_BODY=$(gh issue view {N} --json body --jq '.body // ""')
if echo "$ISSUE_BODY" | grep -qE '\.agents/skills/|"\.agents/skills/"|`\.agents/skills/`'; then
  echo "ESCALATE: issue #{N} touches .agents/skills/ — orchestrator skill files must not be modified by wave sub-agents"
  echo "Marking issue #{N} as escalated in wave-state.json"
  # Record as escalated — these issues require manual orchestration maintenance
fi
```

If the issue body does not explicitly mention `.agents/skills/`, also check the
file list produced by the wave planner's `--dry-run` mode (if available) or fall
back to label-based detection: if any label matches `area:orchestrator`,
`component:wave-orchestrator`, or `module:.agents`, treat as skill-file-touching.

**Escalation response**: Mark the issue as `escalated` in wave-state.json and
report to the user that this issue requires manual handling outside the wave
orchestrator. The user should either handle it directly or create a separate
orchestrator-maintenance workflow that does NOT use the wave orchestrator.

### 3c. Spawn Implementation Sub-agents

Spawn one Task sub-agent per issue using the prompt template in
[REFERENCE.md — Implementation Sub-agent Template](REFERENCE.md#implementation-sub-agent-template).

Each sub-agent implements the fix and opens a PR.

### 3c. Wait + Verify

For each sub-agent in the wave, the orchestrator actively verifies PR creation
instead of passively waiting for a done signal:

**Per-sub-agent verification (parallel for all in wave):**

1. **Hard timeout**: Start a 5-minute timer when the sub-agent is spawned.
   If exceeded, enter the worktree directly, verify state, push if needed,
   and create the PR — bypassing the sub-agent entirely.

2. **Sub-agent done signal with heartbeat**:
   When a sub-agent reports "done":
   - **Step A: Capture the done signal and start a 60s heartbeat timer**
   - **Step B: Immediately verify commit count** (before any other action):
      ```bash
      cd ../worktrees/issue-{N}-{slug}
      COMMITS=$(git log origin/develop..HEAD --oneline 2>/dev/null | wc -l)
      if [ "$COMMITS" -eq 0 ]; then
        # No new commits — sub-agent reported done without committing
        echo "WARNING: Sub-agent reported done but no commits found. Entering recovery."
        enter_recovery_sequence
      elif [ "$COMMITS" -ne 1 ]; then
        # More than 1 commit — stray commits from prior wave (issue #1357)
        echo "WARNING: Sub-agent reported done but found $COMMITS commits (expected 1)." >&2
        echo "Stray commits may have accumulated from a prior wave. Resetting to origin/develop." >&2
        git fetch origin develop
        git reset --hard origin/develop
        enter_recovery_sequence
      fi
      ```
   - **Step C: PR verification loop** (while heartbeat timer is active):
     Poll every 10s for up to 60s:
     ```bash
     gh pr list --search "fix/issue-{N}" --json number,title,state --jq '.[] | select(.state=="OPEN") | .number'
     ```
    - **PR found** → record PR number in wave-state.json, move to next issue
      - **Heartbeat timeout (60s) or PR NOT found** → enter recovery sequence below

   **Post-subagent main-repo sanity check (issue #1275):**
   After each sub-agent reports done (or after recovery), immediately verify the main
   repo is still on `develop`:
   ```bash
   MAIN_BRANCH=$(git -C /home/alex/AI/nexus-proxy branch --show-current)
   if [ "$MAIN_BRANCH" != "develop" ]; then
     echo "WARNING: Main repo is now on branch '$MAIN_BRANCH', not 'develop'." >&2
     echo "Sub-agent may have run git commands outside the worktree." >&2
     echo "Please inspect and reset: cd /home/alex/AI/nexus-proxy && git checkout develop && git reset --hard origin/develop" >&2
     # Do not exit — the PR may still be valid; log and continue
   fi
   ```

3. **Recovery sequence** (when commit check fails, PR missing, or heartbeat timeout):
   ```bash
   cd ../worktrees/issue-{N}-{slug}

     # Step A: verify commit count (safety net — catches silent failure and stray commits, issue #1357)
     COMMITS=$(git log origin/develop..HEAD --oneline 2>/dev/null | wc -l)
     if [ "$COMMITS" -eq 0 ]; then
        echo "RECOVERY: No commits found in worktree. Worktree may be stale."

        # Preserve uncommitted changes before rebasing (issue #1272)
        # git rebase discards unstashed changes; stash is safe when clean
        if [ -n "$(git status --short)" ]; then
          echo "RECOVERY: Stashing uncommitted changes before rebase."
          git add -A && git stash push -m "wave-recovery-$(date +%s)"
        fi

        git fetch origin develop
        if git rebase origin/develop; then
          echo "RECOVERY: Rebase succeeded."
        else
          echo "RECOVERY: Rebase failed, aborting and restoring stashed changes."
          git rebase --abort 2>/dev/null || true
        fi

        # Restore stashed changes if any
        if git stash list | grep -q "wave-recovery"; then
          echo "RECOVERY: Restoring stashed changes."
          git stash pop || echo "WARNING: stash pop failed — manual intervention may be needed"
        fi
     elif [ "$COMMITS" -gt 1 ]; then
        # More than 1 commit — stray commits from prior wave (issue #1357)
        echo "RECOVERY: Found $COMMITS commits in worktree (expected 1). Stray commits detected." >&2

        # Preserve uncommitted changes before hard reset
        if [ -n "$(git status --short)" ]; then
          echo "RECOVERY: Stashing uncommitted changes before reset."
          git add -A && git stash push -m "wave-recovery-stray-$(date +%s)"
        fi

        git fetch origin develop
        git reset --hard origin/develop
        echo "RECOVERY: Branch reset to origin/develop. Stray commits discarded."

        # Restore stashed changes if any
        if git stash list | grep -q "wave-recovery"; then
          echo "RECOVERY: Restoring stashed changes."
          git stash pop || echo "WARNING: stash pop failed — manual intervention may be needed"
        fi
     fi

    # Step A2: detect and recover stray commits on origin/develop (issue #1284)
    STRAY=$(git -C /home/alex/AI/nexus-proxy log --oneline origin/develop | grep "fix/issue-{N}-{slug}" | head -1)
    if [ -n "$STRAY" ]; then
      echo "RECOVERY: Found stray commit on origin/develop for issue #{N}. Cherry-picking into worktree."
      SHA=$(echo "$STRAY" | awk '{print $1}')
      git cherry-pick $SHA
      git -C /home/alex/AI/nexus-proxy reset --hard origin/develop
    fi

    # Step B: check if branch was pushed
   git fetch origin
   if git branch --list origin/fix/issue-{N}-{slug} > /dev/null 2>&1; then
     # Branch exists remotely — PR was not created
     gh pr create --base develop \
       --title "$(git log -1 --format=%s)" \
       --body "Closes #{N}" \
       --head fix/issue-{N}-{slug}
   else
     # Branch was never pushed — push with idempotent lease
     git push -u origin fix/issue-{N}-{slug} --force-with-lease
     gh pr create --base develop \
       --title "$(git log -1 --format=%s)" \
       --body "Closes #{N}" \
       --head fix/issue-{N}-{slug}
   fi

   # Step C: verify PR was created
   gh pr list --search "fix/issue-{N}" --json number --jq 'length'
   # Must return 1 — if 0, escalate to user with worktree path
   ```

4. **PR base verification** (after PR is found, before recording it):
   ```bash
   BASE_REF=$(gh pr view {PR_NUMBER} --json baseRefName --jq '.baseRefName')
   if [ "$BASE_REF" != "develop" ]; then
     # Auto-fix: close wrong-base PR and recreate targeting develop
     gh pr close {PR_NUMBER}
     gh pr create --base develop \
       --title "$(gh pr view {PR_NUMBER} --json title --jq '.title')" \
       --body "$(gh pr view {PR_NUMBER} --json body --jq '.body')" \
       --head fix/issue-{N}-{slug}
   fi
   ```
   This catches sub-agents that omit `--base develop` from `gh pr create`.

5. **Idempotency**: All orchestrator push commands use `--force-with-lease`.
   All `gh pr create` calls are safe to re-run — GitHub returns error if PR
   already exists for that head branch, but the verification above prevents
   reaching that case.

6. **Escalation**: If recovery sequence fails or PR still missing after push,
   record issue as `escalated` in wave-state.json and report to user with
   worktree path so they can inspect and push manually.

**Do not proceed to Phase 4 until every PR in the wave exists (on develop) or is escalated.**

## Phase 4: CI and Merge

### 4a. Merge Ordering

Merge PRs in ascending issue-number order within a wave to minimize
conflict surface. After each merge, check remaining PRs for conflicts.
See [REFERENCE.md — Merge Ordering Strategy](REFERENCE.md#merge-ordering-strategy).

### 4b. Spawn CI Sub-agents

Spawn one sub-agent per PR using the prompt template in
[REFERENCE.md — CI Sub-agent Template](REFERENCE.md#ci-sub-agent-template).

Each sub-agent monitors CI, fixes failures, resolves merge conflicts,
and merges the PR.

### 4c. Issue Close Verification

After each PR merge, the CI sub-agent verifies that all issues mentioned
in the PR body are actually closed (issue #961). This catches the case where
GitHub's auto-close only matches some issues due to incorrect or incomplete
`Closes #N` syntax in the PR body. If any linked issue remains open, the
sub-agent reports BLOCKER and stops instead of proceeding.

### 4d. Wait

Monitor until ALL PRs in the wave are merged (or escalated).
Then clean up worktrees:
```bash
git worktree prune && git remote prune origin
```

### 4e. Main Checkout Sync (issue #1395)

After each wave completes (all PRs merged or escalated), sync the main checkout
to keep it current with `origin/develop` before the next wave:

```bash
cd /home/alex/AI/nexus-proxy

# Verify we are on develop before resetting (safety check — issue #1275)
if [ "$(git branch --show-current)" != "develop" ]; then
  echo "WARNING: Main checkout is on '$(git branch --show-current)', not 'develop'." >&2
  echo "Skipping sync to avoid losing work on a different branch." >&2
else
  if git fetch origin develop && git reset --hard origin/develop; then
    echo "Main checkout synced to origin/develop"
  else
    echo "WARNING: Failed to sync main checkout to origin/develop" >&2
  fi
fi
```

## Phase 5: Next Wave

Repeat Phase 3–4 for the next wave.
After the final wave, report summary:

```
WAVE ORCHESTRATION COMPLETE
===========================
Total issues: {N} | Waves: {count}
Merged: {count} | Escalated: {count} | Skipped: {count}
```

## Communication Rules

- **Silent during execution.** No play-by-play updates.
- **Update the user only when:**
  - Wave plan is ready for review (Phase 2)
  - A wave completes and the next begins
  - A sub-agent is stuck or CI cannot be fixed after 3 attempts
  - A merge conflict requires human resolution
  - The user asks a direct question

## Resume

If interrupted mid-wave, the orchestrator reads `../worktrees/wave-state.json`
to detect in-progress work and resumes from the last incomplete phase.
See [REFERENCE.md — Resume and Recovery](REFERENCE.md#resume-and-recovery).

## Limits

| Parameter | Value |
|---|---|
| Max issues per wave | 3 |
| Max CI fix iterations per PR | 10 |
| Max conflict resolution attempts | 2 |
| Worktree location | `../worktrees/` (parent of repo root) |
