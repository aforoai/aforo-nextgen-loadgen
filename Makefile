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

.PHONY: all build test lint fmt vet tidy install clean release release-check help \
        doctor-local doctor-staging e2e-local e2e-staging e2e-test

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

## release: cross-compile for darwin/linux on amd64+arm64 via goreleaser (snapshot, no publish)
##
## Local dry-run only — does NOT push a tag, does NOT publish a release,
## does NOT update the Homebrew tap. Use this to verify .goreleaser.yaml
## and the cross-compile matrix before tagging. The real release fires
## from .github/workflows/release.yml when a v*.*.* tag is pushed.
release:
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser not found. Install via: brew install goreleaser/tap/goreleaser"; exit 1; }
	HOMEBREW_TAP_GITHUB_TOKEN=dry-run-placeholder goreleaser release --snapshot --clean --skip=publish

## release-check: validate .goreleaser.yaml without building (fast)
release-check:
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser not found. Install via: brew install goreleaser/tap/goreleaser"; exit 1; }
	goreleaser check

## doctor-local: run pre-flight diagnostic against the docker-compose stack on localhost
doctor-local: build
	@AFORO_ADMIN_TOKEN=$${AFORO_ADMIN_TOKEN:?AFORO_ADMIN_TOKEN is required for doctor; export it before running} \
	$(BIN_DIR)/$(BINARY) doctor --target local

## doctor-staging: run pre-flight diagnostic against staging (requires AFORO_STAGING_TOKEN)
doctor-staging: build
	@AFORO_STAGING_TOKEN=$${AFORO_STAGING_TOKEN:?AFORO_STAGING_TOKEN is required for doctor-staging} \
	AFORO_ADMIN_TOKEN=$$AFORO_STAGING_TOKEN \
	$(BIN_DIR)/$(BINARY) doctor --target staging

## e2e-local: full end-to-end flow against the local docker-compose stack
e2e-local: build
	@AFORO_ADMIN_TOKEN=$${AFORO_ADMIN_TOKEN:?AFORO_ADMIN_TOKEN is required for e2e; cd aforo-nextgen-docker && docker-compose up -d, then export the token} \
	$(BIN_DIR)/$(BINARY) e2e \
		--scenario scenarios/crawl-e2e.yaml \
		--target local \
		--include-billing \
		--include-lifecycle

## e2e-staging: full end-to-end flow against staging (requires AFORO_STAGING_TOKEN)
e2e-staging: build
	@AFORO_STAGING_TOKEN=$${AFORO_STAGING_TOKEN:?AFORO_STAGING_TOKEN is required for e2e-staging} \
	AFORO_ADMIN_TOKEN=$$AFORO_STAGING_TOKEN \
	$(BIN_DIR)/$(BINARY) e2e \
		--scenario scenarios/crawl-e2e.yaml \
		--target staging \
		--include-billing \
		--include-lifecycle

## e2e-test: run the tag-gated end-to-end Go test (requires Docker stack up)
e2e-test:
	go test -tags=e2e -count=1 -v ./test/e2e/...

## help: list documented targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
