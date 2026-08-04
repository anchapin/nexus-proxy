#!/usr/bin/env bash
#
# scripts/verify_issues_closed.sh — verify all issues mentioned in a PR body
# are actually CLOSED after merge.
#
# This catches the case where GitHub's auto-close does not fire (e.g., when
# a PR is merged via `gh pr merge --admin` which bypasses some GitHub
# automation). In that case, the linked issues remain OPEN even after a
# successful merge.
#
# Usage:
#   bash scripts/verify_issues_closed.sh <PR_NUMBER>
#
# Exit codes:
#   0  all linked issues are CLOSED
#   1  one or more linked issues remain OPEN (or PR not found)
#   2  invalid usage (wrong arg count)
#
# Requirements: gh CLI authenticated with repo:read scope.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <PR_NUMBER>" >&2
  exit 2
fi

PR_NUMBER=$1

# Fetch the PR body
BODY=$(gh pr view "$PR_NUMBER" --json body --jq '.body // ""')

# Extract distinct issue numbers from closingReferences-style tokens
# Case-insensitive: Closes #N, Fixes #N, Resolves #N
ISSUE_NUMBERS=$(printf '%s\n' "$BODY" \
  | grep -oiE '(closes|fixes|resolves)\s+#[0-9]+' \
  | grep -oE '#[0-9]+' \
  | grep -oE '[0-9]+' \
  | sort -u)

if [[ -z "$ISSUE_NUMBERS" ]]; then
  echo "OK: PR #${PR_NUMBER} has no closingReferences in body"
  exit 0
fi

OPEN_ISSUES=""
CLOSED_COUNT=0
TOTAL_COUNT=0

for ISSUE_NUM in $ISSUE_NUMBERS; do
  TOTAL_COUNT=$((TOTAL_COUNT + 1))
  STATE=$(gh issue view "$ISSUE_NUM" --json state --jq '.state // "UNKNOWN"')

  if [[ "$STATE" == "CLOSED" ]]; then
    CLOSED_COUNT=$((CLOSED_COUNT + 1))
  else
    if [[ -n "$OPEN_ISSUES" ]]; then
      OPEN_ISSUES="${OPEN_ISSUES}, #${ISSUE_NUM}"
    else
      OPEN_ISSUES="#${ISSUE_NUM}"
    fi
  fi
done

if [[ -n "$OPEN_ISSUES" ]]; then
  echo "FAIL: PR #${PR_NUMBER} merged, but these issues remain OPEN: ${OPEN_ISSUES}" >&2
  exit 1
fi

echo "OK: PR #${PR_NUMBER} closed all ${TOTAL_COUNT} linked issue(s)"
exit 0
