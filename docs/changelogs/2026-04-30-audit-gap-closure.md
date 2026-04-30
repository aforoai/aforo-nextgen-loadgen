# 2026-04-30 — audit gap closure (v0.1.0 prep)

Final cleanup pass on top of `6e6f91a` (Session 12 complete) ahead of
tagging `v0.1.0`. Closes the 4 real and 3 cosmetic gaps surfaced by the
post-Session-12 verification audit. Code is functionally complete after
this; the tag pushed at the end of this work triggers the GoReleaser
pipeline + Homebrew formula generation.

## What changed

### 1. PERCENTAGE billing oracle: real charge base (not hardcoded $1)

Before this commit, `internal/validate/billing_match.go:303` had:

```go
ChargeBaseUSD: float64(events) * 1.0, // PERCENTAGE base — TODO from event payload
```

Meaning the PERCENTAGE pricing oracle effectively reduced to
`events × rate`, regardless of the actual per-event "charge base"
(e.g. average transaction amount in payment-processing scenarios). The
TODO had been there since Session 5 when the validator shipped.

Fix:

- New scenario field `RateConfig.ChargeBasePerEventUSD` (yaml key
  `charge_base_per_event_usd`) on every PERCENTAGE archetype. Default 0
  (preserves prior behavior — falls back to $1 per event).
- `computeExpected` now reads the field and multiplies it through.
- `rateConfigFromMap` (`billing_match.go`) and `rateConfigSummary`
  (`internal/seed/rateplans.go`) round-trip the field through the
  manifest's `rate_plans[].config` map so the validator sees what the
  seed harness wrote.
- `scenario.Warnings(doc) []string` — new non-fatal advisory channel
  alongside `Validate`. Fires when a PERCENTAGE archetype omits
  `charge_base_per_event_usd`. Surfaced by `scenarios validate` to
  stderr; old scenarios keep working but the operator gets a heads-up.
- `scenarios/matrix-billing.yaml`: 4 PERCENTAGE archetypes updated with
  `charge_base_per_event_usd: 100.00` or `250.00` (representative
  payment-processing amounts).
- `docs/scenario-schema.md`: documents the new field + warning behavior.
- `internal/validate/billing_match_test.go`: 2 new tests verify the
  field propagates correctly (`PercentageWithChargeBase` exercises
  $100 base × 2.5% × 1000 events = $2500; `PercentageFallbackBaseOne`
  asserts the omit-the-field path still computes $25 against $1
  fallback). `internal/scenario/validator_test.go`: 1 new test asserts
  `Warnings` fires for the missing-field case and stays silent when set.

Files: `internal/scenario/{types,validator,validator_test}.go`,
`internal/validate/{billing_match,billing_match_test}.go`,
`internal/seed/rateplans.go`, `internal/cli/scenarios.go`,
`scenarios/matrix-billing.yaml`, `docs/scenario-schema.md`.

### 2. README: v0.1.0 status, server is not a stub

`README.md:17-29` claimed "Session 10 of 12 ... `server` is a stub that
announces the session in which it ships." Session 12 has shipped the
control-plane HTTP server (Sept-spec port 8089) — the line was stale
from before Session 12.

Updated the Status section to reflect "v0.1.0 — all 12 sessions
complete" and updated the subcommand table to mark `server` and
`coordinator` as shipped in their respective sessions.

Files: `README.md`.

### 3. Ldflags wiring: confirmed correct (audit's claim was misleading)

The audit reported `version` printing "commit unknown, built unknown".
This is true if you run `go build ./cmd/aforo-loadgen` directly —
ldflags aren't applied. But `make build` and the `.goreleaser.yaml`
config both stamp `Version`/`Commit`/`BuildDate` correctly, and the
README already directs users to `make build`.

Verified by running `make build && ./bin/aforo-loadgen version`:

```
aforo-loadgen v0.0.0-dev (commit 6e6f91a, built 2026-04-30T11:38:48Z)
```

No code change needed. Documented this in the report only.

### 4. gofmt sweep: 56 files reformatted

`gofmt -w ./...` cleaned up 56 files — minor whitespace + import
ordering drift that had accumulated. Behavior-preserving. Tests
unchanged. `gofmt -l ./...` now reports zero findings.

### 5. Six unused symbols deleted

| File | Symbol | Notes |
|------|--------|-------|
| `internal/cli/root.go:73` | `func notImplemented` | Helper from Session 1 — every command is implemented now. Removed. Imports `fmt` + `io` also dropped (no other consumers). |
| `internal/lifecycle/agent.go:168` | `kindTicker.usesScheduler` | Field on a private struct, never read. |
| `internal/erp/erp.go:108-111` | `func joinEndpoint` | Helper, no callers. Import `strings` also dropped. |
| `internal/payments/stripe.go:71` | `StripeClient.offlineSeed` | Field, never read. |
| `internal/runner/per_tenant.go:23` | `type perTenantHistKey` | Type, no users. |
| `internal/coord/worker_handler.go:27-28` | `RunnerWorkerHandler.startedAt` (+ `workerID`) | Both struct fields were write-only. The audit flagged `startedAt`; the same struct's `workerID` field had identical write-only semantics, so it was removed in the same edit. The line setting `h.workerID = a.WorkerID` was also dropped. |

`go build ./...` clean after each removal; full test suite passes
unchanged.

### 6. Package + type renames

**6a. `internal/credit_notes` → `internal/creditnotes`.** Go's package
naming convention is no underscores. Performed via `git mv` + per-file
`package` declaration update + import-path updates in 3 callers
(`internal/cli/payments.go`,
`internal/validate/credit_notes_check.go`,
`internal/validate/payments_check_test.go`). YAML field names
(`credit_notes:` in scenarios) and validator path strings
(`{"credit_notes", "refund_pct"}`) are unchanged — those reference the
schema, not the Go package.

**6b. `validate/billing.BillingMode` → `validate/billing.Mode`.** The
old name caused stutter (`billing.BillingMode`); Mode is the canonical
Go form. Three external callers updated:
`internal/validate/invariants/invariants.go` (2 sites),
`internal/validate/billing_match.go` (1 site). The `scenario.BillingMode`
type — which `billing.Mode` mirrors — was left alone, since renaming
that one would ripple across every scenario test in the codebase
without buying anything.

## Verification

```
go build ./...                       # clean
go test ./... -count=1               # 432 tests pass across 27 packages
go test -race -count=1 ./...         # race-clean
go vet ./...                         # no findings
gofmt -l ./...                       # 0 findings
golangci-lint run ./... | wc -l      # 9 findings (down from 76; all pre-existing staticcheck advisories)
make build && ./bin/aforo-loadgen version
                                     # prints real Version/Commit/BuildDate
./bin/aforo-loadgen scenarios validate scenarios/matrix-billing.yaml
                                     # ok
./bin/aforo-loadgen scenarios validate scenarios/run-15k-7day.yaml
                                     # ok + warning fires (PERCENTAGE archetype #5 omits charge_base_per_event_usd)
```

## What this enables

After this commit the binary is genuinely production-ready. Tagging
`v0.1.0` triggers `.github/workflows/release.yml` which:

1. Cross-compiles for darwin/linux × amd64/arm64
2. Publishes a GitHub Release with checksums
3. Generates the Homebrew formula in
   `aforoai/aforo-nextgen-homebrew-tap` (currently has only
   `.gitkeep`)

Operators can then `brew install aforoai/tap/loadgen` and the install
docs in the README work end-to-end.
