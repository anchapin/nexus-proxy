---
about: 'The observability package defines two separate IncrementTruncationCounter
  functions in different packages: ioutils.IncrementTruncationCounter (increments
  ioutils.responseTruncated, the canonical counter) and observability.IncrementTruncationCounter
  (increments a package-level *uint64 pointer that, at boot, is set to point at
  routemetrics''s rc.responseTruncated struct field which is never read back.
  This API naming collision is confusing and is the root cause of the dead-code
  issue above. The observability package function should be renamed or removed.'
labels: area=observability,phase=5-dx
severity: low
---

## Problem

Two packages export a function named `IncrementTruncationCounter` with identical signatures but different targets:

| Package | Function | Target | Used by |
|---------|----------|--------|---------|
| `ioutils` | `IncrementTruncationCounter()` | `ioutils.responseTruncated` (atomic.Uint64) | `upstream`, `cascade`, `router`, `judge`, `rag` |
| `observability` | `IncrementTruncationCounter()` | `observability` package-level `*uint64` → `routemetrics.rc.responseTruncated` | `routemetrics` only |

The `observability` package function is a no-op when its package-level `*uint64` is nil, and it is set exactly once at boot to point at `routemetrics.rc.responseTruncated` — a field that `WriteTo` never reads (see related issue: *Dead code: rc.responseTruncated never read*).

The collision creates a trap: any caller that accidentally imports the observability package's `IncrementTruncationCounter` instead of `ioutils`'s will increment a counter that is never exported to Prometheus.

## Fix

After removing the dead `rc.responseTruncated` code (see related issue), rename the observability package function to something unambiguous (e.g., `IncrementRoutemetricsTruncationCounter`) or remove it entirely if no callers remain after the dead-code removal. Document clearly that `ioutils.IncrementTruncationCounter` is the single API for reporting truncation events to the observability pipeline.

## Files

- `internal/observability/routemetrics.go` — rename or remove `IncrementTruncationCounter`
