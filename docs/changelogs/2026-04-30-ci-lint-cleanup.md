# CI Lint Cleanup — Restore green CI for v0.1.0

**Date:** 2026-04-30
**Commit:** (this commit)
**Tag:** v0.1.0 (force-moved to this commit; previous v0.1.0 at 90653b8 produced no GitHub Release artifacts)

## Why

The audit-gap-closure commit (`90653b8`) shipped with 9 known golangci-lint findings the implementing agent classified as "stylistic, not blocking." CI's golangci-lint job exits non-zero on any finding, so both the `ci.yml` and `release.yml` workflows for v0.1.0 failed. No GitHub Release was created.

This commit clears all 9 findings so v0.1.0 actually publishes.

## What changed

| File | Finding | Fix |
|------|---------|-----|
| `internal/validate/lifecycle_correctness.go` | revive var-naming: `stableIdViolations` should be `stableIDViolations` | Renamed at all 3 occurrences (decl, increment, Set call) |
| `internal/coord/worker_server.go` | gocritic unlambda: `func(addr) { return defaultListenTCP(addr) }` | Replaced with bare `var netListen = defaultListenTCP` (still substitutable in tests) |
| `internal/driver/fairness.go:238` | gocritic assignOp: `z = z - X` | Use `z -= X` |
| `internal/runner/distributed.go:356` | gocritic assignOp: same pattern | Use `z -= X` |
| `internal/cli/payments_test.go:13-16` | staticcheck SA4010: appended slice never used (overwritten on line 18) | Deleted dead loop; kept the second loop which actually populates with stable IDs |
| `internal/chaos/chaos_test.go:189-192` | staticcheck SA9003: empty branch (`if !x == false { /* comment */ }`) | Deleted dead conditional; the next conditional already does the real check |
| `internal/coord/worker_handler.go:104-107` | staticcheck SA9003: empty branch on skew check | Replaced with `log.Printf` of the skew, honoring the comment's stated intent ("surface the skew so the operator can see it") — added `log` import |
| `internal/creditnotes/refund_driver.go:65-68` | staticcheck SA9003: empty `if !cfg.Mix.Enabled` | Removed conditional; preserved the explanatory comment as a normal comment |
| `internal/validate/tax_check.go:25-27` | staticcheck SA9003: empty `if t.Engine == "" \|\| t.Engine == TaxMock` | Removed conditional; preserved comment. Also removed now-unused `scenario` import. |

## Verification

```
$ go vet ./...               # clean
$ go build ./...             # clean
$ go test ./... -count=1     # 27 packages OK, 4 with no test files (cmd, config, metrics, version)
$ gofmt -l .                  # 0 findings
$ golangci-lint run --timeout 5m ./...
$ echo $?                     # 0
```

`make build && ./bin/aforo-loadgen version` reports `v0.1.0 (commit <new>, built <ts>)`.

## Tag handling

`v0.1.0` previously pointed at `90653b8` and produced no GitHub Release (the release workflow failed at the lint step before GoReleaser ran). Tag is force-moved to this commit. Pre-launch posture (no customers, no published artifacts) makes the move clean — no semver bump to v0.1.1 needed.

```
git tag -f v0.1.0 -m "v0.1.0 — Initial release. CI green, all 12 sessions complete."
git push --force origin v0.1.0
```

## Why these existed in the first place

The previous gap-closure commit's agent counted "9 findings down from 76" and reported them as out-of-scope stylistic noise. They were not — CI's lint job is the gate. Pre-existing CI redness on Sessions 11 and 12 should have been the tell. Lesson: never declare lint cleanup done without checking CI exit code, not just local count.
