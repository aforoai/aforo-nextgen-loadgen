# Run Phase — 15K TPS, 7-Day Soak, Multi-Machine

The **run** tier is the headline endurance run. Sessions 1–10 deliver
up to ~5K TPS from a single host. Run tier scales to **15K TPS
sustained, 30K burst, 7-day soak** across **8–10 worker nodes** on a
dedicated AWS perf cluster, with **chaos events** through the week and
**cost tracking** so unit economics are measurable release-on-release.

This doc covers the rationale, the infrastructure, the cost envelope,
and the operator workflow. Wire format and chaos types live in
companion docs:

- [chaos-engineering.md](chaos-engineering.md) — what each chaos type
  injects, what it tests, and how recovery works.
- [ops-runbook.md](ops-runbook.md) — abort safely, triage failures,
  escalation paths.

## Why a separate "run" tier

| Tier  | Topology     | TPS   | Duration | Purpose                       |
|-------|-------------:|-------|----------|-------------------------------|
| Crawl | 1 host       | 50    | 5m       | Smoke / CI gate               |
| Walk  | 1–2 hosts    | 2K    | 24h      | Pre-merge realism             |
| Run   | 8–10 hosts   | 15K   | 7d       | Release-gate + acceptance     |

The **run** tier exists because the previous tiers cannot prove the
properties that matter at production scale:

- **Multi-host coordination** — 1000+ tenants partitioned across 8–10
  workers stresses the gateway-orchestration consumer-identity layer
  in a way a single host cannot.
- **7-day soak** — surfaces slow leaks (FD exhaustion, ClickHouse
  partition pressure, Redis eviction churn) that 24h walks miss.
- **Chaos under load** — proves the platform's resilience layer
  (Resilience4j circuit breakers, Kafka DLT routing, dunning escalation)
  in the same run that exercises the headline TPS.
- **Cost visibility** — $/million-events is the unit-economics SLO.
  Lower tiers can't measure it because they don't run long enough or
  generate enough events.

## Topology

```
+-----------------+        mTLS gRPC*           +-------------------+
|  coordinator    | <------- assign / hb ----> |  worker-0  c6i.4xl |
|  c6i.large      |                             +-------------------+
|                 |                             +-------------------+
|  scenario YAML  | <------- assign / hb ----> |  worker-1  c6i.4xl |
|  manifest.json  |                             +-------------------+
|                 |              ...                    ...
|  run.json       |                             +-------------------+
|  cost.json      | <------- assign / hb ----> |  worker-7  c6i.4xl |
|  soak/*.json    |                             +-------------------+
+-----------------+
                              |
                              | (workers send events to)
                              v
                  +---------------------------------+
                  | Aforo perf-aws cluster          |
                  | (target = perf-aws)             |
                  | usage-ingestor / catalog /      |
                  | pricing / billing / analytics   |
                  +---------------------------------+
```

\* The "gRPC" line in the original Session 11 spec is implemented as
**HTTP/2 + mTLS + JSON** over a small `coord` package — same security
properties, same HTTP/2 multiplexing, no protoc dependency. See
`internal/coord/protocol.go` for the rationale and the four endpoints
(`/v1/assign`, `/v1/heartbeat`, `/v1/report`, `/v1/abort`).

## Storage envelope

| Resource                     | 7-day footprint                        |
|------------------------------|----------------------------------------|
| ClickHouse `usage_records`   | ~9B rows × ~500B = **4.5 TB**          |
| ClickHouse `cogs_events`     | ~9B rows × ~256B = **2.3 TB**          |
| Kafka `usage-events` topic   | retention policy capped at 24h         |
| PostgreSQL invoices/credit   | ~hundreds of MB                        |
| S3 archived run artifacts    | ~50 MB per run (run.json + snapshots)  |

**Operator action**: confirm the perf-aws ClickHouse instance has
≥10 TB free EBS *before* launching a run-tier scenario. The
coordinator's pre-flight prompt prints the projected event count and
cost; verify the storage capacity yourself.

## Cost envelope

The cost tracker in `internal/cost` projects an estimate from on-demand
list prices in us-east-1 (DefaultRates). For the headline 15K TPS /
7-day / 8-worker configuration:

| Line                                  | Estimated weekly cost |
|---------------------------------------|----------------------:|
| 8 × c6i.4xlarge worker compute        | ~$913                 |
| 1 × c6i.large coordinator             | ~$14                  |
| 3 × kafka.m5.2xlarge MSK              | ~$322                 |
| 3 × cache.m6g.large Redis             | ~$35                  |
| NAT gateway (1 AZ)                    | ~$8                   |
| Cross-AZ egress (~4.5 TB)             | ~$405                 |
| ClickHouse storage (apportioned)      | ~$84                  |
| **Total**                             | **~$1,781**           |
| **$ / million events** (≈9B events)   | **~$0.20**             |

These numbers are deliberately conservative — actual cost depends on
reservation coverage, savings plans, spot, and per-account negotiated
rates. The cost tracker's output always carries `is_estimate: true`
and a link to AWS Cost Explorer for ground truth (which lags 24h).

## Operator workflow

### 1. Provision the perf cluster

Out of scope for this tool. The ops team provisions:

- 8 worker EC2 instances (c6i.4xlarge), each with the
  `aforo-loadgen` binary and `worker.service` systemd unit installed.
- 1 coordinator instance (c6i.large) on the bastion subnet.
- mTLS material: a CA + per-host server cert for each worker + one
  client cert for the coordinator.
- IAM role granting SSM `SendCommand` permission on the perf cluster
  hosts (so chaos events can run).

The coordinator and workers do **not** authenticate against AWS — all
of that flows through SSM commands invoked from the chaos package.

### 2. Seed the manifest

Run from the bastion (or your laptop, if it has network access to the
target cluster):

```bash
aforo-loadgen seed \
  --scenario scenarios/run-15k-7day.yaml \
  --target perf-aws \
  --out manifest.json
```

This provisions all 500 tenants × ~10K customers × 4 product types
through the platform's REST APIs. Takes ~20 minutes.

### 3. Pre-flight check

```bash
aforo-loadgen coordinator \
  --scenario scenarios/run-15k-7day.yaml \
  --target perf-aws \
  --manifest manifest.json \
  --workers 10.0.0.10:7070,...,10.0.0.17:7070 \
  --tls-cert tls/coord.pem --tls-key tls/coord.key --tls-ca tls/ca.pem \
  --duration 30m       # ← integration test before the real run
```

The coordinator dials every worker first (mTLS handshake check), then
prints the pre-flight summary including projected events, estimated
cost, and the cost-per-million-events SLO datum. Confirm with `yes`.

For the real 7-day run, omit `--duration`.

### 4. Monitor

- **Coordinator stdout** — heartbeat ticks every 5s with per-worker
  state, TPS, p99.
- **Soak snapshots** — `<out>/snapshots/snapshot-YYYY-MM-DDTHH-MM-SSZ.json`
  written every hour. Anomaly detector emits `WARN`/`CRITICAL` lines
  when p99 drifts >10% over the 24h trailing window.
- **Worker `/metrics`** — each worker exposes Prometheus on its
  metrics-addr. The coordinator does not aggregate; an out-of-band
  Prometheus scraper is the production path.

### 5. Wrap up

When the run completes (or `Ctrl-C` on the coordinator triggers an
abort), the coordinator:

1. POSTs `/v1/abort` to every worker (drain in-flight + emit final
   Report).
2. Fetches each worker's final report.
3. Merges into an `AggregateResult` written to `<out>/run.json`.
4. Computes the final cost estimate from the aggregated event count.

Worker dropouts during the run are recorded in
`AggregateResult.WorkersDropped`. The merged report is still produced,
with a note about the gap. The 7-day expectation is that 1–2 workers
drop out; the platform's headline TPS is robust to that. If more than
half drop out, the run aborts and the operator triages.

## Acceptance criteria for the run tier

A run-tier execution is considered "passed" when:

1. **Aggregate TPS ≈ scenario target.** Compute as
   `events_succeeded / duration_seconds`. Tolerance: ±5%.
2. **Latency p99 ≤ assertions.p99_latency_ms_max.** The 15K scenario
   sets this to 1500ms.
3. **Cross-tenant leakage = 0.** The validator's invariant pass
   produces this. No exceptions.
4. **All chaos events recovered.** Every chaos `Outcome` in
   `<out>/run.json` has a non-zero `recovered_at`.
5. **No CRITICAL anomaly alerts.** Soak monitor's
   `<out>/soak-summary.json` has zero `severity: CRITICAL` entries.
6. **Per-million-events cost within ±10% of the prior release.** The
   cost number itself isn't a pass/fail today — it's a tracked SLO
   with manual review on regression.

A failed run does not auto-abort the release; the platform team
inspects `<out>/run.json` + the validator output and decides whether
to ship, re-run, or roll back.
