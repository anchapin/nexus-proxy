# syntax=docker/dockerfile:1.7
#
# Multi-stage build for Nexus Proxy.
#
# Stage 1: compile a fully static binary in golang:1.21-alpine.
# Stage 2: copy the binary into distroless/static-debian12:nonroot.
#
# Final image has no shell, no package manager, and runs as UID 65532
# (the `nonroot` user bundled with distroless). Telemetry defaults to
# /tmp/nexus-telemetry.jsonl because WORKDIR=/tmp is the only guaranteed
# writable path for that UID in a read-only-root container.

# ---------- Stage 1: build ------------------------------------------------
FROM golang:1.21-alpine AS build

# Build version injected via -ldflags. The release workflow passes the
# git tag here (e.g. --build-arg VERSION=v1.0.0). Defaults to "dev" for
# local `docker build`.
ARG VERSION=dev

WORKDIR /src

# Module cache layer. The repo is currently stdlib-only so go.sum may
# not exist yet — wildcard keeps the COPY conditional and the cache layer
# stable as dependencies are added in future PRs.
COPY go.mod go.sum* ./
RUN go mod download

# Source layer. Re-uses the module cache above; only rebuilds when Go
# source files change.
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Fully static binary — CGO disabled so there is no glibc dependency.
# -trimpath strips local filesystem paths from the binary.
# -ldflags "-s -w" drops the symbol table and DWARF info for size.
# -X main.version injects the build version for `nexus --version`.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/nexus ./cmd/nexus

# ---------- Stage 2: runtime ---------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nexus /nexus

# All runtime state (telemetry JSONL, RAG index, anything the proxy wants
# to write) lands under /tmp because that's the only path guaranteed
# writable for UID 65532 when the container has a read-only root FS.
WORKDIR /tmp

# HTTP listen port — must be reachable from the host or the orchestrator's
# pod network. Override with NEXUS_ADDR=:9000 etc.
EXPOSE 8000

# Distroless/static has no shell, no curl, no wget. HEALTHCHECK NONE
# lets the orchestrator (Docker, k8s, Nomad) drive liveness from
# GET /healthz at the platform layer instead — the proxy already serves
# "ok" on that endpoint for that exact reason.
HEALTHCHECK NONE

# Run as the bundled nonroot user (UID 65532, GID 65532). The proxy
# is env-only by design (no .env file, no flag parsing) so it starts
# cleanly with the platform defaults supplied by `docker run -e`.
USER nonroot:nonroot

ENTRYPOINT ["/nexus"]
