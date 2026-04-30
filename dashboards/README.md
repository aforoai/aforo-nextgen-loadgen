# Aforo Loadgen — Grafana Dashboard

The bundled `loadgen-run.json` dashboard renders live metrics from the
`aforo-loadgen run` Prometheus exporter. The CLI exposes `/metrics` on
`--metrics-addr` (default `:9095`); a Prometheus job scrapes that
endpoint, and Grafana reads from Prometheus.

## Panels

| Panel | Metric |
|-------|--------|
| TPS sent vs failed | `events_sent_total`, `events_failed_total` |
| Ingest latency p50/p95/p99 | `ingest_latency_seconds_bucket` |
| Per-archetype TPS | `events_sent_total{archetype}` |
| Per-product-type TPS | `events_sent_total{product_type}` |
| Error rate by class | `events_failed_total{error_class}` |
| Negative-path injections | `events_negative_path_total{negative_path}` |
| Active tenants | `tenants_active` |
| Backpressure | `backpressure_active` |
| Circuit breaker state | `circuit_breaker_state{driver}` |
| Per-ingestion-path TPS | `events_sent_total{ingestion_path}` (topk 10) |

The CLI does not currently tag metrics with a per-run label — the
panels reflect every active run that scrapes the same Prometheus
instance. Operators isolate a single run by setting Grafana's time
range to the run's start/end window. Control Tower's run detail page
generates a deep-link with `from=<started_at>&to=<ended_at>` so the
isolation is one click away.

## Import options

### Option A — Grafana provisioning (recommended)

Mount this directory at `/var/lib/grafana/dashboards/aforo-loadgen`
inside the Grafana container, and copy `grafana/loadgen-provider.yaml`
into the provisioning directory:

```yaml
# docker-compose.yaml (Grafana service)
services:
  grafana:
    image: grafana/grafana:11.0.0
    volumes:
      - ./aforo-nextgen-loadgen/dashboards:/var/lib/grafana/dashboards/aforo-loadgen:ro
      - ./aforo-nextgen-loadgen/dashboards/grafana/loadgen-provider.yaml:/etc/grafana/provisioning/dashboards/loadgen-provider.yaml:ro
    environment:
      GF_SERVER_ROOT_URL: https://grafana.aforo.space
```

Grafana re-scans the path every 30 seconds (configured in the
provider YAML) so dashboard edits land without restarting Grafana.

### Option B — Manual one-off import

`Dashboards → Import → Upload JSON file → loadgen-run.json`. Pick a
Prometheus datasource when prompted; the dashboard's `${datasource}`
variable resolves transparently.

## Variables

- `datasource` — Prometheus datasource picker (auto-detected).
- `runId` — free-text textbox; populated via the `?var-runId=...`
  URL parameter when the operator clicks "View live metrics in
  Grafana →" from Control Tower's run detail page. Used only for the
  title strip; panels are not yet filtered by run id.

## Adding a per-run filter (future)

If we add a `run_id` const label to the metrics registry (in
`internal/metrics/prometheus.go`, via
`prometheus.WrapRegistererWith`) every panel can filter to a single
run by appending `{run_id="$runId"}` to the queries. That's a
non-breaking additive change but out of scope for the operator-layer
deliverable in Session 12.
