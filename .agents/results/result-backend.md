# Result — Backend (Issue #37: proxy authentication middleware)

## Status
COMPLETE

## Summary
Added an optional bearer-token inbound authentication gateway. When at least
one of `NEXUS_PROXY_API_KEY` / `NEXUS_PROXY_API_KEYS` is set, requests to
`/v1/chat/completions` require a valid `Authorization: Bearer <key>` or
`X-API-Key` header; `/healthz` stays exempt for liveness probes. Key
comparison uses `crypto/subtle.ConstantTimeCompare`. When no key is
configured the middleware is a no-op pass-through (zero breaking change for
localhost dev). A boot-time `slog.Warn` fires when auth is disabled AND the
bind address is not loopback.

## Files changed
- NEW `internal/auth/auth.go` — `Middleware(keys, exempt)`; constant-time compare; OpenAI error envelope on 401.
- NEW `internal/auth/auth_test.go` — table-driven tests for all 8 acceptance criteria + extractKey/keyMatches/writeUnauthorized unit tests.
- MODIFIED `internal/config/config.go` — `ProxyAPIKey`, `ProxyAPIKeys`, `ProxyAuthEnabled` fields; `ProxyAuthKeys()` dedup helper; `splitCSV`; exported `IsLoopbackAddr`.
- MODIFIED `cmd/nexus/main.go` — wired `auth.Middleware` around mux with `/healthz` exempt; boot warn when disabled + non-loopback; server uses wrapped handler.
- MODIFIED `.env.example` — documented `NEXUS_PROXY_API_KEY` / `NEXUS_PROXY_API_KEYS`.
- MODIFIED `docker-compose.yml` — `NEXUS_PROXY_API_KEY` passthrough.

## Acceptance criteria checklist
- [x] NEXUS_PROXY_API_KEY set → missing/invalid key → 401 OpenAI envelope
- [x] NEXUS_PROXY_API_KEYS multi-key → any one accepted (rotation)
- [x] Neither set → identical to today (no-op middleware)
- [x] /healthz accessible without auth (healthzExempt predicate)
- [x] Boot slog.Warn when auth disabled + non-loopback bind
- [x] Constant-time compare (crypto/subtle.ConstantTimeCompare)
- [x] Unit tests: valid/invalid/missing key, multi-key rotation, healthz exempt, disabled=no-op
- [x] make ci components pass (vet/build/test clean; lint issues are pre-existing environmental toolchain mismatch, not introduced here)

## Test result
PASS — `go test -race ./...` → 406 passed in 17 packages; auth+config scoped → 90 passed.

## Lint result
PASS for changed packages. Pre-existing typecheck noise in internal/health/health_test.go and a vendored go1.26 file (golangci-lint built with go1.24) confirmed present on base commit — not introduced by this change. `go vet ./...` clean.
