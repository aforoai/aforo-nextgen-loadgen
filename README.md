# aforo-nextgen-loadgen

A Go CLI for load-testing the [Aforo NextGen](https://github.com/aforoai)
platform's ingestion pipeline at scale.

**Target:** sustain 15K TPS across 500 simulated tenants. Crawl-Walk-Run.

**Coverage:** all 4 product types (API, Agentic API, AI Agent, MCP Server),
all 9 gateway adapter types, all 6 pricing models, all 3 billing modes
(POSTPAID / PREPAID / HYBRID), full subscription lifecycle, payment + tax +
ERP flows, and 6 negative-path categories.

This repo lives next to the platform monorepo. For the platform's
architecture, services, and conventions see
[`Nextgen_Aforo/CLAUDE.md`](../CLAUDE.md).

## Status

Session 1 of 12. The command tree is in place; most subcommands are stubs
that announce the session in which they ship. The `version` subcommand is the
only fully-implemented command.

| Subcommand  | Ships in   | What it does                                                      |
| ----------- | ---------- | ----------------------------------------------------------------- |
| `seed`      | Session 2  | Seed tenants, products, rate plans, subscriptions for a run.      |
| `scenarios` | Session 2  | List, describe, and inspect built-in scenarios.                   |
| `run`       | Session 3  | Drive a load-test scenario against a target.                      |
| `validate`  | Session 4  | Static-validate a scenario or config file with no traffic.        |
| `lifecycle` | Session 5  | Drive subscription lifecycle transitions.                         |
| `payments`  | Session 6  | Drive payment, tax, and ERP integration flows.                    |
| `replay`    | Session 7  | Replay captured event traffic from a recorded log.                |
| `e2e`       | Session 8  | End-to-end smoke flows against a live target.                     |
| `report`    | Session 10 | Render results from a completed run.                              |
| `doctor`    | Session 11 | Diagnose local environment and target reachability.               |
| `server`    | Session 12 | Control-plane server (dashboard + multi-node coordinator).        |
| `version`   | Session 1  | Print semver, commit SHA, and build date.                         |

## Install

### Homebrew (Session 9+)

```bash
brew install aforoai/tap/loadgen
```

The tap is wired in Session 9 once the first signed release ships. Until
then, install via `go install`.

### `go install`

```bash
go install github.com/aforoai/aforo-nextgen-loadgen/cmd/aforo-loadgen@latest
```

### From source

```bash
git clone https://github.com/aforoai/aforo-nextgen-loadgen.git
cd aforo-nextgen-loadgen
make build
./bin/aforo-loadgen --help
```

## Quickstart

```bash
aforo-loadgen --help                              # see all subcommands
aforo-loadgen version                             # print build metadata
aforo-loadgen scenarios                           # list scenarios (Session 2)
aforo-loadgen run --target https://usage-ingestor.aforo.space \
                  --config ./loadgen.yaml        # run a scenario (Session 3)
aforo-loadgen report --run-id <id>                # render results (Session 10)
```

## Global flags

| Flag           | Default | Purpose                                                       |
| -------------- | ------- | ------------------------------------------------------------- |
| `--target`     | _none_  | Base URL of the platform under test.                          |
| `--config`     | _none_  | Path to a loadgen YAML config file.                           |
| `--log-level`  | `info`  | `debug`, `info`, `warn`, `error`.                             |
| `--json-logs`  | `false` | Emit logs as newline-delimited JSON.                          |

## Development

```bash
make build      # compile to bin/aforo-loadgen
make test       # run unit tests with -race
make lint       # run golangci-lint (run `make lint-install` first)
make fmt        # gofmt -s
make tidy       # go mod tidy
```

CI runs `make build`, `make test`, and `golangci-lint` on every PR.

### Adding a subcommand

1. Add a file under `internal/cli/<name>.go` exporting `new<Name>Command`.
2. Register it in `internal/cli/root.go`.
3. Append the name to `expectedSubcommands` in `internal/cli/cli_test.go`.
4. Update the table at the top of this README.

The Session 1 acceptance test (`TestEverySubcommandExitsZero`) enforces that
every registered subcommand exits 0 with output.

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Persona model

Three personas show up in scenarios and docs:

- **Aforo** — the platform itself (this repo's _target_).
- **SmartAI** — a simulated tenant of Aforo.
- **Acme** — a simulated end-customer of SmartAI.

These names are used consistently across fixtures, README examples, and the
scenarios catalog.
