---
about: Remove dead code in routemetrics.go — rc.responseTruncated field is
  incremented but never read by the Prometheus output path
labels: area=observability,phase=5-dx
severity: low
---

## Problem

`routemetrics.go` has a dead-code chain for response truncation counting:

1. `rc.responseTruncated uint64` (line 194) — struct field, never read by `WriteTo`
2. `SetTruncationCounter(&rc.responseTruncated)` (line 301) — sets the package-level `*uint64` pointer to point at the struct field
3. `IncrementTruncationCounter()` (line 389) → `atomic.AddUint64(&rc.responseTruncated, 1)` — increments the struct field

But `WriteTo` reads the truncation count from `ioutils.ReadAllTruncatedCounter()` (line 1035), which accesses the `ioutils` package-level `responseTruncated` — a completely separate counter incremented by `upstream.ReadAllLimited` / `ReadAllLimitedPooled`.

**The struct field `rc.responseTruncated` is never read from. It is dead code.**

The `SetTruncationCounter` wiring also creates confusion: the observability package-level `IncrementTruncationCounter` is a no-op when the global `*uint64` pointer is nil, but it's never the less-used path — it's set once at boot to point at a field that is never read.

## Fix

Remove the `rc.responseTruncated` struct field, `SetTruncationCounter`, `IncrementTruncationCounter`, and the `IncrementTruncationCounter()` call at line 826. The `ioutils` counter is the single source of truth and it is already correctly wired to Prometheus via `WriteTo`.

Confirm no other caller calls `observability.IncrementTruncationCounter` before removing it from the package.

## Files

- `internal/observability/routemetrics.go` — remove struct field, `SetTruncationCounter`, `IncrementTruncationCounter`, and the call site
