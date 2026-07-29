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

**Present the plan to the user before executing.** Wait for confirmation.

## Phase 3: Wave Execution (per wave)

For each issue in the current wave:

### 3a. Worktree Setup

```bash
git worktree add ../worktrees/issue-{N}-{slug} -b fix/issue-{N}-{slug} develop
```

### 3b. Spawn Implementation Sub-agents

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

2. **PR verification loop** (while timer is active):
   After the sub-agent reports "done", poll every 10s for up to 60s:
   ```bash
   gh pr list --search "fix/issue-{N}" --json number,title,state --jq '.[] | select(.state=="OPEN") | .number'
   ```
   - **PR found** → record PR number in wave-state.json, move to next issue
   - **PR NOT found after 60s** → enter recovery sequence below

3. **Recovery sequence** (when PR missing or timeout):
   ```bash
   cd ../worktrees/issue-{N}-{slug}

   # Step A: check if branch was pushed
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

   # Step B: verify PR was created
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

### 4c. Wait

Monitor until ALL PRs in the wave are merged (or escalated).
Then clean up worktrees: `git worktree prune`

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
