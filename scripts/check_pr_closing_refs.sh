#!/usr/bin/env bash
#
# scripts/check_pr_closing_refs.sh — verify a PR's body declares exactly
# N closingReferences-style tokens (Closes #N / Fixes #N / Resolves #N).
#
# Used by the agent workflow to gate issue-scoped PRs: a PR titled
# `docs: resolve #455 — ...` must close exactly one issue (#455), not
# silently drag in a sibling issue via a stray "Fixes #N" in the body.
# Without this check a multi-issue PR can land without explicit reviewer
# consent for the second issue.
#
# Usage:
#   bash scripts/check_pr_closing_refs.sh <PR_NUMBER> <EXPECTED_COUNT>
#
# Exit codes:
#   0  closingReferences count == EXPECTED_COUNT
#   1  closingReferences count != EXPECTED_COUNT, or PR not found
#   2  invalid usage (wrong arg count, non-numeric EXPECTED_COUNT)
#
# Requirements: gh CLI authenticated with repo:read scope.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <PR_NUMBER> <EXPECTED_COUNT>" >&2
  exit 2
fi

PR_NUMBER=$1
EXPECTED=$2

if ! [[ "$EXPECTED" =~ ^[0-9]+$ ]]; then
  echo "EXPECTED_COUNT must be a non-negative integer, got: $EXPECTED" >&2
  exit 2
fi

# Fetch the PR body. `gh pr view` prints body + metadata; we isolate the
# body via --json so we are robust to column-width changes.
BODY=$(gh pr view "$PR_NUMBER" --json body --jq '.body // ""')

# Count distinct closingReferences-style tokens. Case-insensitive, allow
# optional whitespace before the issue number. We dedupe on the issue
# number because a PR body that says "Closes #455" twice still closes
# one issue.
count=$(printf '%s\n' "$BODY" \
  | grep -oiE '(closes|fixes|resolves)\s+#[0-9]+' \
  | grep -oE '#[0-9]+' \
  | sort -u \
  | wc -l \
  | tr -d ' ')

if [[ "$count" -ne "$EXPECTED" ]]; then
  echo "FAIL: PR #${PR_NUMBER} has ${count} distinct closingReferences, expected ${EXPECTED}" >&2
  echo "----- PR body -----" >&2
  printf '%s\n' "$BODY" >&2
  echo "----- end body -----" >&2
  exit 1
fi

echo "OK: PR #${PR_NUMBER} has ${count} closingReferences (expected ${EXPECTED})"
exit 0