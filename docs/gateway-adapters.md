# Gateway Adapters — Per-Vendor Envelope Reference

Aforo supports nine API gateway adapters in production: five with full plugin
implementations (Kong, Apigee, AWS API Gateway, Azure APIM, MuleSoft) and
four marked as stubs (APISIX, Tyk, Gravitee, Envoy). Every adapter eventually
proxies the customer's request to `/v1/ingest`, but each one decorates the
request with vendor-specific identification + observability headers.

This is the reference for what each gateway driver in
`aforo-nextgen-loadgen` adds on top of the canonical Aforo request envelope.

## Why does this matter for load testing?

Three reasons:

1. **Header parsing on the receiving side.** The platform's `UsageEventValidator`
   doesn't read these headers, but downstream consumers (analytics, audit
   log, security) categorize events by source. Generating realistic per-
   gateway header sets exercises the same code paths a production deployment
   would.

2. **HTTP/2 connection pool behavior.** Different `User-Agent` strings can
   trip header-table evictions in long-lived HTTP/2 connections. Spreading
   load across nine distinct UAs catches header-cache leaks that a single-
   UA load test wouldn't.

3. **Server-side WAF / rate-limiter routing.** Some platforms route based on
   `X-Forwarded-By`. The walk-tier load test verifies these routing rules
   don't drop traffic from any one gateway.

## Real adapters (full plugins)

These five gateways have aforo-metering plugins in the platform's source tree
(`kong-plugin-aforo-metering`, `apigee-shared-flow-aforo-metering`, etc.).
The driver in loadgen mimics the plugin's outbound request shape.

### Kong

```
User-Agent: kong/3.9.0
X-Forwarded-By: kong
X-Gateway-Source: kong
X-Kong-Service-Id: svc_aforo_ingest
X-Kong-Plugin-Name: aforo-metering
X-Kong-Plugin-Priority: 1000
X-Kong-Request-Id: <event_id>
```

Source: `kong-plugin-aforo-metering/handler.lua` — emits the headers in the
log phase before forwarding to the upstream service.

### Apigee Edge

```
User-Agent: Apigee-Edge/Cloud (sharedflow=aforo-metering)
X-Forwarded-By: apigee
X-Gateway-Source: apigee
X-ApigeeProxy-Name: aforo-ingest-proxy
X-Apigee-Org: aforo
X-Apigee-Env: prod
X-Apigee-Sharedflow: aforo-metering
X-Apigee-Plugin-Phase: PostFlow
X-Apigee-Message-Id: <event_id>
```

Source: `apigee-shared-flow-aforo-metering` — applies in the API Proxy
post-flow, then forwards to Aforo's ingest URL.

### AWS API Gateway / Lambda

```
User-Agent: AmazonAPIGateway/aforo-metering-lambda
X-Forwarded-By: aws-apigw
X-Gateway-Source: aws-apigw
X-Apigateway-Api-Id: aforo-prod-api
X-Apigateway-Stage: prod
X-Amz-Apigw-Lambda-Phase: metering
X-Aws-Region: us-east-1
X-Amzn-Trace-Id: Root=1-<8 hex>-<24 hex>
X-Amzn-RequestId: <event_id>
X-Amzn-Apigateway-Api-Id: aforo-prod-api
```

Source: `aws-lambda-aforo-metering/index.js`. The trace ID format follows
AWS X-Ray conventions (`Root=1-<epoch>-<random>`).

### Azure API Management

```
User-Agent: Microsoft-APIM/Cloud (policy=aforo-metering)
X-Forwarded-By: azure-apim
X-Gateway-Source: azure-apim
X-AzureAPIM-Service: aforo-apim-prod
X-AzureAPIM-Api: aforo-ingest
X-AzureAPIM-Operation: ingest-event
X-AzureAPIM-Region: westus2
X-AzureAPIM-Subscription: tier-prod
X-AzureAPIM-Policy-Fragment: aforo-metering
X-AzureAPIM-RequestId: <event_id>
X-Correlation-Id: <event_id>
```

Source: `azure-apim-policy-aforo-metering/policy.xml` — APIM applies the
inbound + outbound policy fragment around the proxied request.

### MuleSoft Anypoint

```
User-Agent: MuleSoft-Anypoint/aforo-metering-policy
X-Forwarded-By: mulesoft-cloudhub
X-Gateway-Source: mulesoft-cloudhub
X-MuleSoft-Org: aforo
X-MuleSoft-Env: prod
X-MuleSoft-Api: aforo-ingest
X-MuleSoft-Asset-Version: 1.0.0
X-MuleSoft-Policy: aforo-metering
X-MuleSoft-Correlation-Id: <event_id>
X-Correlation-Id: <event_id>
```

Source: `mulesoft-policy-aforo-metering` — Anypoint applies the custom
policy at the API instance level.

## Stub adapters

These four gateways don't have aforo-metering plugins yet. The header sets
below are synthesized from each vendor's public docs (`apisix.apache.org`,
`tyk.io/docs`, `docs.gravitee.io`, `envoyproxy.io`) for the standard plugin
envelope each one produces. When real plugins ship, the headers should
match — at which point the test in `internal/driver/gateway_drivers_test.go`
becomes a contract pin.

### APISIX

```
User-Agent: Apache-APISIX/3.10 (plugin=aforo-metering-stub)
X-Forwarded-By: apisix
X-Gateway-Source: apisix
X-APISIX-Route-Id: aforo-ingest-route
X-APISIX-Service-Id: aforo-ingest-svc
X-APISIX-Plugin: aforo-metering-stub
X-APISIX-Upstream: aforo-usage-ingestor
X-APISIX-Request-Id: <event_id>
```

### Tyk

```
User-Agent: Tyk-Gateway/5.4 (middleware=aforo-metering-stub)
X-Forwarded-By: tyk
X-Gateway-Source: tyk
X-Tyk-Api-Id: aforo-ingest-api
X-Tyk-Api-Slug: aforo-ingest
X-Tyk-Org: aforo
X-Tyk-Plugin: aforo-metering-stub
X-Tyk-Auth-Type: auth_token
X-Ratelimit-Limit: 0
X-Ratelimit-Remaining: 0
X-Tyk-Request-Id: <event_id>
```

### Gravitee

```
User-Agent: Gravitee-Gateway/4.4 (policy=aforo-metering-stub)
X-Forwarded-By: gravitee
X-Gateway-Source: gravitee
X-Gravitee-Api: aforo-ingest
X-Gravitee-Api-Version: 1
X-Gravitee-Plan: default
X-Gravitee-Subscription: aforo-prod
X-Gravitee-Plugin: aforo-metering-stub
X-Gravitee-Org: aforo
X-Gravitee-Env: prod
X-Gravitee-Transaction-Id: <event_id>
X-Gravitee-Request-Id: <event_id>
```

### Envoy

```
User-Agent: envoy/1.30.0 (filter=aforo-metering-stub)
X-Forwarded-By: envoy
X-Gateway-Source: envoy
X-Envoy-Original-Path: /v1/ingest
X-Envoy-Internal: true
X-Envoy-Cluster: aforo_usage_ingestor
X-Envoy-Filter: aforo-metering-stub
X-Envoy-Upstream-Service-Time: 0
X-Envoy-Decorator-Operation: ingest-event
X-Envoy-Peer-Metadata: aforo-loadgen
X-Request-Id: <event_id>
X-Envoy-Trace-Id: <event_id>
```

## When the real plugin ships

Replace the synthesized `*-stub` header values with whatever the real plugin
emits. The test `TestGatewayDrivers_AllSendCanonicalEnvelope` in
`internal/driver/gateway_drivers_test.go` asserts:

- `X-Forwarded-By` matches the gateway's vendor token
- One vendor-specific identification header is present and non-empty
- `User-Agent` looks like the vendor's plugin

These three assertions form a contract pin that catches drift when the
plugin's header set changes (or when the plugin actually ships and we need
to update the loadgen driver to match).

## Adding a tenth gateway

When the platform supports a new gateway adapter:

1. Add `gateway_<vendor>` to `scenario.IngestionPaths`.
2. Create `internal/driver/gateway_<vendor>.go` with a `GatewayProfile`
   matching the vendor's plugin output.
3. Wire it into `driver.Registry.construct` + `driver.AllNames()`.
4. Add a row to the test in `gateway_drivers_test.go`.
5. Document the header set here.

See `gateway_kong.go` for the canonical example.
