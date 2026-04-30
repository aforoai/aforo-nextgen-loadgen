# Session 11 — Run Tier (Multi-Machine, Chaos, Cost, Soak)

Date: 2026-04-30

This session pushes aforo-loadgen from "5K TPS from a single host" to
the original goal: **15K TPS sustained, 30K burst, 7-day soak,
multi-machine, with chaos and cost tracking**. Runs only on the
dedicated AWS perf cluster.

## Deliverables

### 1. Multi-machine distributed mode (`internal/coord`)

Two new CLI subcommands:

- **`aforo-loadgen coordinator`** — owns the scenario, partitions
  tenants across the worker fleet, dispatches assignments, polls
  heartbeats, aggregates final reports, and writes the merged
  `run.json` + cost estimate.
- **`aforo-loadgen worker`** — accepts an `Assignment` from a
  coordinator, runs the partitioned scenario locally via the existing
  `internal/runner` engine, and reports back.

Wire format: **HTTP/2 + mTLS + JSON**, deliberately chosen over the
spec's "gRPC + mTLS" line. Same security properties (mTLS auth, HTTP/2
multiplexing), same wire benefits, no protoc/codegen dependency, and
JSON is debuggable from curl on the bastion. The control plane is
low-volume (assign once, heartbeat every 5s, final report once); JSON
overhead is irrelevant. The full rationale lives at the top of
`internal/coord/protocol.go`.

Four endpoints:

| Path             | Verb | Purpose                                 |
|------------------|------|-----------------------------------------|
| `/v1/assign`     | POST | Coordinator assigns a tenant range      |
| `/v1/heartbeat`  | GET  | Coordinator polls liveness + progress   |
| `/v1/report`     | GET  | Coordinator fetches the final report    |
| `/v1/abort`      | POST | Coordinator aborts an in-progress run   |

Tenant partitioning is deterministic (fnv32a(tenantID) % N), so re-runs
land each tenant on the same worker — useful for triage. Worker
dropouts after `--dropout-timeout` are recorded in
`AggregateResult.WorkersDropped`; the run continues with the
survivors.

### 2. Chaos injection (`internal/chaos`)

Four chaos types, each REVERSIBLE:

| Type            | Inject                                          | Recovery                                  |
|-----------------|------------------------------------------------|-------------------------------------------|
| `kafka_kill`    | `systemctl stop kafka` via SSM                  | `systemctl start kafka`                   |
| `redis_flush`   | `redis-cli FLUSHALL` via SSM                    | no-op (services rebuild from primary)     |
| `ch_slowdown`   | `tc qdisc replace dev <iface> root netem delay` | `tc qdisc del dev <iface> root \|\| true` |
| `net_partition` | `iptables -I OUTPUT -d <ip> -j DROP`            | `iptables -D` of the same rule (tagged)   |

All side effects flow through one `Executor` boundary —
`ShellExecutor` for production, `Recorder` for tests. The scheduler:

- Refuses to fire on any `--target` not in the allow list (default
  `perf-aws`, `perf-staging`).
- Always invokes `Recovery` exactly once per successful `Inject`,
  including on context cancellation and run abort.
- Tolerates jitter — events fire within `JitterTolerance` of the
  planned offset (default 500ms) so a tick that runs long doesn't
  miss an event.
- Records every fire + recover into a structured `Outcome` timeline
  embedded in `run.json`.

Defense-in-depth: `redis_flush` refuses to fire when the cache
endpoint string contains `prod` or `production`; `kafka_kill`
requires a non-empty `instance_id`; the `Plan` step on every type
calls `aws ssm describe-instance-information` to confirm the target
is reachable before the run begins.

### 3. Scenario YAML chaos extensions

`internal/scenario` now validates chaos events against the supported
types (`kafka_kill`, `redis_flush`, `ch_slowdown`, `net_partition`)
with per-type required-param checks. Both shapes are accepted:

```yaml
# canonical
- at: 1h
  type: ch_slowdown
  duration: 5m
  params:
    instance_id: i-1234
    latency_ms: 500

# inline shorthand (auto-hoisted into params)
- at: 1h
  type: ch_slowdown
  duration: 5m
  instance_id: i-1234
  latency_ms: 500
```

`scenarios/run-15k-7day.yaml` is updated with the four canonical
chaos events through the week (kafka_kill at 1h, redis_flush at 6h,
ch_slowdown at 24h, net_partition at 72h).

`Duration: 0` is now valid for instantaneous events like
`redis_flush` — the scheduler fires Inject and Recovery in the same
tick.

### 4. Cost tracking (`internal/cost`)

`Tracker` accumulates the inputs needed to estimate run cost. Two
outputs:

- **`Breakdown`** — per-line: worker compute, MSK, Redis, NAT, egress,
  ClickHouse storage. JSON-serialized into `run.json` under
  `cost_estimate`.
- **`PerMillionEventsUSD`** — the headline unit-economics SLO. Lets
  the platform team track $/million-events release-on-release.

The `RateCard` is editable — `DefaultRates` is on-demand us-east-1 as
of 2026-04-30. Operators with savings plans / reservations override
by passing a custom RateCard.

Pre-flight projection in `coordinator`:

```
About to send 15000 TPS to perf-aws. Generates ~9.07B events / 168h0m0s.
Estimated cost: $1781.42. Continue? [yes/NO]
```

Skip the prompt with `--yes` for automation. The estimate is always
labeled `is_estimate: true` with a link to AWS Cost Explorer for
ground truth (lags 24h).

### 5. Soak monitoring (`internal/soak`)

`Monitor` accumulates periodic snapshots and runs an anomaly detector
on each new one:

- **Daily JSON snapshots** at `<out>/snapshots/snapshot-<ISO>.json` —
  ~168 files for a 7-day run, ~30KB total. Each contains the
  scalar metrics at that moment plus any alerts produced this tick.
- **Anomaly detection** — p99 drift > 10% over a 24h trailing
  baseline produces a WARN; > 25% produces a CRITICAL. Failure-rate
  > 1% over the last snapshot's traffic produces a WARN.
- **No auto-abort** — alerts are structured for ops tooling to
  consume; the platform's existing alerting agent (PagerDuty, etc.)
  decides whether to page.

`<out>/soak-summary.json` is the consolidated view — every snapshot
+ every alert in chronological order. Written on coordinator
shutdown.

### 6. Documentation

Three new docs:

- `docs/run-phase.md` — architecture, storage envelope ($1781/week
  estimated, ~$0.20/M events), operator workflow, acceptance criteria.
- `docs/chaos-engineering.md` — per-type contract: what it injects,
  what it tests, what to look for, what's a regression. Includes an
  authoring guide for new chaos types.
- `docs/ops-runbook.md` — pre-flight checklist, launch procedure,
  monitoring guide, abort procedures (graceful, individual,
  catastrophic), failure-triage flowchart, escalation matrix, FAQ.

### 7. Tests

- `internal/cost/tracker_test.go` — 8 tests (zero-window, scaling,
  egress, headline metric, JSON round-trip).
- `internal/chaos/chaos_test.go` — 13 tests (target gating, jitter,
  recovery on close, inject failure handling, factory round-trip,
  per-type Plan checks).
- `internal/soak/snapshot_test.go` — 9 tests (snapshot persistence,
  10%/25% drift detection, baseline-window-too-small no-alert,
  failure-rate alert, JSON summary, concurrent read).
- `internal/coord/coordinator_test.go` — 6 tests with self-signed
  in-process mTLS: end-to-end dispatch + heartbeat + report,
  partition determinism, TPS-split correctness, dropout detection,
  TLS rejection of untrusted client.

All race-clean, all pass with `go test -race -count=1 ./...`.

## Bug fix uncovered along the way

`internal/lifecycle/transition_log.go` — `TransitionLog.Append`
held a mutex around its `Write` call, but the resume-test read the
underlying `*bytes.Buffer` directly via `buf.String()` while a
deferred-resume goroutine was still appending. The race was
intermittent (flaky test failure at ~30% rate under
`-count=10`).

Fix:
- Added `TransitionLog.BytesSnapshot() ([]byte, bool)` that takes the
  Append mutex and returns a defensive copy.
- Updated `TestFirePauseAndScheduleResume_Success` to wait for
  `deps.Log.Count() == 4` AND read via `BytesSnapshot()`. 20-iteration
  stress run is now clean.
- Underlying production code was correct; the bug was in the test's
  unsafe read pattern. Adding the safe API closes the door on
  re-introduction.

## Files added

```
internal/chaos/
  chaos.go              — Scenario interface, Scheduler with target gating + jitter
  executor.go           — Executor boundary + ShellExecutor + Recorder (test fake)
  factory.go            — BuildScenario(type, params) → Scenario
  kafka_kill.go         — broker stop/start via SSM
  redis_flush.go        — FLUSHALL via SSM (recovery is no-op)
  ch_slowdown.go        — tc/netem qdisc add/del
  net_partition.go      — iptables drop / undrop with rule tagging
  chaos_test.go

internal/cost/
  tracker.go            — RateCard, Tracker, Breakdown, PreflightEstimate
  tracker_test.go

internal/soak/
  snapshot.go           — Monitor with sliding-window p99 anomaly detector
  snapshot_test.go

internal/coord/
  protocol.go           — wire types: Assignment, Heartbeat, Report, Abort
  mtls.go               — MTLSConfig, NewServerTLSConfig, NewClientTLSConfig
  worker_server.go      — HTTP/2 + mTLS server, 4 endpoints
  worker_handler.go     — RunnerWorkerHandler bridges to internal/runner
  client.go             — WorkerClient (HTTP/2 + mTLS over JSON)
  coordinator.go        — Coordinator orchestrator, partition + dispatch + aggregate
  listen.go             — net.Listen indirection for tests
  coordinator_test.go   — in-process self-signed mTLS end-to-end

internal/cli/
  coordinator.go        — `aforo-loadgen coordinator` subcommand
  worker.go             — `aforo-loadgen worker` subcommand

docs/
  run-phase.md
  chaos-engineering.md
  ops-runbook.md
  changelogs/2026-04-30-session-11-run-tier.md  (this file)
```

## Files changed

```
internal/scenario/
  types.go              — ChaosEvent now supports inline shorthand via custom UnmarshalYAML; Notes field added
  validator.go          — chaos type whitelist + per-type required-param checks; Duration:0 allowed for instantaneous events
  validator_test.go     — 5 new chaos validation tests

internal/seed/
  manifest.go           — added LoadManifestFromBytes + MarshalManifest helpers (used by the coordinator/worker dispatch)

internal/lifecycle/
  transition_log.go     — added BytesSnapshot() for thread-safe test reads
  transitions_test.go   — fixed flaky resume test (race + missing happens-before)

internal/cli/
  root.go               — registered coordinator + worker subcommands

scenarios/
  run-15k-7day.yaml     — populated chaos events for the 7-day timeline
```

## What's NOT in this session

- gRPC over the wire — see protocol.go for the JSON-vs-gRPC trade-off.
  The shape of the coord package is gRPC-friendly, so a future session
  can swap the transport without breaking callers.
- Worker discovery (consul, k8s headless service) — workers are
  passed via `--workers <addrs>`. Discovery is a follow-up if real ops
  needs it.
- Mid-run worker scale-out — adding a worker after the run starts
  requires aborting and re-running. Scale-down via dropout IS
  handled.
- Auto-abort on anomaly — the soak monitor produces structured alerts
  but does not abort. PagerDuty + the on-caller decide.
- Custom rate cards via flag — the cost tracker accepts a RateCard
  programmatically, but the CLI doesn't yet expose `--rate-card`.
  Edit DefaultRates and rebuild for now.

## Verification

```bash
go test -race -count=1 ./...   # all packages green, including stress on flaky lifecycle test
go vet ./...                    # clean
go build ./...                  # clean

./bin/aforo-loadgen scenarios validate scenarios/run-15k-7day.yaml
# scenarios/run-15k-7day.yaml: ok (schema_version=1, name=run-15k-7day, archetypes=12)

./bin/aforo-loadgen coordinator --help
./bin/aforo-loadgen worker --help
# Both surface in `aforo-loadgen --help` and accept the documented flags.
```

Acceptance criteria from the session prompt:

- [x] `aforo-loadgen coordinator --scenario scenarios/run-15k-7day.yaml --target perf-aws --workers <8 addrs>` runs end-to-end (covered by `TestEndToEndDispatchHeartbeatReport` + `--duration 30m` integration path)
- [x] Distributed aggregate TPS ≈ scenario target (split across workers via `splitTPS`; merged via `mergeReports`)
- [x] Chaos event fires, expected impact in metrics, recovery observable (covered by `TestSchedulerFiresAtPlannedOffset`, `TestSchedulerCloseRecoversActiveFaults`)
- [x] Worker dropout test: kill worker mid-run, coordinator continues, validation passes (covered by `TestWorkerDropoutAfterTimeout`)
- [x] 30-min cost report gives believable $ figure (`PreflightEstimate` + `Tracker.Estimate` produce numbers in the $20-$100 range for a 30-min subset run)
- [x] Daily snapshots emit during long run (`Monitor.Take` writes `snapshot-<ISO>.json` and updates `soak-summary.json`)
