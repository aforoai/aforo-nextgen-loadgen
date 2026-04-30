# Chaos Engineering

aforo-loadgen ships four chaos scenarios that inject infrastructure
faults during a run-tier scenario. Each is **reversible**: every Inject
returns a Recovery callback that the scheduler invokes when the event's
duration elapses (or the run ends — including on abort or panic).

This doc is the operator-facing contract for each chaos type: what it
does, what it tests, the SSM-targetable parameters it expects, and the
recovery semantics.

## Design principles

1. **Reversibility is a hard contract.** The `chaos.Scheduler` calls
   `Recovery` exactly once per successful Inject. A run that exits
   without recovering is a bug in the scheduler, not a feature.
2. **Target gating is fail-closed.** The scheduler refuses to fire any
   event when the run's `--target` name is not in the
   `AllowedTargets` list. Default allow list: `perf-aws`, `perf-staging`.
3. **Side effects flow through one boundary.** All chaos types call
   `Executor.Run(ctx, label, name, args...)` rather than shelling out
   directly. Production uses `ShellExecutor`; tests inject a `Recorder`
   for deterministic assertions.
4. **No retries on injected faults.** A failed Inject is logged and the
   event is skipped — re-applying a partial fault risks leaving the
   target in an unrecoverable state.

## Type matrix

| Type            | What it injects                              | What it tests                                                                |
|-----------------|----------------------------------------------|------------------------------------------------------------------------------|
| `kafka_kill`    | `systemctl stop kafka` on one broker         | Kafka producer retry + idempotent commit + DLT routing                       |
| `redis_flush`   | `redis-cli FLUSHALL` against the cache       | BillingHierarchyEnricher cold-cache rebuild + Kong policy re-warm            |
| `ch_slowdown`   | `tc qdisc … netem delay <N>ms` on CH host    | Resilience4j circuit breaker + AnalyticsQueryRouter PG fallback              |
| `net_partition` | `iptables -I OUTPUT -d <ip> -j DROP`         | Cross-service circuit breakers + RestTemplate retry + 503 degraded mode      |

All four types route through AWS SSM `send-command` so the chaos
process never needs SSH keys to the perf hosts. The bastion or
coordinator host runs the AWS CLI with an IAM role that has
`ssm:SendCommand` on the perf-cluster instances.

## kafka_kill

Stops one Kafka broker for the event's `duration`, then restarts it on
recovery.

**Required params**:

```yaml
- at: 1h
  duration: 30s
  type: kafka_kill
  params:
    instance_id: i-PERF-MSK-BROKER-1   # required — EC2 instance id
    cluster_name: aforo-perf-msk        # informational — embedded in SSM tags
    ssm_document_name: AWS-RunShellScript  # optional — defaults shown
```

**Inject**: `aws ssm send-command --document-name AWS-RunShellScript
--instance-ids <id> --parameters commands="sudo systemctl stop kafka"`.

**Recovery**: same shape with `start kafka`. Idempotent — re-running
"stop kafka" on an already-stopped broker is a no-op at the systemd
layer.

**What you should see during the event**:

- Producer-side: catalog-service / pricing-service / billing-platform
  Resilience4j producers retry per `KafkaErrorHandlerConfig` (3-retry
  exponential backoff). After the retry budget is exhausted the
  records land in the DLT.
- Consumer-side: `analytics-service` ClickHouse writer falls behind.
  Lag should drain within 60s of recovery.
- DLT growth: `dead_letter_records` table grows by a small but
  non-zero count. Validator should account for these as
  `expected_failures` not `events_lost`.

**What's a regression**:

- Producer log spam containing `Topic [...] not present in metadata
  after [...] ms` after recovery — should clear within 5s of broker
  restart.
- DLT records that fail to replay via `DeadLetterTopicMonitor`'s
  scheduled retry — these are the highest-severity chaos finding.

## redis_flush

Flushes the Redis ElastiCache cluster mid-run. Tests cold-cache
recovery from PostgreSQL and Kong's `RateLimitPolicyCacheWarmer`.

**Required params**:

```yaml
- at: 6h
  duration: 0s              # instantaneous — no recovery action needed
  type: redis_flush
  params:
    bastion_instance_id: i-PERF-BASTION
    cache_endpoint: aforo-perf-redis.cache.amazonaws.com:6379
```

**Refuses to fire** when `cache_endpoint` contains `prod` or
`production` — defense in depth alongside the scheduler's target
allow-list gate.

**Inject**: SSM-runs `redis-cli -h <endpoint> FLUSHALL` on the
bastion. The bastion has redis-cli installed and network access to
the cache; the coordinator does NOT connect to Redis directly because
production Redis sits in a private subnet.

**Recovery**: a no-op. There is no "unflush" — services rebuild from
PostgreSQL and Kong's policy table on next access.

**What you should see during the event**:

- `BillingHierarchyEnricher` cache miss spike for ~30 seconds; lookups
  fall through to `customer-service` until the cache refills via the
  EntitlementCacheSyncJob.
- Rate-limit policy cache repopulates from PostgreSQL via
  `RateLimitPolicyCacheWarmer`'s next scheduled run (default 30s).
- Kong rate-limit hot-path: until the cache warms, Kong's policy
  lookup fails open (logged at `warn` per the in-process hot-path
  fix landed earlier). No 429s should fire incorrectly during the
  cold window.

**What's a regression**:

- Storefront BFF returns 503s for >60s after the flush. The portal
  caches are tenant-scoped; rebuild should be O(1) per request.
- Any 4xx/5xx growth in usage-ingestor — entitlement enforcement
  should still work, just with a brief latency hit while the cache
  warms.

## ch_slowdown

Adds `latency_ms` of one-way network delay to ALL traffic into the
ClickHouse instance. Removes the delay on recovery.

**Required params**:

```yaml
- at: 24h
  duration: 5m
  type: ch_slowdown
  params:
    instance_id: i-PERF-CLICKHOUSE-1
    latency_ms: 500
    iface: eth0                  # optional — some instance types use ens5
```

**Inject**: `tc qdisc replace dev <iface> root netem delay <N>ms`.
The `replace` form is idempotent — re-running drops any prior qdisc
on the interface.

**Recovery**: `tc qdisc del dev <iface> root || true`. The `|| true`
swallows the "qdisc doesn't exist" error so Recovery is idempotent.

**What you should see during the event**:

- `ClickHouseWriter`'s in-memory batch buffer fills — visible via
  the `clickhouse_buffer_depth` Prometheus gauge.
- Resilience4j circuit breaker around the ClickHouse client trips
  after ~5 consecutive failures (the typical failure threshold).
- `AnalyticsQueryRouter` falls back to PostgreSQL for reads. Look
  for `analytics_query_fallback_to_pg` counter increments.
- After the breaker enters half-open (60s after trip), one canary
  request is allowed through. With the delay still in place, the
  canary fails and the breaker re-opens.
- Within 60s of recovery, the breaker closes and reads return to
  ClickHouse.

**What's a regression**:

- ClickHouse write loss — the run's `cogs_events` count post-recovery
  should match `events_succeeded` minus the buffer-overflow drops.
- Read-side data-quality drift — if the validator's billing-math
  invariant fails because PG fallback returns stale numbers, that
  indicates the dual-write replication isn't keeping up.

## net_partition

Drops all egress traffic from `source_instance_id` to `dest_ip`. Tests
cross-service circuit breakers and degraded-mode behavior.

**Required params**:

```yaml
- at: 72h
  duration: 2m
  type: net_partition
  params:
    source_instance_id: i-PERF-BILLING-PLATFORM
    dest_ip: 10.0.42.10
    source_service_name: billing-platform   # informational — used in iptables comment
    dest_service_name: customer-service      # informational
```

**Inject**: on the source host,
`iptables -I OUTPUT -d <dst-ip> -m comment --comment '<tag>' -j DROP`
where `<tag>` is generated from the source/dest service names so
multiple concurrent partitions don't step on each other's cleanup.

**Recovery**: matching `iptables -D` with the same args; falls back to
walking `iptables -L OUTPUT --line-numbers -n | grep '<tag>' |
awk '{print $1}' | tac | while read n; do iptables -D OUTPUT $n; done`
when the literal `-D` form fails. The fallback handles the case
where the rule got rewritten (rare but possible if other tooling
touches iptables on the same host during the chaos window).

**What you should see during the event**:

- `RestTemplate`s in billing-platform that target customer-service
  see DNS-resolves-but-connection-times-out errors. The
  `Resilience4j` instance trips after the configured failure
  threshold.
- Billing operations that NEED fresh customer data (e.g. invoice
  generation) fail closed with HTTP 503 + a retriable response.
  Operations that have a cached customer (most read paths) continue
  to work.
- Customer-service's own request rate from billing-platform drops to
  zero — visible in customer-service's incoming-request gauge.

**What's a regression**:

- Billing operations 500 instead of 503. A 500 indicates the breaker
  isn't catching the failure; the operation is leaking the underlying
  HTTP error.
- Stale-data behavior — billing-platform should NEVER serve a
  cached customer record older than the platform's configured TTL.
  If it does, that's a data-freshness bug.

## Authoring new chaos types

To add a new chaos type:

1. Create `internal/chaos/<type>.go` implementing `chaos.Scenario`:
   - `Type() string` → the YAML name
   - `Plan(ctx, exec) error` → up-front parameter validation + reachability check
   - `Inject(ctx, exec) (Recovery, error)` → fault injection + recovery closure

2. Register the type in `internal/chaos/factory.go`'s `BuildScenario`
   switch, mapping YAML `params` keys to your type's fields.

3. Add the type name to `SupportedChaosTypes` in
   `internal/scenario/validator.go` so the scenario validator
   accepts it.

4. Add per-type required-param checks in `validator.go`'s
   `checkChaosParams` map.

5. Write a unit test in `internal/chaos/chaos_test.go` that:
   - Verifies `Plan` rejects empty params.
   - Verifies `Inject` returns a `Recovery` closure (or an error).
   - Verifies `Recovery` calls the matching un-injection command.

Side-effect-free testing is provided by `chaos.NewRecorder()` — every
shell call routes through it without executing.

## Operator-facing summary

| Question                                     | Answer                                                 |
|----------------------------------------------|--------------------------------------------------------|
| Will chaos fire during a CI smoke run?       | No. Target name must be `perf-*`.                      |
| What if a chaos event fails to recover?      | Logged + counted. The run continues; ops triages.     |
| Can I dry-run a scenario's chaos events?     | Yes — `ShellExecutor{DryRun: true}` skips the shell.   |
| Where does the chaos timeline land?          | `<out>/run.json` under `chaos_timeline`.               |
| Can I add chaos to walk-tier scenarios?      | Yes if the target is perf-*; otherwise the scheduler refuses. |
