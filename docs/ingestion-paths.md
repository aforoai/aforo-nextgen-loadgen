# Ingestion Paths — Driver Reference

Aforo's usage-ingestor exposes one canonical event endpoint, but customer
traffic reaches it through 16 distinct paths. Each path has different
authentication, headers, request shape, and failure modes. The walk-tier
load test exercises every path so regressions in any one of them surface
during a 30-minute smoke run, not in a customer escalation.

This is the reference for which loadgen driver maps to which platform
component, and what the on-wire request looks like.

## Path → driver matrix

| Path key (scenario YAML) | Driver | Endpoint | Body | Auth | Identification headers |
|--------------------------|--------|----------|------|------|------------------------|
| `rest_direct` | `RESTDirect` | `POST /v1/ingest` | Envelope JSON | Bearer | (none — direct call) |
| `sdk_node` | `SDKNode` | `POST /v1/ingest` | Envelope JSON | Bearer | `User-Agent: aforo-metering-node/<v>` + `X-SDK-Lang: node` + `X-SDK-Version: <v>` |
| `sdk_python` | `SDKPython` | `POST /v1/ingest` | Envelope JSON | Bearer | `User-Agent: aforo-metering-python/<v>` + `X-SDK-Lang: python` |
| `sdk_java` | `SDKJava` | `POST /v1/ingest` | Envelope JSON | Bearer | `User-Agent: com.aforo.metering/<v>` + `X-SDK-Lang: java` |
| `sdk_go` | `SDKGo` | `POST /v1/ingest` | Envelope JSON | Bearer | `User-Agent: metering-go/<v>` + `X-SDK-Lang: go` |
| `gateway_kong` | `GatewayKong` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: kong` + `X-Kong-*` set |
| `gateway_apigee` | `GatewayApigee` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: apigee` + `X-ApigeeProxy-Name`, `X-Apigee-Sharedflow` |
| `gateway_aws` | `GatewayAWS` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: aws-apigw` + `X-Apigateway-Api-Id` + `X-Amzn-Trace-Id` |
| `gateway_azure` | `GatewayAzure` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: azure-apim` + `X-AzureAPIM-*` |
| `gateway_mulesoft` | `GatewayMuleSoft` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: mulesoft-cloudhub` + `X-MuleSoft-*` |
| `gateway_apisix` | `GatewayAPISIX` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: apisix` + `X-APISIX-*` (stub adapter — synthesized from public docs) |
| `gateway_tyk` | `GatewayTyk` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: tyk` + `X-Tyk-*` (stub) |
| `gateway_gravitee` | `GatewayGravitee` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: gravitee` + `X-Gravitee-*` (stub) |
| `gateway_envoy` | `GatewayEnvoy` | `POST /v1/ingest` | Envelope JSON | Bearer | `X-Forwarded-By: envoy` + `X-Envoy-*` (stub) |
| `webhook_receiver` | `Webhook` | `POST /v1/ingest/webhook/{sourceId}` | Envelope JSON | HMAC-SHA256 | `X-Hub-Signature-256: sha256=<hex>` |
| `csv_upload` | `CSVUpload` | `POST /v1/ingest/upload` | multipart/form-data CSV | Bearer | `Content-Type: multipart/form-data` + `defaultMetricName`, `defaultCustomerId` form fields |

## Common envelope (JSON-bodied paths)

All JSON-bodied paths POST the same `Envelope` shape. The platform's
`UsageEventValidator` consumes this; per-driver differences are entirely in
headers.

```json
{
  "event_id":         "32-hex-chars",
  "event_timestamp":  "2026-04-30T12:00:00Z",
  "tenant_id":        "tenant-001",
  "customer_id":      "cust-abc",
  "subscription_id":  "sub-001",
  "product_type":     "API|AI_AGENT|MCP_SERVER|AGENTIC_API",
  "metric_id":        "metric-uuid",
  "body":             { /* product-type-specific payload from generator/templates.go */ }
}
```

For negative-path events tagged `malformed`, the driver sends `Event.RawBody`
as-is (already corrupt JSON by design).

## Universal headers

Every JSON-bodied driver sets these on top of its identification headers:

```
Authorization: Bearer <Event.Auth.Token>      (or ClientID:ClientSecret for CLIENT_CREDENTIALS keys)
X-Tenant-Id:   <Envelope.TenantID>
X-Customer-Id: <Envelope.CustomerID>
X-Client-Id:   <Event.Auth.ClientID>           (when key is CLIENT_CREDENTIALS)
Content-Type:  application/json
X-Loadgen-Event-Id: <Envelope.EventID>         (correlation with run.json)
X-Loadgen-Negative-Path: late|future|malformed|wrong_auth|stale_key|oversize  (when injected)
```

## Webhook driver — HMAC signing

The webhook driver mirrors Aforo's `WebhookIngestService` verification:

```
signature = lowercase_hex(HMAC-SHA256(secret, body))
header    = "X-Hub-Signature-256: sha256=" + signature
```

The platform's verifier strips the `sha256=` prefix and compares using
constant-time comparison. The driver reproduces this byte-for-byte:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(body)
hex := hex.EncodeToString(mac.Sum(nil))  // lower-case
header := "sha256=" + hex
```

### Source resolution

The webhook driver looks up the source by `Envelope.TenantID`. Sources are
loaded from `webhook_sources.json` — the sidecar file written next to
`manifest.json` when seed runs with `--provision-webhooks`.

When no source is configured for a tenant, the driver falls back to a
synthetic record (sourceId = "loadgen-<tenantId>", secret =
"loadgen-synthetic-secret"). The platform's receiver returns 404; the load
shape (HTTP envelope, signing math) still exercises the path.

## CSV upload driver — buffering semantics

The CSV driver differs from the others in two ways:

1. **It buffers events per tenant** before uploading. The default batch size
   is 100 rows. Events accumulate in the per-tenant buffer; when a tenant's
   buffer reaches the threshold, a single multipart/form-data upload to
   `/v1/ingest/upload` is fired and the Result is returned for the triggering
   event. The 99 prior buffered events report status 202 (accepted) with
   near-zero latency — the runner records them as success but the HDR
   histogram doesn't pick up real-server latency for them.

2. **It flushes at end-of-run** via `CSVUpload.Flush(ctx)`, draining all
   per-tenant buffers. The runner's Close path triggers Flush automatically.

This means the per-tenant fairness report under-reports csv_upload tail
latency relative to the other drivers. csv_upload is intentionally a
low-share path in the walk scenario (5%) so this skew doesn't dominate.

## Driver registry

The runner constructs drivers via `driver.Registry`. The registry resolves an
ingestion-path string (e.g. "sdk_node") to a constructed driver. Drivers are
constructed lazily — a scenario that doesn't reference `gateway_envoy` never
pays for its HTTP client.

`driver.AllNames()` returns the canonical list of supported names —
useful for tests and for documentation generation.

`driver.Multiplex` wraps the registry as a single `Driver` so the worker pool
fans across many drivers without spawning one pool per path. The pool's
worker goroutines are shared, which is what enables tenant-fairness
scheduling — the gate sees every event regardless of its destination.

## Adding a new driver

1. Create `internal/driver/<new>.go` implementing the `Driver` interface.
2. Add the path key to `scenario.IngestionPaths` (struct + `Sum()` + the
   `ingestionWeightsFor` call in `generator/generator.go`).
3. Add a case to `driver.Registry.construct` and a name to
   `driver.AllNames()`.
4. Add a contract test alongside the existing SDK / gateway tests.
5. Document the wire shape here.

See `internal/driver/sdk_node.go` for the canonical minimal driver.
