# 2026-04-29 — Session 1: repo scaffold

## What changed

Initial scaffold for `aforo-nextgen-loadgen` — a Go CLI for load-testing the
Aforo NextGen ingestion pipeline. This commit lays down the command tree and
build/CI plumbing so subsequent sessions can drop in real implementations
without touching layout.

## Files added

### Module + entry point
- `go.mod` — module `github.com/aforoai/aforo-nextgen-loadgen`, Go 1.22, sole
  direct dep `spf13/cobra v1.10.2`.
- `go.sum` — checksums for cobra + transitive deps (mousetrap, pflag).
- `cmd/aforo-loadgen/main.go` — single binary entry; defers everything to
  `internal/cli.NewRootCommand().Execute()`.

### Internal packages
- `internal/cli/root.go` — root cobra command, persistent global flags
  (`--target`, `--config`, `--log-level`, `--json-logs`), subcommand
  registration, shared `notImplemented` helper.
- `internal/cli/{run,replay,seed,validate,lifecycle,payments,report,scenarios,doctor,server,e2e}.go` —
  11 stub subcommands. Each returns `notImplemented(...)` and exits 0 with a
  message naming the session in which the real implementation lands.
- `internal/cli/version.go` — only fully-implemented subcommand. Prints
  semver, commit SHA, build date sourced from `internal/version`.
- `internal/cli/cli_test.go` — five test functions:
  - `TestRootHelpListsAllSubcommands` — `--help` mentions every entry in the
    `expectedSubcommands` contract list.
  - `TestEverySubcommandExitsZero` — each registered subcommand exits 0 and
    produces output (Session 1 acceptance criterion).
  - `TestStubsAdvertiseSession` — every stub announces "not yet implemented"
    and a Session number.
  - `TestVersionSubcommandPrintsBuildMetadata` — version output contains the
    expected fields.
  - `TestGlobalFlagsAreRegistered` — all four persistent flags exist on root.
- `internal/version/version.go` — three vars (Version, Commit, BuildDate)
  defaulted to dev placeholders, overridden via `-ldflags -X` at build time.
- `internal/config/config.go` — `Config` struct + no-op `Load` so the
  `--config` flag has a typed landing place. Real schema arrives in Session 2.

### Tooling
- `Makefile` — targets: `build`, `test`, `lint`, `lint-install`, `fmt`, `vet`,
  `tidy`, `install`, `clean`, `release` (stub), `help`. Build embeds version
  metadata via `-ldflags`. VERSION resolves to the exact git tag if checked
  out at one, else `0.0.0-dev` — keeps untagged dev builds visibly labelled.
- `.golangci.yml` — `errcheck`, `govet`, `ineffassign`, `staticcheck`,
  `unused`, `gofmt`, `goimports`, `misspell`, `unconvert`, `gocritic`,
  `revive`. Test files exempted from `errcheck`.
- `.github/workflows/ci.yml` — two jobs on push/PR to `main`:
  `build-test` (build, smoke `--help` + `version`, run tests with race)
  and `lint` (`golangci-lint-action@v6` pinned to v1.64.5).
- `.gitignore` — Go binaries, editor/OS junk, coverage, `.env`.

### Distribution
- `scripts/homebrew/loadgen.rb` — Homebrew formula stub. Real release URLs
  + SHAs land in Session 9; the placeholder URL is intentionally invalid so
  no one ships a broken `brew install` path by accident.

### Docs + license
- `README.md` — what it is, install (brew/go install/source), quickstart,
  global flags, subcommand roadmap table (which session implements which),
  contribution checklist for adding new subcommands, persona model.
- `LICENSE` — Apache 2.0, copyright 2026 Aforo, Inc.
- `scenarios/.gitkeep` + `docs/.gitkeep` — placeholders so the empty
  directories track in git until Session 2 fills them.

## Why

Three tightly-coupled goals shape this scaffold:

1. **Lock the public surface early.** All 12 subcommands are named and wired
   now, even though 11 are stubs. CI enforces the surface via
   `expectedSubcommands` in the test file — adding/renaming a subcommand
   forces an explicit table update in the same commit. This prevents the
   surface from drifting as later sessions ship.

2. **Make every subsequent session a drop-in.** Each stub lives in its own
   file under `internal/cli/`, isolated from the others. Session 3 swaps
   `run.go`'s body for the real implementation without touching `root.go`,
   `cli_test.go`, or any sibling subcommand.

3. **Visible "not yet implemented" messaging.** Stubs print which session
   they ship in. A user installing a pre-release build sees exactly what's
   coming rather than a silent or cryptic failure. The `TestStubsAdvertise-
   Session` test enforces this contract.

## Acceptance criteria status

| Criterion                                                | Status |
| -------------------------------------------------------- | :----: |
| `make build` produces `bin/aforo-loadgen`                | done   |
| `--help` lists all 12 subcommands                        | done   |
| `version` prints semver + commit + build date            | done   |
| Every stub exits 0 with "not yet implemented" message    | done   |
| `make test` passes                                       | done   |
| `make lint` passes (`golangci-lint` v1.64.5, 11 linters) | done   |
| CI workflow defined                                      | done   |
| README quickstart documented                             | done   |

## Out of scope (deferred)

- Real `run` execution path — Session 3.
- Scenarios catalog YAML — Session 2.
- Cross-platform release artifacts + Homebrew tap wiring — Session 9.
- Phase 2 dashboard / control-plane server — Session 12.

## Notes for future sessions

- When adding a subcommand, edit three places in one commit: a new file
  under `internal/cli/`, registration in `root.go`, and the
  `expectedSubcommands` slice in `cli_test.go`. The test will fail loudly
  if you miss any of them.
- Build metadata flows through `-ldflags -X` — never read git directly from
  Go code. This keeps `go install` users (who never run `make`) from
  getting a different version-string format than CI users.
