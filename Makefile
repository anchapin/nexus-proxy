# Nexus Proxy Makefile
#
# Common development tasks. CI runs `make ci` which gates on vet, build,
# test, and lint. Everything else is convenience for local iteration.

GO          ?= go
BINARY      ?= nexus
PKG         := ./...
LINT        ?= golangci-lint

.PHONY: help build run test test-race vet fmt lint tidy ci clean

help:
	@echo "Targets:"
	@echo "  build       - compile $(BINARY) into ./bin/"
	@echo "  run         - go run ./cmd/nexus"
	@echo "  test        - run unit tests"
	@echo "  test-race   - run unit tests with -race"
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

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

lint:
	$(LINT) run

tidy:
	$(GO) mod tidy

ci: vet build test lint

clean:
	rm -rf bin/ coverage.txt coverage.html