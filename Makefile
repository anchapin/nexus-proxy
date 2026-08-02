# Nexus Proxy Makefile
#
# Common development tasks. CI runs `make ci` which gates on vet, build,
# test, and lint. Everything else is convenience for local iteration.

GO          ?= go
BINARY      ?= nexus
PKG         := ./...
LINT        ?= golangci-lint

# Build version injected via -ldflags. Defaults to "dev" for local
# builds; the release workflow overrides this with the git tag (e.g.
# v1.0.0). Override locally with: make build VERSION=v1.2.3
VERSION     ?= dev
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: help build run test test-race bench bench-short bench-baseline vet fmt lint tidy ci clean docker-build install-hooks release release-snapshot check-rules check

help:
	@echo "Targets:"
	@echo "  build       - compile $(BINARY) into ./bin/"
	@echo "  run         - go run ./cmd/nexus"
	@echo "  test        - run unit tests"
	@echo "  test-race   - run unit tests with -race"
	@echo "  bench       - run all benchmarks with -benchmem -count=5"
	@echo "  bench-short - run benchmarks with -benchtime=100ms for CI"
	@echo "  bench-baseline - regenerate bench/paseline.txt for benchstat (issue #1186)"
	@echo "  vet         - go vet"
	@echo "  fmt         - gofmt -w (writes in place)"
	@echo "  lint        - golangci-lint run"
	@echo "  tidy        - go mod tidy"
	@echo "  check       - run 'nexus check' boot-time diagnostics (issue #1248)"
	@echo "  ci          - vet + build + check + test + test-race + lint (what CI runs)"
	@echo "  docker-build - build the container image (smoke; needs Docker)"
	@echo "  clean       - remove ./bin/ and coverage files"
	@echo "  install-hooks - install git pre-commit hook (gofmt check)"
	@echo "  release     - run goreleaser release (requires tag + secrets)"
	@echo "  release-snapshot - dry-run goreleaser locally (no upload)"
	@echo "  check-rules - validate Prometheus rule files with promtool (issue #1151)"

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/nexus

# check runs the built binary's diagnostic suite (issue #1248). Network-
# dependent checks (Ollama, frontier) skip (not fail) when services are
# absent, so this is safe to run in CI without any external deps.
check: build
	./bin/$(BINARY) check --json

run:
	$(GO) run ./cmd/nexus

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race $(PKG)

# Benchmarks — see docs/BENCHMARKS.md for the baseline run that
# produced the reference numbers, and instructions for re-running on a
# new machine.
#
# `make bench` is the full pass (5x iterations per benchmark, suitable
# for local profiling). Use `make bench-short` on CI: -benchtime=100ms
# caps every benchmark at ~100ms so the whole suite runs in under 30s
# while still catching gross regressions.
BENCH_PACKAGES ?= $(PKG)

bench:
	$(GO) test -run='^$$' -bench=. -benchmem -count=5 -benchtime=1s $(BENCH_PACKAGES)

bench-short:
	$(GO) test -run='^$$' -bench=. -benchmem -benchtime=100ms $(BENCH_PACKAGES)

# bench-baseline regenerates the stored baseline used by CI benchstat
# regression detection (issue #1186). Run on develop after hot-path
# changes, then commit bench/baseline.txt. Uses -count=10 for tighter
# statistical confidence and -benchtime=100ms to keep runtime manageable.
bench-baseline:
	$(GO) test -run='^$$' -bench=. -benchmem -count=10 -benchtime=100ms $(BENCH_PACKAGES) > bench/baseline.txt

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

lint:
	$(LINT) run

tidy:
	$(GO) mod tidy

# `test-race` is included here because race conditions in concurrent code
# (transport, metrics, budget tracker, VRAM limiter) are easy to miss in
# manual testing and can hide in CI for weeks before surfacing in prod.
ci: vet build check test test-race lint bench-short

# docker-build smoke-builds the container image. Used by the ci.yml
# `docker` job (issue #541) so Dockerfile / go.mod Go-version drift is
# caught on every PR. Build only — never pushes. Requires Docker.
docker-build:
	docker build .

clean:
	rm -rf bin/ coverage.txt coverage.html

# install-hooks configures Git's hooksPath to point to .githooks/, then
# marks the pre-commit hook as executable. Run 'make install-hooks' once
# after cloning; no need to re-run unless .githooks/ is updated.
install-hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/pre-commit

# release and release-snapshot wrap GoReleaser (issue #1179). The
# Homebrew formula is generated from .goreleaser.yml and pushed to
# anchapin/homebrew-nexus on real releases.
#
# `make release-snapshot` is a local dry-run — it builds archives and
# generates the formula into ./dist/ without uploading anything.
# `make release` runs the full pipeline and requires:
#   - A clean git tag (e.g. git tag v1.0.0)
#   - GITHUB_TOKEN env var
#   - HOMEBREW_TAP_GITHUB_TOKEN env var (PAT for the tap repo)
GORELEASER ?= goreleaser

release:
	$(GORELEASER) release --clean

release-snapshot:
	$(GORELEASER) release --snapshot --clean

# check-rules validates the shipped Prometheus alerting and recording
# rule files with promtool (issue #1151). promtool is the canonical
# validator; if it is not on PATH the target prints install instructions
# and exits non-zero so CI fails loudly rather than silently skipping.
PROMTOOL ?= $(shell command -v promtool 2>/dev/null)
RULE_FILES := deploy/prometheus/recording-rules.yaml deploy/prometheus/alerts.yaml

check-rules:
ifeq ($(PROMTOOL),)
	@echo "promtool not found on PATH." >&2
	@echo "Install it from https://prometheus.io/docs/prometheus/latest/installation/ or:" >&2
	@echo "  cd /tmp && curl -sL https://github.com/prometheus/prometheus/releases/latest/download/prometheus-\$$(uname -s | tr A-Z a-z)-amd64.tar.gz | tar xz" >&2
	@exit 1
else
	$(PROMTOOL) check rules $(RULE_FILES)
endif
