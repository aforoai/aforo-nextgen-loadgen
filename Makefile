# aforo-loadgen — Makefile
#
# Convention: every target works against the repo root and writes binaries to
# ./bin. CI uses the same targets, so what passes locally passes there.

BINARY      := aforo-loadgen
BIN_DIR     := bin
PKG_MAIN    := ./cmd/$(BINARY)
MODULE      := github.com/aforoai/aforo-nextgen-loadgen

# Version metadata baked into the binary at build time.
#
# VERSION resolves to the exact tag when a release commit is checked out, and
# falls back to "0.0.0-dev" everywhere else. Using --exact-match (rather than
# --always or --abbrev=0) keeps untagged dev builds clearly labelled — devs
# always see "0.0.0-dev" until a real release ships. CI overrides VERSION at
# release time.
VERSION     ?= $(shell git describe --tags --exact-match 2>/dev/null || echo "0.0.0-dev")
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS     := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

GOFLAGS     := -trimpath

.PHONY: all build test lint fmt vet tidy install clean release help

all: build

## build: compile the CLI to bin/aforo-loadgen
build:
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(PKG_MAIN)

## test: run all unit tests with race detector
test:
	go test -race -count=1 ./...

## lint: run golangci-lint (install via `make lint-install` if missing)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Run: make lint-install"; exit 1; }
	golangci-lint run ./...

## lint-install: install golangci-lint into $GOPATH/bin
lint-install:
	GO111MODULE=on go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.5

## fmt: gofmt -s on every package
fmt:
	gofmt -s -w .

## vet: go vet
vet:
	go vet ./...

## tidy: ensure go.mod/go.sum are minimal and correct
tidy:
	go mod tidy

## install: install the binary into $GOBIN (or $GOPATH/bin)
install:
	go install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(PKG_MAIN)

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## release: cross-compile for darwin/linux on amd64+arm64 (Session 1: stub)
release: build
	@echo "release: cross-compile matrix lands in Session 9"

## help: list documented targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
