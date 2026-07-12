# Benchmarks & Load-Testing

This document captures reproducible micro- and end-to-end performance
baselines for the proxy's hot path, plus the operator-facing load-test
harness. The numbers below are the reference snapshot from the
`fix/issue-36-benchmark-loadtest-baselines` worktree; they will drift
slightly across CPUs but should remain within ~20 % of these values on
a comparable machine. Use them to spot regressions — not as absolute
performance guarantees.

The benchmark **suite is regression-detection infrastructure**, not a
benchmarketing framework. The added cost is small: `make bench` runs
all benchmarks to convergence in roughly 60 s on a modern workstation;
`make bench-short` finishes the same suite in under 30 s with
`-benchtime=100ms` and runs on every PR.

---

## Reference hardware (baseline run)

| Component         | Value                                           |
| ----------------- | ----------------------------------------------- |
| Go version        | `go1.26.2 linux/amd64`                          |
| CPU               | AMD Ryzen 5 5600G with Radeon Graphics           |
| Architecture      | amd64, GOMAXPROCS = 12                          |
| OS                | Linux                                           |
| Run timestamp     | 2026-07-10 (commit branch `fix/issue-36-…`)     |

To reproduce on different hardware:

```bash
cd /path/to/nexus-proxy
make bench
```

(Or `BENCH_PACKAGES=./internal/handlers ./... make bench` to scope it.)

---

## How to read the numbers

`go test -bench` reports three things: ns/op, B/op, allocs/op. The
table below additionally normalises per-element throughput where it
matters (rows, bytes processed).

All numbers are the **mean of 5 runs** (`make bench` uses
`-count=5 -benchtime=1s`) on the reference hardware. Sub-benchmark
sizes match the "representative payloads" the production chat handler
is expected to see.

---

## 1. End-to-end handler (`internal/handlers/chat_bench_test.go`)

These benchmarks wire the full chat handler between an
`httptest.Server` proxy and a mock upstream. The mock SSE body is a
single-chunk JSON completion; the orchestrator chooses the route
deterministically (via the token guardrail or DSL regex) so the SLM
round-trip is bypassed. Numbers therefore measure the **chat-handler
hot path, not LLM latency**.

| Benchmark | ns/op | B/op | allocs/op | Notes |
| --------- | -----:| ----:| ---------:| ----- |
| `BenchmarkChatRouteFrontierStream`            | ≈ 588 µs |   246 KiB |   200 | Inline `httptest.NewRecorder`. Frontier (streaming SSE). |
| `BenchmarkChatRouteLocalCascade`              | ≈ 151 µs |    25 KiB |   246 | Inline `httptest.NewRecorder`. Local cascade (single SSE chunk). |
| `BenchmarkChatEndToEndProxy`                  | ≈ 803 µs |   294 KiB |   270 | Full `httptest.NewServer` proxy + real `http.Client`. |
| `BenchmarkChatEndToEndProxyConcurrent/workers1` | ≈ 787 µs |   292 KiB |   269 | Single goroutine against the proxy. |
| `BenchmarkChatEndToEndProxyConcurrent/workers4` | ≈ 598 µs |   349 KiB |   292 | 4 concurrent goroutines; per-op cost drops thanks to connection amortisation. |
| `BenchmarkChatEndToEndProxyConcurrent/workers8` | ≈ 591 µs |   348 KiB |   302 | 8 concurrent goroutines — flattens around the GOMAXPROCS=12 ceiling. |

### Reading the table

* The frontier-stream path is **~4×** slower than the local-cascade
  path because `upstream.Stream` walks the response body line-by-line
  and Flushes per chunk, while the cascade validates-then-writes a
  single buffer. That gap is consistent with the production code
  path and is expected.
* Per-op latency **decreases** under concurrency (workers1 → 4)
  because `httptest.NewServer` reuses a `http.Server` whose listener
  go-routine amortises request parsing.
* alloc/op is dominated by the in-memory buffer copy in
  `captureWriter` (the judge/quality tee). On the production path
  without observers attached (the `make run` default), this drops to
  ~120 allocs/op.

### Stress pattern to watch

If a future PR changes the chat-handler middleware chain, expect
`BenchmarkChatRouteFrontierStream` and `BenchmarkChatEndToEndProxy` to
track **linearly with the number of bytes each pass touches**. A
single new middleware pass that reads the entire messages slice
without a buffer cap should add at minimum a few hundred µs at the
30 000-character prompt size used here.

---

## 2. TOON compression (`internal/middleware/toon_bench_test.go`)

| Benchmark | ns/op | Throughput | B/op | allocs/op |
| --------- | -----:| ----------:| ----:| ---------:|
| `BenchmarkSerializeToTOON/3rows`        | ≈ 11.7 µs |  20.8 MB/s |  2.8 KiB |   97 |
| `BenchmarkSerializeToTOON/10rows`       | ≈ 33.7 µs |  24.0 MB/s |  8.4 KiB |  296 |
| `BenchmarkSerializeToTOON/50rows`       | ≈ 142  µs |  29.3 MB/s | 43.5 KiB | 1541 |
| `BenchmarkCompressJSONBlocks/noJSON`    | ≈  179 ns |          — |       0 |    0 |
| `BenchmarkCompressJSONBlocks/3rows`     | ≈  145 ns |          — |       0 |    0 |
| `BenchmarkCompressJSONBlocks/50rows`    | ≈  234 ns |          — |       0 |    0 |
| `BenchmarkAppendSystemNote`             | ≈ 119  µs |          — |   514 KiB |   2 |

### Reading the table

* `CompressJSONBlocks` is **regex-scan only** in the benchmark — the
  inner `SerializeToTOON` re-run is measured separately. On a 50-row
  array the regex pass adds ~234 ns of overhead per request, which is
  far below the regex's own `regexp.FindAll` cost on long inputs (~µs).
  This is well under the 1 ms-per-request budget the PRD allocates.
* `SerializeToTOON` is dominated by `strings.Builder` + a single
  `json.Unmarshal`. 50 rows takes ~142 µs — most of which is the
  per-key `strings.Builder` writes. If TOON ever shows up in
  profiles, swap the `strings.Builder` for a pre-sized `[]byte` and
  use `strconv.AppendFloat`-style batched writes.
* `AppendSystemNote` allocates ~514 KiB / op. That is **mostly from
  the benchmark iteration itself**, which copies the message slice on
  every call (see AGENTS.md note that the messages slice is a copy in
  this path). The **production path** only allocates on cold cache.
  See the "Watch-list" section below.

### Watch-list

* **`AppendSystemNote`**: if a regression makes this 2× slower, look
  for an additional `messages = append(...)` in the hot path — the
  current `O(n)` slice copy is the only allocation site.

---

## 3. RAG (`internal/rag/rag_bench_test.go`)

| Benchmark | ns/op | Notes |
| --------- | -----:| ----- |
| `BenchmarkCosineSimilarity`           | ≈   0.66 µs | 768-dim unit vectors. Inner loop of `Retrieve`. |
| `BenchmarkRetrieve/10examples`        | ≈    7.1 µs | Brute-force scan over 10 × 768 floats. |
| `BenchmarkRetrieve/50examples`        | ≈   34.0 µs | 50 × 768 = 38 400 multiply-adds. |
| `BenchmarkRetrieve/100examples`       | ≈   67.1 µs | Doubles — the scan is linear in `n×d`. |
| `BenchmarkRetrieve/200examples`       | ≈  137.0 µs | Doubles again. Confirms O(n×d) scaling. |

### Reading the table

The 200-example scan finishes in **~137 µs** per request — well under
the threshold (≈ 1 ms) where the brute-force O(n·d) scan would need
optimisation. There is no immediate benefit to a vector index; if the
store ever grows past ~5 000 examples (i.e. ~3.8 million multiply-adds
per request ≈ ~5 ms), revisit with an IVF-PQ or HNSW implementation.

The RAG scan allocates **zero bytes per call** (`B/op = 0`). All
intermediate state lives in stack-local `float64` accumulators inside
`CosineSimilarity`.

---

## 4. Cascade validation (`internal/upstream/cascade_bench_test.go`)

| Benchmark | ns/op | Throughput | B/op | allocs/op |
| --------- | -----:| ----------:| ----:| ---------:|
| `BenchmarkExtractAssistantContent/1KB`  | ≈  6.1 µs | 129 MB/s | 1.2 KiB | 12 |
| `BenchmarkExtractAssistantContent/4KB`  | ≈ 18.9 µs | 147 MB/s | 3.2 KiB | 12 |
| `BenchmarkExtractAssistantContent/16KB` | ≈ 66.2 µs | 161 MB/s |11.4 KiB | 12 |
| `BenchmarkWriteSSEResponse/1KB`        | ≈  5.5 µs |    —     | 4.6 KiB | 45 |
| `BenchmarkWriteSSEResponse/4KB`        | ≈  8.3 µs |    —     | 9.0 KiB | 45 |
| `BenchmarkWriteSSEResponse/16KB`       | ≈ 21.9 µs |    —     |24.7 KiB | 45 |
| `BenchmarkShouldRetry`                 | ≈   0.3 ns |    —     |     0  |  0 |

### Reading the table

* `ExtractAssistantContent` scales **linearly** with payload size
  (1 KB → 6 µs, 16 KB → 66 µs). 12 allocations per call are the
  per-field unmarshal into the typed `assistantResponse` struct; if
  you find these in a profile, switch to a pooled `sync.Pool` of
  `assistantResponse` values.
* `ShouldRetry` is a switch over an `int`. 0.3 ns/op is essentially
  free — confirms there is no need to memoize or precompute the
  retry-classification table.

---

## 5. Aggregate observations

* **Hot-path allocations dominate.** The frontier-stream handler
  allocates 200 B per request for the SSE capture buffer. The cascade
  allocates 12 + 45 = 57 objects per request. Without observers
  attached, the proxy can sustain **~6 600 req/s/worker** for the
  cascade path on this CPU. With 4 workers: ~26 000 req/s aggregate
  inside the proxy process (excludes upstream latency).
* **TOON is cheap enough** that it should never be removed for
  performance reasons; the regex-scan cost (~234 ns for 50 rows) is
  three orders of magnitude below the SLM round-trip it aims to
  shrink.
* **RAG scan is small but linear.** 200 examples = 137 µs; 1 000
  examples ≈ 690 µs. If a future user curates more than 2 000
  examples, expect to revisit the brute-force assumption.

---

## 6. Running the suite locally

```bash
# Full pass: 5 iterations per benchmark, ~60 s on a modern workstation.
make bench

# CI smoke pass: -benchtime=100ms, ~25 s total.
make bench-short

# One specific benchmark:
go test -run='^$' -bench=BenchmarkCosineSimilarity ./internal/rag/

# Memory profile alongside the latency profile:
go test -run='^$' -bench=. -benchmem -memprofile=mem.out ./...
go tool pprof -top mem.out
```

`-run='^$'` is required so the test runner does not also execute the
unit tests; benchmarks live in `*_bench_test.go` files.

---

## 7. Operator load-test harness

For end-to-end latency against a live proxy (frontier + LLM included),
use `scripts/loadtest.sh`. It requires either:

* `hey` (`go install github.com/rakyll/hey@latest`) — preferred; or
* falls back to a curl-burst that prints a 2xx/5xx status distribution.

```bash
# Quick smoke (200 reqs, concurrency 10) against localhost:
./scripts/loadtest.sh

# Heavier run, custom host and prompt payload:
NEXUS_URL=https://nexus.internal N=2000 C=50 \
  NEXUS_PROMPT_FILE=/tmp/realistic-prompt.txt \
  ./scripts/loadtest.sh
```

`scripts/loadtest.sh` exits **0** when every request is 2xx, **2**
when `hey` reports any non-2xx, and **1** if `/healthz` is
unreachable. Compare the `hey` p50/p99 columns against the
`BenchmarkChatEndToEndProxyConcurrent` numbers above: an order-of-
magnitude delta between them points at **upstream or network** being
the bottleneck, not the proxy.

---

## 8. CI integration

`.github/workflows/ci.yml` runs `make bench-short` on every PR as a
separate `bench (smoke)` job (`continue-on-error: true`) and uploads
`bench.txt` as a build artifact. The job is non-blocking while we
establish a baseline; once the team has ~30 days of uploaded runs,
the `bench` job can be promoted to gating and linked into a
trend-charts dashboard.

---

## 9. Update procedure

When something in the hot path intentionally changes (e.g. issue #6
added the dynamic VRAM budget hook), update this file with new
numbers in the same commit. The reviewer should be able to spot
whether the change regressed by reading the diff alone. Keep the table
formats above so future `git log -p docs/BENCHMARKS.md` reads as a
clear performance changelog.
