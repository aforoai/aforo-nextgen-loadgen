# Grafana Setup

Step-by-step playbook for wiring Grafana to consume `aforo-loadgen`
metrics. The dashboard lives at `dashboards/loadgen-run.json` — see
`dashboards/README.md` for the panel-by-panel reference.

## 0. Prerequisites

- Grafana 10.x or 11.x.
- A Prometheus instance reachable from Grafana.
- A way for Prometheus to scrape `aforo-loadgen run`'s `/metrics`
  endpoint (default `:9095`).

## 1. Prometheus scrape config

`aforo-loadgen run` exposes `/metrics` on `--metrics-addr` (default
`:9095`). Add a scrape job:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'aforo-loadgen'
    static_configs:
      - targets: ['loadgen-host:9095']
        labels:
          env: 'staging'   # or prod, ci, etc.
```

For multi-host distributed runs (`aforo-loadgen coordinator`), each
worker exposes its own `/metrics` on its assigned port — list them
all under `targets`. The dashboard panels sum across instances.

## 2. Grafana datasource

```bash
# datasources.yaml — drop into /etc/grafana/provisioning/datasources/
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    uid: PBFA97CFB590B2093
    url: http://prometheus:9090
    isDefault: true
```

The dashboard's `${datasource}` template variable auto-binds to
whichever Prometheus datasource is configured.

## 3. Dashboard import

Two options:

### A — File-based provisioning (recommended)

```bash
# 1. Mount the repo's dashboards/ directory into the Grafana
#    container at /var/lib/grafana/dashboards/aforo-loadgen.
# 2. Drop dashboards/grafana/loadgen-provider.yaml into the
#    provisioning directory.
```

Grafana re-scans the directory every 30 seconds, so editing the
dashboard JSON in git and pushing is sufficient — no manual UI
re-import.

### B — One-off manual import

`Dashboards → New → Import → Upload JSON file → loadgen-run.json`.
Pick the Prometheus datasource when prompted.

## 4. Deep-linking from Control Tower

When the loadgen server is configured with `--grafana-base-url`,
every run row in Supabase carries a `grafana_url` field of the form:

```
https://grafana.aforo.ai/d/loadgen-run/loadgen-run?var-runId=<run-id>
```

Control Tower's run detail page renders this as a "View live
metrics in Grafana →" button. The `runId` template variable is a
free-text textbox — its value is the run id from the URL, used in
the title strip but not yet used to filter individual panels (the
CLI does not currently emit a `run_id` Prometheus label; see
"Per-run isolation" below).

## 5. Per-run isolation

Two options for narrowing the dashboard to a single run:

1. **Time range** — when an operator clicks the deep-link, append
   `from=<started_at>&to=<ended_at>` to the URL (the loadgen server
   doesn't do this today; can be added in a follow-up). All panels
   then reflect only the window that run was active.
2. **`run_id` const label** — wrap the metrics registry with
   `prometheus.WrapRegistererWith({"run_id": runID}, reg)` in the run
   engine. Every metric carries the label; the dashboard's panels
   gain `{run_id="$runId"}` filters. Non-breaking additive change;
   not done in Session 12 because the operator-time-range workflow
   is sufficient for the cases the operator UI is designed for.

## 6. Verification

After import, the dashboard should show empty graphs (no runs in
flight). Trigger a smoke test:

```bash
aforo-loadgen run \
  --scenario ci-smoke \
  --target local \
  --manifest manifest.json \
  --metrics-addr :9095
```

Within ~5 seconds the **TPS sent vs failed** panel should show a
line; **Active tenants** should match the manifest count; **Per-
archetype TPS** should show one line per archetype.

If panels stay flat:

- Check Prometheus `/targets` — the loadgen job should be `UP`.
- Check `aforo-loadgen run`'s logs for `metrics: bind ... in use`
  errors.
- `curl http://loadgen-host:9095/metrics | head -20` — should print
  Prometheus exposition format.
