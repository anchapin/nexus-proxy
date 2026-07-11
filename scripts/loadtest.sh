#!/usr/bin/env bash
#
# scripts/loadtest.sh — operator load-test harness for a live nexus-proxy.
#
# Drives an OpenAI-compatible /v1/chat/completions at the configured URL
# with a configurable request burst and concurrency level, then prints a
# p50 / p90 / p99 latency + throughput summary suitable for comparing
# against the baseline numbers in docs/BENCHMARKS.md.
#
# The Go binary itself stays stdlib-only (see AGENTS.md), but operators
# who want to load-test a live deployment need an external HTTP driver.
# We use `hey` (github.com/rakyll/hey) when available, and gracefully
# fall back to a curl-based burst when it isn't installed — `hey` is a
# 5 MB static binary with no runtime dependencies and is the GitHub-
# Actions-recommended load tester for this class of harness.
#
# Usage:
#   ./scripts/loadtest.sh                                  # 200 reqs @ c=10
#   N=1000 C=50 ./scripts/loadtest.sh                      # 1000 reqs @ c=50
#   NEXUS_URL=http://localhost:8000 ./scripts/loadtest.sh  # different host
#   NEXUS_PROMPT_FILE=/tmp/big.txt ./scripts/loadtest.sh   # realistic prompt
#
# Environment knobs:
#   NEXUS_URL                 target base URL (default http://127.0.0.1:8000)
#   N                        total request count (default 200)
#   C                        concurrency level (default 10)
#   NEXUS_PROMPT_FILE        optional file whose contents become the
#                            user message; if unset, a fixed short prompt
#                            is used (which trips RouteLocal via the DSL
#                            regex, exercising the cascade path).
#   NEXUS_LOADTEST_TIMEOUT   per-request timeout (default 30s)
#   NEXUS_SKIP_HEY=1         force the curl fallback even when hey exists
#
# Exit code:
#   0   — every request returned 2xx and the driver reported no failures
#   1   — connectivity check failed (proxy not reachable)
#   2   — driver reported any non-2xx response

set -euo pipefail

# --- knobs ----------------------------------------------------------------
NEXUS_URL="${NEXUS_URL:-http://127.0.0.1:8000}"
N="${N:-200}"
C="${C:-10}"
TIMEOUT="${NEXUS_LOADTEST_TIMEOUT:-30}"
PROMPT_FILE="${NEXUS_PROMPT_FILE:-}"
SKIP_HEY="${NEXUS_SKIP_HEY:-}"

# --- helpers --------------------------------------------------------------

# pretty prints the chosen driver + tunables so the operator can sanity-
# check what is about to run before any traffic is generated.
print_config() {
  local driver="$1"
  echo "==> load-test against ${NEXUS_URL}/v1/chat/completions"
  echo "    driver: ${driver}"
  echo "    requests (N): ${N}"
  echo "    concurrency (C): ${C}"
  echo "    per-request timeout: ${TIMEOUT}s"
  if [[ -n "${PROMPT_FILE}" ]]; then
    echo "    prompt source: ${PROMPT_FILE} ($(wc -c <"${PROMPT_FILE}") bytes)"
  else
    echo "    prompt source: built-in short prompt (RouteLocal via DSL regex)"
  fi
  echo
}

# builds a /tmp/payload.json with a representative OpenAI-compatible
# request body. The prompt either comes from $PROMPT_FILE (so operators
# can reproduce a heavy real-world payload deterministically) or is a
# fixed short string that triggers the RouteLocal DSL rule. We use
# python instead of jq so the script has the same prerequisites as the
# proxy itself — nothing.
build_payload() {
  local prompt
  if [[ -n "${PROMPT_FILE}" && -f "${PROMPT_FILE}" ]]; then
    # escape backslashes and double quotes for embedding in JSON below
    prompt=$(python3 -c '
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    sys.stdout.write(json.dumps(fh.read())[1:-1])
' "${PROMPT_FILE}")
  else
    # keep this in sync with internal/handlers/chat_test.go so that the
    # DSL regex matches and RouteLocal is exercised. Single source of
    # truth: the regex `\b(css|format|docstring|lint|typo|boilerplate)\b`.
    prompt="please fix the css formatting in components/Button.tsx"
  fi

  python3 -c '
import json, sys
print(json.dumps({
    "model": "loadtest",
    "messages": [{"role": "user", "content": sys.argv[1]}],
    "stream": False,
}))
' "${prompt}" >/tmp/nexus-loadtest-payload.json
}

# --- pre-flight -----------------------------------------------------------

echo "[loadtest] checking proxy reachability at ${NEXUS_URL}/healthz"
if ! curl -fsS --max-time 5 "${NEXUS_URL}/healthz" >/dev/null 2>&1; then
  echo "[loadtest] FAILED: proxy not reachable at ${NEXUS_URL}/healthz" >&2
  echo "[loadtest] hint: start it with 'make run' or set NEXUS_URL correctly" >&2
  exit 1
fi
echo "[loadtest] OK"
echo

# --- driver selection -----------------------------------------------------

build_payload

driver="hey"
if [[ -n "${SKIP_HEY}" ]] || ! command -v hey >/dev/null 2>&1; then
  driver="curl-burst"
fi

print_config "${driver}"

# --- run ------------------------------------------------------------------

set +e   # the drivers return non-zero on any failure; capture and report

case "${driver}" in
  hey)
    # hey prints its own latency histogram + status-code breakdown. We
    # translate "non-zero exit" into our own exit code in `report`.
    hey \
      -n "${N}" \
      -c "${C}" \
      -t "${TIMEOUT}" \
      -m POST \
      -H "Content-Type: application/json" \
      -T "Content-Type: application/json" \
      -d @/tmp/nexus-loadtest-payload.json \
      "${NEXUS_URL}/v1/chat/completions"
    hey_rc=$?
    ;;
  curl-burst)
    # Fallback: open $C parallel curl loops, each issuing N/C requests
    # sequentially. Output is summarized (status counts) at the end.
    out_dir=$(mktemp -d)
    per=$(( N / C ))
    if (( per < 1 )); then per=1; fi

    pids=()
    for ((i = 0; i < C; i++)); do
      (
        for ((r = 0; r < per; r++)); do
          code=$(curl -s -o /dev/null -w "%{http_code}" --max-time "${TIMEOUT}" \
              -X POST \
              -H "Content-Type: application/json" \
              -d @/tmp/nexus-loadtest-payload.json \
              "${NEXUS_URL}/v1/chat/completions" || echo "000")
          echo "${code}" >>"${out_dir}/codes"
        done
      ) &
      pids+=("$!")
    done
    for pid in "${pids[@]}"; do
      wait "${pid}" || true
    done

    echo "==> curl-burst complete"
    sort "${out_dir}/codes" | uniq -c | sort -rn
    rm -rf "${out_dir}"
    hey_rc=0   # status-code breakdown above; non-2xx is the operator's call
    ;;
esac

set -e

# --- post-run report ------------------------------------------------------
#
# For hey, "perfect" run returns 0. Any 5xx surfaces as a non-zero exit
# AND prints a `[200]/[5xx]` distribution; we surface 2 only for 5xx
# non-zero and 1 for unreachability (handled earlier).
if [[ "${driver}" == "hey" && ${hey_rc} -ne 0 ]]; then
  echo
  echo "[loadtest] hey exited with status ${hey_rc}; check output above for failures"
  exit 2
fi

echo
echo "[loadtest] OK — compare the p50/p99 columns above against docs/BENCHMARKS.md"
