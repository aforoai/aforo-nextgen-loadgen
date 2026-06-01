package driver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayProfile encapsulates the per-gateway identification and envelope
// behavior. The actual transport is identical across all 9 supported
// gateways: each one PROXIES the customer's request to /v1/ingest and adds
// its own identification + observability headers (X-Forwarded-By,
// X-Gateway-Source, vendor-specific request IDs, etc.).
//
// Per-gateway plugin source is referenced where it exists (Kong: kong-plugin-
// aforo-metering/handler.lua; Apigee: apigee-shared-flow-aforo-metering;
// AWS: aws-lambda-aforo-metering/index.js; Azure: azure-apim-policy-
// aforo-metering/policy.xml; MuleSoft: mulesoft-policy-aforo-metering).
//
// APISIX, Tyk, Gravitee, Envoy are stub adapters in the platform — there's
// no plugin source yet. The header set we synthesize below is based on each
// vendor's public docs for typical request-header injection patterns.
type GatewayProfile struct {
	// Name is the driver identifier and must match the scenario YAML key.
	Name string

	// UserAgent is the gateway-emitted UA in the proxied request.
	UserAgent string

	// ForwardedBy is the value of X-Forwarded-By the gateway plugin sets.
	// Convention: lowercase vendor token ("kong", "apigee", "aws-apigw",
	// "azure-apim", "mulesoft-cloudhub", etc.).
	ForwardedBy string

	// VendorHeaders are gateway-specific headers added on top of the
	// canonical Aforo set (Authorization, X-Tenant-Id, etc.). Examples:
	//   Kong:  X-Kong-Service-Id, X-Kong-Request-Id
	//   AWS:   X-Amzn-Trace-Id, X-Apigateway-Api-Id
	//   Azure: X-AzureAPIM-RequestId, X-Forwarded-Host
	VendorHeaders map[string]string

	// HeaderForGen returns dynamic per-event headers (request IDs, traces).
	// Optional — nil means no per-event headers. The function MUST be
	// concurrency-safe; it's invoked from worker goroutines.
	HeaderForGen func(*generator.Event) map[string]string
}

// gatewayDriver is the generic implementation backed by a GatewayProfile.
// Each gateway_*.go file constructs one with its specific profile.
type gatewayDriver struct {
	cfg     HTTPBaseConfig
	client  *http.Client
	url     string
	profile GatewayProfile
}

// newGatewayDriver constructs a generic gateway driver. Returns an error if
// the target lacks a usage-ingestor URL (which is what every gateway proxies
// to in this load test).
func newGatewayDriver(cfg HTTPBaseConfig, profile GatewayProfile) (*gatewayDriver, error) {
	cfg.applyDefaults()
	if profile.Name == "" {
		return nil, fmt.Errorf("gateway: profile.Name is required")
	}
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("%s: target %s has no usage-ingestor URL: %w", profile.Name, cfg.Target.Name, err)
	}
	return &gatewayDriver{cfg: cfg, client: cfg.HTTPClient, url: url, profile: profile}, nil
}

// Name reports the driver identifier — used in metrics labels and matching
// the scenario.IngestionPaths key.
func (d *gatewayDriver) Name() string { return d.profile.Name }

// Submit dispatches one event using the gateway envelope.
func (d *gatewayDriver) Submit(ctx context.Context, e *generator.Event) Result {
	headers := map[string]string{
		"User-Agent":       d.profile.UserAgent,
		"X-Forwarded-By":   d.profile.ForwardedBy,
		"X-Gateway-Source": d.profile.ForwardedBy,
	}
	for k, v := range d.profile.VendorHeaders {
		headers[k] = v
	}
	if d.profile.HeaderForGen != nil {
		for k, v := range d.profile.HeaderForGen(e) {
			headers[k] = v
		}
	}
	req, body, err := buildJSONIngestRequest(ctx, d.url, e, d.cfg.AdminToken, headers)
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *gatewayDriver) Close() error {
	closeIdle(d.client)
	return nil
}

// genRequestID synthesizes a hex request ID for vendors that include one
// (Kong, AWS, Azure all do). Uses the event's existing event ID for
// determinism — re-running with the same seed produces the same request IDs.
func genRequestID(e *generator.Event) string {
	if e == nil {
		return ""
	}
	return e.EventID
}
