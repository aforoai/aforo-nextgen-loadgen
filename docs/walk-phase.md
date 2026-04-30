# Walk Phase — Realistic 50-Tenant, 24-Hour Sustained Load

The walk phase is the second tier in the Crawl → Walk → Run progression. Where
the crawl tier (`ci-smoke`, `crawl-e2e`) verifies basic correctness at single
digits of TPS, the walk tier exercises the platform at the scale and duration
where leaks, GC pressure, scheduler overlap, and noisy-neighbor failures show
up.

This document explains what the walk-tier scenario `walk-realistic-50t.yaml`
covers, how to run it safely, and how to interpret the per-tenant fairness
report that's the load test's marquee deliverable.

## Targets

- **50 tenants** drawn from 8 archetypes covering all 6 pricing models, all 3
  billing modes, and all 4 GA product types.
- **2K TPS sustained** for **24 hours** (staging only — see safety rails).
- **Pareto 80/20** tenant traffic distribution: top 10 tenants get ~60% of
  events.
- **All 16 ingestion paths exercised**: REST direct, four SDKs (Node, Python,
  Java, Go), nine gateway adapters (Kong, Apigee, AWS, Azure, MuleSoft, APISIX,
  Tyk, Gravitee, Envoy), webhook receiver, and CSV bulk upload.
- **Three-peak diurnal shape** (sine_24h, regional phasing) — peaks at 06:00,
  14:00, 22:00 UTC, approximating Asia / EU / US business cycles.
- **Light fault injection** — 0.1% each of late, future, malformed, wrong-auth,
  and stale-key events.
- **Per-tenant SLO isolation** — `per_tenant_p99_fairness_max_stddev_pct: 0.20`
  blocks runs where one tenant's p99 deviates more than 20% from the population
  mean.

## Running it

### Prerequisites

1. Seeded manifest (≥ 50 tenants):
   ```
   aforo-loadgen seed --scenario walk-realistic-50t --target staging \
                      --provision-webhooks --out runs/walk/manifest.json
   ```
   The `--provision-webhooks` flag creates one webhook ingest source per tenant
   via `POST /api/v1/webhook-sources` and writes `webhook_sources.json` next
   to the manifest. Without it, webhook traffic exercises the synthetic 404
   fallback (still useful for shape-only load tests but the receiver rejects
   every request).

2. Bearer token in env: `export AFORO_ADMIN_TOKEN=...`

### 30-minute smoke run

```
aforo-loadgen run --scenario walk-realistic-50t \
                  --target staging \
                  --manifest runs/walk/manifest.json \
                  --duration 30m \
                  --fairness-min-share 0.5 \
                  --out runs/walk-30m
```

This is the acceptance gate: 30 minutes is enough to engage all 16 drivers,
populate the per-tenant histograms with statistically-meaningful samples, and
exercise the time-of-day shape's first regional peak.

### Full 24-hour run

```
aforo-loadgen run --scenario walk-realistic-50t \
                  --target staging \
                  --manifest runs/walk/manifest.json \
                  --partitions 4 \
                  --fairness-min-share 0.5 \
                  --metrics-addr :9095 \
                  --out runs/walk-24h
```

`--partitions 4` enables distributed mode — four tenant-partitioned runners in
parallel, each handling ~12-13 tenants and 500 TPS, results merged into a
single `runs/walk-24h/run.json`.

### Local laptop guard

`--target=local` runs longer than 1 hour fail with:

```
refusing to run walk-realistic-50t against --target=local for 24h0m0s (>1h0m0s).
  Walk-tier scenarios are sized for staging; running locally for this duration
  will saturate the laptop and risk corrupting the docker-compose backend.
  Re-run with --i-know-what-im-doing to override, or shorten with --duration
```

If you really mean it, pass `--i-know-what-im-doing`. The default behavior
prevents the most common foot-gun: starting a 24h run against the local
docker-compose stack overnight and finding a melted laptop and a corrupted
PostgreSQL data volume in the morning.

## Anatomy of a walk-tier run

| Stage | What happens | Where to look |
|-------|--------------|---------------|
| **Pacer warmup** | The sine pacer integrates the rate function and computes the first 100 deadlines | `events_generated` ramps from 0 to ~target_tps over ~30s |
| **Per-driver warmup** | Each ingestion path's HTTP client opens connections to its endpoint; idle pool reaches steady state | After ~5 minutes the driver fanout stabilizes |
| **Cache warmup** | Server-side Redis cache hits the 90% threshold for steady-state requests | `cache_hit_ratio` in run.json reports the post-warmup steady state |
| **Three peak pulses** | At 06:00, 14:00, 22:00 UTC (relative to run start), the rate climbs to (1 + amp) × TargetTPS — for 24h runs this exercises auto-scaling | Visible in the rate graph at /metrics |
| **Fault injection** | 0.5% combined fault rate produces 4xx/5xx responses by design | `negative_path_counts` in run.json |
| **Fairness measurement** | Per-tenant HDR histograms accumulate; the report is computed at end-of-run | `fairness` block in run.json + per-tenant table in the HTML report |

## Per-tenant fairness report

The walk-phase deliverable is the per-tenant fairness report — a measurement
of whether one tenant's traffic affects another's tail latency.

```json
"fairness": {
  "tenants_observed": 50,
  "mean_p99_ms": 142.3,
  "stddev_p99_ms": 18.7,
  "stddev_pct": 0.131,
  "min_p99_ms": 108.2,
  "max_p99_ms": 213.5,
  "worst_offenders": [
    { "tenant_id": "t_arch07_03", "p99_ms": 213.5, "delta_frac": 0.50 },
    { "tenant_id": "t_arch07_05", "p99_ms": 198.2, "delta_frac": 0.39 }
  ]
}
```

- `stddev_pct < 0.30` is the spec's pass threshold. The walk scenario tightens
  to 0.20 — any tenant whose p99 is more than 20% off the population mean
  triggers the assertion.
- `worst_offenders` is sorted by absolute deviation from the mean. The top
  five are reported even when the run passes — they're the candidates for
  capacity tuning.

## Memory footprint

Per-tenant histograms occupy roughly 120 MiB at the walk-tier breakdown (50
tenants × 4 ingestion paths × 4 product types × 150 KiB per HDR). The spec's
ceiling is 400 MB; actual cost is well below that. The runner reports
`per_tenant_histograms_mb` in run.json so growth above 200 MiB on a 24h run
flags a regression.

## Distributed mode caveats

`--partitions N` runs N tenant-partitioned runners in the same process. Each
partition writes its own per-partition latencies.hdr; the merged run.json at
the parent OutputDir takes the WORST observed p99 across partitions as a
conservative upper bound. For ground-truth p99 percentiles, read the per-
partition HDR files directly.

The CLI's `--partitions` flag is intentionally distinct from `--workers`
(which is the per-pool worker count). A future session can swap the in-
process partitioning for cross-process gRPC coordination without changing the
flag semantics.

## What this scenario doesn't catch

- **Cold-start cost** — partitions warm up independently. Set the run length
  to ≥ 5 minutes so cache + connection pools reach steady state before the
  fairness window matters.
- **Cross-region behavior** — single-region run; the regional sine peaks are
  synthetic. A real cross-region test needs multiple loadgen processes, one
  per region.
- **Cross-service tail latency** — the loadgen measures end-to-end ingest
  latency. Per-service breakdowns (catalog vs pricing vs billing) need to
  come from server-side traces.
- **Disk-pressure scenarios** — the walk scenario doesn't exercise disk-
  pressure paths (full ClickHouse write queue, full Kafka partition). The
  run-15k-7day scenario covers those.

See [scenario-schema.md](scenario-schema.md) for the YAML reference and
[ingestion-paths.md](ingestion-paths.md) for the per-driver wire shapes.
