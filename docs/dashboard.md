# Loadgen Operator Dashboard

The operator-facing surface for `aforo-loadgen` consists of two pieces:

1. **`aforo-loadgen server`** — a thin REST API (port 8095) that wraps the
   same scenario catalog and run engine the CLI uses, plus persistence
   for runs (Supabase + S3) and a Grafana deep-link.
2. **Control Tower `/admin/loadgen`** — three Next.js pages
   (runs list, trigger form, run detail) that operators use without
   touching the CLI.

The CLI remains the workhorse for 95% of usage — anything you can do
from the dashboard, you can also do from a terminal. The operator
layer exists for one-off triggers, casual browsing of historical
results, and giving non-engineers (e.g. CS / product) a way to start
a smoke-test on a feature branch without `ssh`.

## Running the server locally

```bash
make build

# Local dev — in-memory index, local-fs manifest store, no auth.
./bin/aforo-loadgen server \
  --listen :8095 \
  --work-dir /tmp/loadgen-server \
  --manifests-dir /tmp/loadgen-manifests \
  --allow-anonymous \
  --static-role platform_admin
```

The `--allow-anonymous` flag treats every request as a fixed
identity (configurable via `--static-user-id` / `--static-role`). It's
gated to local dev — production must use `--supabase-url` + the two
Supabase keys.

```bash
# Production — Supabase index, S3 manifest store.
aforo-loadgen server \
  --listen :8095 \
  --supabase-url https://xxx.supabase.co \
  --supabase-anon-key $SUPABASE_ANON_KEY \
  --supabase-service-role-key $SUPABASE_SERVICE_ROLE_KEY \
  --s3-bucket aforo-loadgen-runs \
  --s3-prefix prod/ \
  --grafana-base-url https://grafana.aforo.space
```

## Endpoints

All endpoints are JSON; auth is a Supabase JWT in the
`Authorization: Bearer ...` header. `/api/v1/health` is the only
unauthenticated route.

| Method | Path | RBAC |
|--------|------|------|
| `GET` | `/api/v1/health` | none |
| `GET` | `/api/v1/scenarios` | any internal role |
| `GET` | `/api/v1/runs` | any internal role |
| `POST` | `/api/v1/runs` | `platform_admin` only |
| `GET` | `/api/v1/runs/{id}` | any internal role |
| `GET` | `/api/v1/runs/{id}/manifest` | any internal role |
| `POST` | `/api/v1/runs/{id}/cancel` | `platform_admin` only |

The internal role is read from Supabase's `internal_roles` table —
the same table Control Tower uses for its own RBAC. A user with no
row in that table gets a 403 even if their Supabase JWT is valid.

## Trigger form (Control Tower)

`/admin/loadgen/new` renders a three-field form:

- **Scenario** — drop-down populated from `/api/v1/scenarios`. The
  bundled catalog (ci-smoke, walk-realistic-50t, run-15k-7day, etc.)
  is the only set of scenarios the server accepts. Ad-hoc YAML over
  HTTP is not supported by design (defense against arbitrary file
  reads).
- **Target** — `local`, `staging`, `ci`, or `prod`. Mirrors the CLI's
  `--target` flag.
- **Duration override** (optional) — formats like `30s`, `5m`, `1h`.
  Empty ⇒ scenario default.

Scenarios with `target_tps > 1000` show an inline confirmation
checkbox. Submission is blocked until the operator ticks it. This
mirrors the CLI's `--i-know-what-im-doing` flag.

## Run detail page

`/admin/loadgen/{runId}` shows:

- Status + outcome badges (running pulses; pass/fail color-coded).
- Stat strip — target, started, p99 latency, events sent/succeeded.
- **Assertions** — validate-oracle output, one row per check.
- **Per-archetype breakdown** — events_sent / succeeded / p99 per
  tenant archetype.
- **Per-negative-path breakdown** — late, future, malformed,
  wrong_auth, stale_key, oversize counts.
- **Cancel Run** button — only visible while the run is queued or
  running. Sends SIGINT to the worker; the run engine drains
  in-flight events and writes a partial manifest before exiting.
- **View live metrics in Grafana →** — deep-links to the dashboard
  with `var-runId` pre-filled (when the server has
  `--grafana-base-url` configured).
- **Download manifest** — pulls run.json from the manifest store
  (file:// or s3:// resolved transparently).

While the run is in-flight the page polls `/api/v1/runs/{id}` every
3 seconds. After completion polling stops automatically.

## Storage

| Layer | Backend | Notes |
|-------|---------|-------|
| Run index | Supabase `loadgen_runs` table | Migration 014 in `aforo-nextgen-control-tower-ui`. RLS default-deny — server reads/writes via service-role key. |
| Manifests | S3 bucket OR local FS | `--s3-bucket` ⇒ shells out to `aws s3 cp`. Empty ⇒ local-fs at `--manifests-dir`. |
| Run artifacts | Server `--work-dir` | One subdir per run; cleaned up by an out-of-band cron. |

S3 manifest objects are pruned after 90 days by an S3 lifecycle rule.
Index rows are preserved indefinitely — assertion summaries are tiny
and useful for postmortems even after the manifest is gone.

## What's intentionally missing

Phase 3 deliverables explicitly excluded from Session 12:

- **Side-by-side compare** of two runs.
- **Regression alerts** — Slack / PagerDuty when assertions fail.
- **Scheduled runs UI** — cron is sufficient for v1.
- **Per-run Prometheus label** — the dashboard shows every active
  run that scrapes the same Prometheus instance. Operators isolate by
  setting Grafana's time range to the run's start/end window.

See `dashboards/README.md` for the Grafana side and
`docs/grafana-setup.md` for the integration playbook.
