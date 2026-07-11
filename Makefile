# Nexus Proxy Makefile
#
# Common development tasks. CI runs `make ci` which gates on vet, build,
# test, and lint. Everything else is convenience for local iteration.

GO          ?= go
BINARY      ?= nexus
PKG         := ./...
LINT        ?= golangci-lint

.PHONY: help build run test test-race bench bench-short vet fmt lint tidy ci clean

help:
	@echo "Targets:"
	@echo "  build       - compile $(BINARY) into ./bin/"
	@echo "  run         - go run ./cmd/nexus"
	@echo "  test        - run unit tests"
	@echo "  test-race   - run unit tests with -race"
	@echo "  bench       - run all benchmarks with -benchmem -count=5"
	@echo "  bench-short - run benchmarks with -benchtime=100ms for CI"
	@echo "  vet         - go vet"
	@echo "  fmt         - gofmt -w (writes in place)"
	@echo "  lint        - golangci-lint run"
	@echo "  tidy        - go mod tidy"
	@echo "  ci          - vet + build + test + lint (what CI runs)"
	@echo "  clean       - remove ./bin/ and coverage files"

build:
	@mkdir -p bin
	$(GO) build -o bin/$(BINARY) ./cmd/nexus

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

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

lint:
	$(LINT) run

tidy:
	$(GO) mod tidy

ci: vet build test lint bench-short

clean:
	rm -rf bin/ coverage.txt coverage.html