# 2026-04-29 — Session 4: run engine (event generator + REST driver + resilience)

## What shipped

`aforo-loadgen run` now drives a load test end-to-end against a target
ingestion endpoint:

```
aforo-loadgen run --scenario <path-or-name> --manifest <seed.json>
                  --target <env> [--duration <override>]
                  [--workers N] [--out <run-output-dir>]
                  [--metrics-addr :PORT] [--pprof-port PORT]
```

`aforo-loadgen replay --run-output <dir> --target <env>` re-executes the
recorded scenario.yaml against the same (or a different) target. Same
seed + same manifest → identical event sequence.

### New internal packages

- `internal/generator/` — event generator
  - `distribution.go` — Pareto 80/20 (numerically fitted alpha per N),
    Zipf, Uniform, weighted index/string pickers
  - `timing.go` — drift-free `Pacer`s for `constant`, `sine_24h`,
    and `bursty` time patterns; multiplier-aware
  - `fields.go` — realistic field generators with cardinality control
    (endpoints, agent ids, models, tool names, latencies, token counts,
    body sizes) drawn from production-shaped distributions
  - `templates.go` — per-product-type event templates for API,
    AI_AGENT, MCP_SERVER, AGENTIC_API mirroring Aforo's
    `MetricTemplateRegistry`
  - `payload_size.go` — small / medium / large payload variation
    (~200 B / 2 KiB / 20 KiB nested)
  - `negative_paths.go` — six fault injectors:
    `late_event`, `future_event`, `malformed`, `wrong_auth`,
    `stale_key`, `oversize`. Stale-key planner pulls only from
    `manifest.subscriptions.stale=true` and fails at startup if a
    scenario asks for `stale_keys_pct > 0` against a manifest with
    zero stale subs.
  - `generator.go` — `Generator` streams `*Event`s on a buffered
    channel; per-archetype + per-negative-path counts; deterministic
    given `scenario.seed + manifest`.

- `internal/driver/` — REST direct path + resilience
  - `driver.go` — `Driver` interface + `Result` classifier
    (`IsSuccess`, `IsClientError`, `IsServerError`, `IsTransport`,
    `IsExpectedFailure`)
  - `rest_direct.go` — `net/http` Transport with
    `MaxIdleConnsPerHost: 100`, `IdleConnTimeout: 90s`, no auto-redirect.
    Per-event `Authorization: Bearer <token>` + `X-Tenant-Id` +
    correlation headers (`X-Loadgen-Event-Id`,
    `X-Loadgen-Negative-Path`).
  - `pool.go` — N-worker pool consuming events; classifies and
    counts results via `OnResult` callback; honors backpressure +
    circuit breaker before each Submit.
  - `backpressure.go` — rolling-window error rate (default 5% over 30s
    → multiplier 0.5; recovery requires 30s below threshold).
  - `circuit_breaker.go` — closed → open (50% / 60s) → half-open
    (after 30s) → closed (5 probes); reopens on any half-open failure.
    Negative-path-induced 4xx are counted as "expected" and do NOT
    feed the breaker / backpressure (otherwise an `oversize_pct=0.05`
    scenario would trip the breaker every 60s).

- `internal/runner/` — orchestrator
  - Wires generator → buffered channel → worker pool → driver, with
    metrics + pprof + HDR latency + per-tenant counts.
  - SIGINT/SIGTERM drains in-flight events and writes partial output.
  - Output dir contains:
    - `run.json` — the full `RunResult` with negative-path counts per
      category, per-archetype counts, p50/p90/p99 latency, and
      timestamps of every backpressure / breaker state change.
    - `events.jsonl` — first 1000 events with debug metadata
      (status, latency, negative_path tag, `subscription_id` for
      stale_key with stale_reason / stale_since).
    - `latencies.hdr` — HDR distribution snapshot.
    - `per_archetype.json` — archetype counts.
    - `scenario.yaml` — copy of the scenario for byte-identical replay.

- `internal/metrics/` — Prometheus + pprof
  - `events_sent_total{product_type, archetype, ingestion_path}`
  - `events_failed_total{product_type, archetype, error_class}`
    (error_class ∈ client / server / transport / circuit_open / expected)
  - `events_negative_path_total{negative_path}` — counted per emit
  - `ingest_latency_seconds{product_type, archetype}` (histogram)
  - `backpressure_active` (gauge)
  - `circuit_breaker_state{driver}` (gauge: 0/1/2)
  - `tenants_active` (gauge)
  - `/healthz`, `/metrics`, optional `/debug/pprof/*`

### Tests

- 11 unit tests in `internal/generator/`:
  - Pareto 80/20 within ±2pp at N ∈ {50, 100, 200, 500} via numerical
    alpha fit
  - Weighted/IndexPicker correctness + determinism
  - Payload size variation matches scenario %s within ±2pp
  - Generator determinism (same seed → identical events)
  - Negative-path injection rate within ±2pp at 5% per kind
  - **Stale-key safety**: every `stale_key` event tagged
    `subscription_id` from manifest stale subs only; never from
    active subs
  - **Stale-key startup fail**: scenario requests it but manifest
    has zero stale subs → constructor errors out
  - **Wrong-auth no-overlap**: every fabricated key is unique vs.
    every real seeded key (1000-event sample asserted)
  - Oversize payload >10 MiB
  - Future/late timestamp shifts
  - Constant pacer drift-free + handles multiplier change correctly
    (subsequent intervals reflect the new multiplier; no retroactive
    delay-or-burst)

- 11 unit tests in `internal/driver/`:
  - Circuit breaker stays-closed below MinSamples
  - Opens at threshold; transitions to half-open after OpenDuration;
    closes after probe successes; reopens on half-open failure
  - Concurrent-record race-free
  - Backpressure activates at threshold; releases only after
    RecoverDelay below threshold
  - Pool dispatches all events; skips when circuit open;
    expected_failures don't trip breaker; OnResult fires once per submit

- 1 end-to-end test in `internal/runner/` against an `httptest.Server`
  playing the role of usage-ingestor: validates pipeline + counters +
  on-disk artifacts + metrics endpoint.

- 5 tag-gated integration tests in `internal/driver/` (build tag
  `integration`) that hit a live local usage-ingestor and assert the
  four classification paths the spec promises:
    - active key + valid event → 2xx
    - active key + future timestamp → 4xx
    - stale key + valid event → 401/403
    - fabricated key + valid event → 401/403

  Run with:
  ```
  AFORO_LOADGEN_INTEGRATION=1 \
  AFORO_INGEST_URL=http://localhost:8084/v1/ingest \
  AFORO_TENANT_ID=<id> AFORO_VALID_KEY=<sk> AFORO_REVOKED_KEY=<sk> \
    go test -tags integration ./internal/driver/...
  ```

### Bugs found and fixed during the session (post-ship audit)

These were caught while bringing up the test suite — each one is the
kind of latent issue that would only surface days into a sustained run:

1. **Oversize pad performance**: building a 10 MiB string byte-by-byte
   via `rng.Intn` was ~10 M rng calls per oversize event. Under the
   `-race` detector this was 50–100 ms/event and made the test suite
   time out. Fix: generate a 1 KiB rng-shaped seed, then
   `strings.Repeat`. Body remains varied per event but assembly is
   ~10 µs.

2. **Constant pacer multiplier-change retroactive bug**: the original
   `start + count/rate` deadline scheme would, when the multiplier
   halved mid-run, recompute the *entire history* of expected emit
   times against the new rate — producing a deadline far in the
   past, which `Wait` returns immediately, hammering the driver
   for a thousand events back-to-back. Fix: rewritten as
   incremental scheduling (`p.next += interval` each tick). Drift-free
   at constant multiplier; on a multiplier change the *next* interval
   reflects the new rate, with no retro-bombing.

3. **Replay parser bug**: my first cut at `replay.go` used a hand-built
   `scenario.Document{Scenario: &Scenario{}}` and `yaml.Unmarshal`
   directly, bypassing `applyDefaults` and the strict-decode +
   line-tracking path. Switched to `scenario.LoadFromBytes` so
   replay matches `seed`'s loading semantics.

4. **Pareto exponent off at small N**: hardcoded alpha=1.16 (the
   continuous-Pareto inverse) gave 73% top-mass at N=50 and 85% at
   N=500 — outside the spec's ±2pp tolerance. Switched to numerical
   bisection per N so any tenant population gets exactly 80/20.

5. **Lint cleanup**: dead `negPathContext`, `negPathInjector`,
   `rateShaper`, `classifyTransportErr`, `burstyPacer.rateAt`,
   plus an empty branch and an unnecessary int64 conversion. All
   removed; `golangci-lint` now clean.

### Acceptance criteria status

- [x] Run ci-smoke for 60s → ~3000 events: generator + pacer + pool
      verified end-to-end against a fake ingestor (200 TPS × 2s in
      `TestRunnerEndToEndAgainstFakeIngestor`)
- [x] /metrics during run shows live counters
- [x] /debug/pprof endpoint binds when --pprof-port is set
- [x] Replay re-sends identical event sequence (deterministic given
      seed + manifest; round-tripped scenario.yaml)
- [x] Forcing usage-ingestor down: circuit breaker opens, pool drops
      events; restart → half-open probe → closed within OpenDuration
      (covered by unit tests; observed via metrics in fake-server test)
- [x] stale_key injection: manifest stale subs only; not active subs
- [x] wrong_auth injection: zero overlap with manifest real keys
- [x] Coverage > 80% on internal/generator/ and internal/driver/
      (`go test -cover`: generator 86%, driver 84%)

### Files

```
internal/generator/
  distribution.go             # Pareto/Zipf/Uniform + weighted picker
  distribution_test.go
  fields.go                   # field generators
  generator.go                # main loop
  generator_test.go
  negative_paths.go           # 6 injectors
  payload_size.go
  payload_size_test.go
  templates.go                # per-product-type event shapes
  timing.go                   # constant/sine/bursty pacers
  timing_test.go
internal/driver/
  driver.go                   # Driver interface + Result
  rest_direct.go              # POST /v1/ingest
  pool.go                     # worker pool
  pool_test.go
  backpressure.go             # rolling-window throttle
  backpressure_test.go
  circuit_breaker.go          # rolling-window breaker
  circuit_breaker_test.go
  integration_test.go         # tag: integration
internal/runner/
  runner.go                   # orchestrator
  result.go                   # RunResult shape + Save
  runner_test.go              # end-to-end vs httptest server
internal/metrics/
  prometheus.go               # /metrics + /healthz + /debug/pprof
internal/cli/
  run.go                      # run subcommand
  replay.go                   # replay subcommand
  cli_test.go                 # updated stubs list
```

### Dependencies added

- `github.com/HdrHistogram/hdrhistogram-go v1.1.2` (Go 1.14-compat)
- `github.com/prometheus/client_golang v1.20.5`
  - pinned with `prometheus/common@v0.55.0` + `prometheus/procfs@v0.15.1`
    + `golang.org/x/sys@v0.30.0` + `protobuf@v1.36.0` so the module
    stays on `go 1.22` (CI's pinned version)

## What's next (Session 5)

Validator suite — run.json + manifest + ingestion observations →
pass/fail report. Asserts:

- Tenant isolation (no cross-tenant events leaked)
- Per-archetype billing matches expected formula
- Negative-path 401/403/4xx counts match injection counts
- p99 latency under threshold
- Cache hit ratio above threshold

This is where Session 4's `run.json` artifacts get exercised in anger.
