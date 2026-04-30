# Getting Started — `aforo-loadgen` in 5 Minutes

This is the quickstart. Follow it on a fresh checkout against a clean local
docker stack and you should have a green end-to-end report in under 10
minutes total wall-clock.

## What you need

- macOS or Linux. Windows is not supported in Session 7.
- Docker Desktop (or Colima / Rancher Desktop) running with at least 6 GB
  of memory.
- Go 1.22 or newer (only if you're building from source).
- An admin bearer token. On a fresh local stack, the docker-compose
  bootstrap mints one and prints it; copy that into `AFORO_ADMIN_TOKEN`.

## 1. Install

### From Homebrew (recommended)

```bash
brew install aforo/tap/loadgen
```

### From source

```bash
git clone https://github.com/aforoai/aforo-nextgen-loadgen.git
cd aforo-nextgen-loadgen
make build
# binary lands at ./bin/aforo-loadgen
```

## 2. Bring the platform up

```bash
cd ../aforo-nextgen-docker
docker-compose up -d
```

The first start downloads images and runs Flyway migrations on every
service. Cold start typically takes 60–120 seconds; wait for `docker
compose ps` to show every service as `(healthy)`.

## 3. Export your bearer token

The platform's bootstrap script prints an admin JWT to stdout on the
very first start. Copy it into your shell:

```bash
export AFORO_ADMIN_TOKEN=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...
```

If you've lost that initial output, log into Control Tower (port 3001)
and copy a fresh token from `Settings → API Keys → Generate Admin
Token`. Tokens are valid for 24 hours.

## 4. Run doctor

```bash
aforo-loadgen doctor --target local
```

This probes every microservice's `/actuator/health`, verifies your
token, and reports infra component status. Expect output like:

```
doctor — target: local
STATUS  SEVERITY  CHECK                       DETAIL
OK      CRITICAL  service:organization        UP
OK      CRITICAL  service:catalog             UP
OK      CRITICAL  service:pricing             UP
OK      CRITICAL  service:customer            UP
OK      CRITICAL  service:usage-ingestor      UP
OK      CRITICAL  service:analytics           UP
OK      CRITICAL  service:billing             UP
OK      CRITICAL  service:storefront          UP
OK      WARNING   service:ai-service          reachable (no actuator)
OK      CRITICAL  auth:bearer-token           HTTP 200
OK      WARNING   auth:tenant-bootstrap       3 tenant(s) reachable
OK      CRITICAL  infra:db                    UP across 8 service(s)
OK      CRITICAL  infra:kafka                 UP across 6 service(s)
OK      CRITICAL  infra:redis                 UP across 7 service(s)
OK      CRITICAL  infra:clickhouse            UP across 1 service(s)

summary: OK — pass=15 fail=0 skip=0  (1240ms)
```

If anything fails, doctor prints a remedy line that names the next
command to run. Most common: a service is still warming up — wait 30
seconds and re-run doctor.

## 5. Run the end-to-end flow

```bash
aforo-loadgen e2e \
    --scenario scenarios/crawl-e2e.yaml \
    --target local \
    --include-billing \
    --include-lifecycle
```

This chains every loadgen subsystem in one go:

1. **doctor** — same probes as step 4 (skip via `--skip-doctor`).
2. **seed** — provisions 4 tenants × 100 customers each. ~30 seconds.
3. **run** — drives 50 events/sec for 5 minutes against the seeded
   population. ~5 minutes.
4. **lifecycle** — fires subscription transitions in parallel
   (~1/minute). Drains when run completes.
5. **validate** — runs all 11 invariant checks against run.json,
   manifest.json, and (with `--include-billing`) live billing data.
6. **report** — renders a self-contained `report.html` you can forward
   to Slack or attach to a PR comment.
7. **clean** — archives all seeded entities (skipped via `--keep-data`).

Expected output:

```
e2e summary
scenario: crawl-e2e   target: local   out: e2e/crawl-e2e-1714500000
────────────────────────────────────────────────────────────────────────────
  doctor       PASS     1.2s        checks: 15 pass / 0 fail / 0 skip
  seed         PASS     31s         tenants=4 customers=400 subs=400 errors=0
  run          PASS     5m0s        events=15012 ok=14111 4xx=901 ... p99=240ms
  lifecycle    PASS     5m0s        transitions=58
  validate     PASS     12s         checks: 11 pass / 0 fail / 0 skip / 11 total
  report       PASS     0.3s        report.html: e2e/crawl-e2e-1714500000/report.html
  clean        PASS     8s
────────────────────────────────────────────────────────────────────────────
  overall=PASS   total=5m52s
```

## 6. Open the report

```bash
open e2e/crawl-e2e-*/report.html
```

The report is fully self-contained — no CDN, no external fonts. Forward
the HTML file directly; it renders identically anywhere, including
offline.

## What's next

- `scenarios/walk-realistic-50t.yaml` — 50 tenants, ~10 minutes.
- `scenarios/matrix-billing.yaml` — exercises all 6 pricing models.
- `scenarios/lifecycle-stress.yaml` — high-rate transition stress.
- `scenarios/run-15k-7day.yaml` — production target. Run only on a real
  staging cluster, never on a laptop.

To customize, copy a scenario YAML, edit, and pass `--scenario
path/to/your.yaml` to any subcommand. See
[`docs/scenario-schema.md`](scenario-schema.md) for the full grammar.

## Make targets

```bash
make doctor-local    # equivalent to step 4
make e2e-local       # equivalent to step 5
make doctor-staging  # requires AFORO_STAGING_TOKEN
make e2e-staging     # requires AFORO_STAGING_TOKEN
```

## When something goes wrong

See [`docs/troubleshooting.md`](troubleshooting.md). The pattern is
always: doctor first, then dig into individual stages with `aforo-loadgen
seed --dry-run`, `aforo-loadgen run --duration 30s`, etc.
