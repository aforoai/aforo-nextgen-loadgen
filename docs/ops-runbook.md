# Ops Runbook — Run-Tier (15K TPS / 7-day) Operations

The run tier exposes operations to real money and real risk. This
runbook is the operator's reference for: triggering a run safely,
aborting cleanly, triaging failures, and escalating.

It complements:

- [run-phase.md](run-phase.md) — architecture and rationale
- [chaos-engineering.md](chaos-engineering.md) — chaos type semantics

## Pre-flight checklist

Before running `aforo-loadgen coordinator --target perf-aws ...`,
confirm:

- [ ] The perf-aws cluster has been freshly provisioned, OR a prior
      run's tenants have been cleaned via `aforo-loadgen seed --clean
      --manifest manifest.json --target perf-aws`.
- [ ] ClickHouse has ≥10TB free EBS — projected events ≈ 9B × 500B =
      4.5TB plus headroom for re-aggregation.
- [ ] All worker EC2 instances are reachable from the coordinator
      host: `for w in $WORKERS; do nc -z $w 7070 || echo "DOWN: $w"; done`.
- [ ] mTLS material is in place: cert files exist, key files are mode
      0600, CA bundle includes the worker certs' issuer.
- [ ] AWS CLI is configured on the bastion with SSM `SendCommand`
      permission on the perf cluster instance ids.
- [ ] PagerDuty escalation policy is paged-on for the chaos events
      window — false alarms during chaos are noise, but the on-caller
      should know the run is live.

## Launch

### Standard 7-day run

```bash
export AFORO_ADMIN_TOKEN=...   # required by workers for per-event auth fallback
ssh bastion

aforo-loadgen coordinator \
  --scenario scenarios/run-15k-7day.yaml \
  --target perf-aws \
  --manifest /home/ops/manifest.json \
  --workers 10.0.0.10:7070,10.0.0.11:7070,...,10.0.0.17:7070 \
  --tls-cert /etc/aforo-loadgen/tls/coord.pem \
  --tls-key  /etc/aforo-loadgen/tls/coord.key \
  --tls-ca   /etc/aforo-loadgen/tls/ca.pem \
  --tls-server-name worker.perf.aforo.internal \
  --out /var/lib/aforo-loadgen/run-15k-$(date +%Y%m%d) \
  | tee /var/log/aforo-loadgen/run-15k-$(date +%Y%m%d).log
```

The pre-flight prompt prints:

```
═══ pre-flight check ═══
scenario           run-15k-7day
target             perf-aws
workers            8
tenants            500
target_tps         15000
duration           168h
projected events   9.07B
estimated cost     $1781.42 USD (us-east-1)
  worker compute   $913.92
  msk + redis      $357.84
  egress (4222 GB) $379.98
  ch storage (...) $84.00
per million events $0.1964
note               Estimate based on on-demand list prices in us-east-1; ...

About to send 15000 TPS to perf-aws. Generates ~9.07B events / 168h0m0s.
Estimated cost: $1781.42. Continue? [yes/NO]
```

Type `yes` to launch. Anything else aborts.

### Subset run (integration test)

Before the real 7-day fire, do a 30-minute subset run against the same
infra to prove the multi-machine wiring works:

```bash
aforo-loadgen coordinator \
  --scenario scenarios/run-15k-7day.yaml \
  --target perf-aws \
  --manifest manifest.json \
  --workers 10.0.0.10:7070,10.0.0.11:7070 \   # 2 workers — faster setup
  --duration 30m \
  --yes                                         # skip prompt for automation
  ...
```

A successful subset run is a precondition for the real launch.

## Monitoring during the run

### What to watch

| Signal                               | Where                             | Action                          |
|--------------------------------------|-----------------------------------|----------------------------------|
| Worker heartbeats (5s)               | Coordinator stdout                | Worker dropouts logged           |
| Soak hourly snapshots                | `<out>/snapshots/*.json`          | Anomaly alerts in same files     |
| Aggregate p99 trend                  | `<out>/soak-summary.json`         | Reject if drift >25% sustained   |
| Per-worker Prometheus                | `:9095/metrics` on each worker    | Grafana dashboard `aforo-loadgen` |
| ClickHouse storage                   | AWS Console / `df -h /clickhouse` | Stop run if <500GB free          |
| Kafka consumer lag                   | MSK metrics                       | Diagnose if >10K sustained       |
| AWS Cost Explorer                    | Console (24h-lag)                 | Compare to projected             |

### Anomaly alert reference

The soak monitor emits two alert kinds:

| Kind                  | Severity | Trigger                                                    | Auto-action |
|-----------------------|----------|------------------------------------------------------------|-------------|
| `p99_drift`           | WARN     | Latest p99 ≥110% of trailing-24h median                    | None        |
| `p99_drift`           | CRITICAL | Latest p99 ≥125% of trailing-24h median                    | None        |
| `high_failure_rate`   | WARN     | Latest snapshot's failed/total > 1% (configurable)         | None        |

The monitor never aborts the run automatically. The on-caller decides
whether to abort based on:

- One-off CRITICAL during a chaos window — likely chaos-induced; let
  it ride.
- Sustained WARN over 3+ snapshots after a chaos window — likely a
  real regression; abort and triage.
- Multiple `high_failure_rate` outside chaos windows — the platform
  is dropping events; abort.

## Aborting the run safely

### Coordinator-driven (preferred)

`Ctrl-C` on the coordinator process triggers `signal.NotifyContext` →
the coordinator fans out `/v1/abort` to every worker, drains
in-flight, fetches final reports, and writes the merged `run.json`
+ cost estimate to disk.

This is the only way to abort that produces a complete,
analyzable artifact.

### Worker-individual (only if a worker hangs)

If a single worker is unresponsive but the rest are healthy:

```bash
# from the coordinator host:
curl --cert client.pem --key client.key --cacert ca.pem \
     -X POST -d '{"run_id":"...","reason":"manual abort"}' \
     https://10.0.0.13:7070/v1/abort
```

The coordinator detects the dropout via heartbeat and continues with
the survivors. The worker's tenant range is marked incomplete in the
final report.

### Hard reset (catastrophic — use last)

If the coordinator AND the workers are unresponsive (rare — usually
indicates a bastion-side problem):

```bash
# Stop every worker via SSM:
for ID in i-PERF-WORKER-1 ... i-PERF-WORKER-8; do
  aws ssm send-command --instance-ids $ID \
    --document-name AWS-RunShellScript \
    --parameters commands="sudo systemctl stop aforo-loadgen-worker"
done
```

This loses any in-flight chaos faults that the schedulers would have
recovered. Manually verify and undo:

```bash
# Each worker:
sudo iptables -L OUTPUT --line-numbers -n | grep aforo-loadgen-chaos
sudo tc qdisc show dev eth0
# Remove anything tagged 'aforo-loadgen-chaos-*'.
```

## Failure triage

### "Worker rejected assignment"

Coordinator log line:
```
worker 10.0.0.13:7070 rejected assignment: scenario invalid: ...
```

**Diagnosis**: scenario YAML failed to parse on the worker, OR a
schema-version mismatch between coordinator and worker, OR the
scenario validator on the worker side rejected something the
coordinator's validator accepted (unlikely — they're the same code).

**Fix**: confirm worker `aforo-loadgen --version` matches coordinator.
Re-deploy whichever is older.

### "TLS handshake error"

**Diagnosis**: the worker server cert is signed by a CA the
coordinator doesn't trust, OR the SAN doesn't include the worker's
hostname/IP, OR the worker's CA bundle doesn't trust the
coordinator's client cert.

**Fix**: regenerate certs with the right SANs. Both sides should
trust the same root CA.

### "p99 drift CRITICAL outside chaos window"

**Diagnosis**: real regression in the platform OR chaos event from
prior window didn't recover.

**First check** — chaos timeline in `<out>/run.json`. Every event
should have non-zero `recovered_at`. Any event with
`recovery_error` is a real failure.

**Recovery action**:
- Confirm tc/iptables/redis are clean on each chaos target host.
- If a chaos didn't recover, manually undo per the type's
  recovery command (see `chaos-engineering.md`).
- If chaos is clean, check the platform's own breaker dashboards —
  one downstream service may be slow.

### "Worker dropout"

**Diagnosis**: worker process died, OR network partition, OR the
host ran out of disk for the events log.

**Recovery action**:
- SSH to the worker. `journalctl -u aforo-loadgen-worker --since '5m ago'`.
- If the process died with OOM, increase the worker's memory or
  reduce the scenario's `--workers` per-pool.
- If the disk is full, the events log capped at 1000 events —
  the cap may have grown via Session 8 changes; truncate the
  legacy log files in `/var/lib/aforo-loadgen/*/events.jsonl`.
- Re-add the worker to the pool by restarting `aforo-loadgen-worker`;
  the coordinator will not re-assign tenants to it (single-shot
  assignment per run), but the run continues with the survivors.

### "ClickHouse storage filling"

**Diagnosis**: `usage_records` retention is per-table TTL on the
ClickHouse side. If the soak generated more than the cluster's
provisioned EBS allows, the table's MergeTree blocks new inserts and
analytics-service writes start failing.

**Recovery action**:
- Stop the run via Coordinator-driven abort.
- On ClickHouse host: `ALTER TABLE usage_records DROP PARTITION
  toYYYYMM('<earliest day>')` to free space.
- Validate post-purge invariants: per-tenant aggregates should still
  be consistent against the unaffected partitions.
- Restart with a shorter `--duration` or larger storage.

## Escalation

| Issue                                          | Notify                            |
|------------------------------------------------|-----------------------------------|
| Run aborts within first 30 min                 | Platform on-call (PagerDuty)      |
| ClickHouse storage at risk                     | Data infra on-call                |
| AWS account quota / spend alarm                | Finance ops (#aforo-cost-alerts)  |
| Suspected data corruption / cross-tenant leak  | Security on-call (paged + Slack)  |
| Chaos event failed to recover                  | Platform on-call (paged)          |
| All workers dropped out                        | Platform + Network on-call        |

The **only** issue type that requires *immediate* page is suspected
cross-tenant leakage. Aforo is a multi-tenant SaaS billing platform;
even one leaked event during a chaos window is a data-isolation
breach and triggers the security incident process.

All other issues triage during business hours; the run can sit
half-aborted for hours without catastrophic effect (worst case
is paying ~$10/hour for idle compute).

## Post-run

1. Archive `<out>/` to `s3://aforo-loadgen-runs/run-15k-YYYYMMDD/`.
2. Run `aforo-loadgen validate --run <out> --target perf-aws` to
   produce the validator's invariant report.
3. Run `aforo-loadgen report --run <out> --validate <validate-out>`
   to produce the HTML report for the platform team.
4. Compare the cost-per-million-events to the prior release's
   number. Flag any >10% regression for review.
5. Tear down per-tenant data via `aforo-loadgen seed --clean
   --manifest manifest.json --target perf-aws`.
6. Record the run in the platform's release tracker with a link to
   the S3 archive.

## Frequently asked questions

**Q: Can I run multiple coordinator runs concurrently against the
same target?**
A: No. The coordinator does not coordinate across other coordinators
— each one assumes it owns the cluster. If you need parallel runs,
provision separate perf-aws-2, perf-aws-3 clusters.

**Q: What's the recovery time after an aborted run?**
A: ~5 min for the coordinator + workers to drain. ~20 min for
`aforo-loadgen seed --clean` to delete tenants. Storage retention
is whatever ClickHouse + PG are configured for (typically days).

**Q: Will SIGSTOPing a worker confuse the coordinator?**
A: Yes — the coordinator detects "no heartbeat" and marks the worker
dropped after `--dropout-timeout` (default 30s). SIGCONTing it
afterward does not re-add it; restart the worker process to
re-register. (And realistically, don't SIGSTOP workers.)

**Q: Where does my cost estimate come from?**
A: `internal/cost/tracker.go`. The DefaultRates table is on-demand
us-east-1 prices as of 2026-04-30. To use a different rate card
(savings plan, reservation, eu-west-1), supply a `RateCard` JSON to
the (planned) `--rate-card` flag — for now, edit DefaultRates and
rebuild.
