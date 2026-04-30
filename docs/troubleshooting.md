# Troubleshooting `aforo-loadgen`

When the tool fails, the failure usually falls into one of the buckets
below. Each entry pairs the symptom with a one-line diagnosis and the
exact remedy.

## doctor

### "X not reachable at localhost:NNNN"

The named service isn't running. Check the docker-compose stack:

```bash
cd aforo-nextgen-docker
docker-compose ps
docker-compose logs <service-name> | tail -100
```

A common culprit on first start is Flyway migration hangs — the service
container shows `(starting)` for >2 minutes. Logs will name the
specific migration that's stuck. Fix typically: bring the stack down,
remove the volume for the affected DB, bring it back up.

### "auth:bearer-token FAIL — HTTP 401"

Your token is bad or expired. Check:

```bash
echo $AFORO_ADMIN_TOKEN | cut -c1-20
```

Tokens start with `eyJ`. If the variable is empty, the platform's
bootstrap output was lost — log into Control Tower (port 3001) and
generate a fresh one. Tokens are 24-hour scoped.

### "infra:db DOWN reported by: catalog, pricing"

PostgreSQL is up but at least one service can't reach it. Almost
always: the service started before postgres became healthy and is
stuck on a stale connection pool. Restart the affected services:

```bash
docker-compose restart catalog pricing
```

### "service:ai-service SKIP" or "WARNING"

Expected. ai-service has no actuator surface today and is only required
for admin AI features (Brand Extract, Section Generator). The crawl-e2e
scenario doesn't exercise it, so the SKIP / WARNING is non-blocking.

## seed

### "seed: completed with N error(s)"

A subset of tenant provisioning calls failed. The full manifest is
still preserved at `<out>/manifest.json` — re-run with `--clean` to tear
down what landed:

```bash
aforo-loadgen seed --clean --out manifest.json --target local
```

Common causes:

- **organization-service rate-limited the bulk tenant creation.** Add
  `--concurrency 2` to seed; the default of 4 is too aggressive on a
  cold local stack.
- **catalog-service Flyway hadn't finished when seed started.** Doctor
  caught this if you ran it. Wait 30 seconds and retry.
- **Duplicate tenant name from a prior incomplete clean.** `seed
  --clean` is idempotent; running it on a half-cleaned manifest is safe.

### "seed: bearer token is required"

You forgot `export AFORO_ADMIN_TOKEN=...`. Doctor would have caught
this. Run doctor first.

## run

### "run: 5xx rate above threshold"

Either a service crashed mid-run, or you've exceeded the platform's
local capacity. Inspect in this order:

1. `docker-compose logs --tail=200 usage-ingestor` — most 5xx noise
   originates here.
2. `kubectl top pods` (staging only) — usage-ingestor OOMs first.
3. The run's `events.jsonl.zst` file — every 5xx response body is
   logged there. Look for repeating error strings.

### "run: events_lost > 0"

Events sent to usage-ingestor never landed in ClickHouse. This is a
real correctness regression — open a ticket against analytics-service
with the run id. The validator's Check 1 (event_count_per_tenant) is
the canonical detector for this.

### "context deadline exceeded"

The run's HTTP client gave up. Defaults are 5s per request and 30s
total. On a cold local stack, the very first batch of requests can
take 10+ seconds while JVMs warm up. Retry once after a 60-second
cooldown.

## lifecycle

### "lifecycle: 0 transitions fired"

The picker found no eligible subjects. Check:

```bash
jq '.summary' manifest.json
# subscription_state_mix should include ACTIVE > 0
```

If every sub is in CANCELLED state (e.g. you manually flipped them via
the platform admin UI), the lifecycle agent has nothing to act on.
Re-seed.

### "lifecycle: ticker MAX_INTERVAL — eligible=0"

A specific transition kind has no eligible subjects. Most common with
`trial_conversion`: the scenario's subscription_state_mix has zero
TRIALING subs. The crawl-e2e scenario does not include TRIALING by
design, and the trial-conversion ticker SKIPs silently. This is not
an error.

## validate

### "Check 5 — per_archetype_billing_match: SKIP"

You forgot `--include-billing`. The check is opt-in because it's
slow (queries billing-service for every archetype). Re-run validate
with the flag.

### "Check 6 — negative_path_correctness: FAIL — stale_keys"

The platform accepted an event from a CANCELLED or EXPIRED
subscription. This is a tenant-isolation bug — open a ticket against
usage-ingestor. The validator's report at
`<out>/validation.json` includes the offending key id.

### "Check 8 — bill_run_concurrency: SKIP"

Same as Check 5: opt-in via `--include-billing`. Without it, no
duplicate bill-run trigger is fired so concurrency cannot be tested.

## e2e

### "e2e: stage `seed` FAIL — clean ran"

Seed failed mid-flight. The orchestrator still ran the clean stage
to roll back anything it had managed to provision. Inspect `seed`'s
detail in `<out>/e2e.json` for the per-tenant failure list.

### "e2e: stage `validate` FAIL — report ran"

A check failed. The orchestrator deliberately renders the report HTML
even on validate failure because the rendered checklist is what you
use to triage. Open the HTML.

### "e2e completed but I want to look at the data"

Re-run with `--keep-data`:

```bash
aforo-loadgen e2e \
    --scenario crawl-e2e \
    --target local \
    --keep-data \
    --out e2e-debug
```

The clean stage SKIPs and `e2e-debug/manifest.json` keeps every
seeded entity id for direct platform UI inspection.

### "e2e took longer than 10 minutes on a healthy local stack"

That's a regression target — Session 7 promised <10 min on a healthy
stack. Capture the per-stage timings from `<out>/e2e.json` and open a
ticket. Most likely culprit: usage-ingestor 5xx storm slowing the run
stage. Inspect with the run troubleshooting tips above.

## When all else fails

Bring the stack down clean, prune everything, bring it back up:

```bash
cd aforo-nextgen-docker
docker-compose down -v       # destroys all data — fresh start
docker-compose up -d
# wait 60 seconds for healthchecks to flip green
make doctor-local
make e2e-local
```

If e2e still fails on a clean stack with a green doctor, file a bug
against `aforo-nextgen-loadgen` with:

- The scenario YAML
- `<out>/e2e.json`
- `<out>/run.json`
- `<out>/validation.json`
- `docker-compose logs --tail=500 > docker-logs.txt`
