package driver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// SDKNodeVersion is the synthesized client version embedded in headers.
// Kept distinct from RawSDKVersion so when the real @aforo/metering Node
// SDK ships and emits its own User-Agent, we can match it here without
// touching every test.
const SDKNodeVersion = "1.4.2"

// sdkNodeUserAgent matches the convention typical Node.js HTTP libraries
// produce: "<package>/<version> (<language>/<runtime>; <platform>)".
const sdkNodeUserAgent = "aforo-metering-node/" + SDKNodeVersion + " (node/20; loadgen)"

// SDKNode mimics the on-wire shape of @aforo/metering Node.js client.
//
// Wire shape (verified per CLAUDE.md "Customer Ingestion Integration Layer"):
//   - POST /v1/ingest
//   - Same envelope JSON body as rest_direct
//   - Authorization: Bearer <token>
//   - X-Tenant-Id, X-Customer-Id headers
//   - User-Agent identifies the SDK + version + runtime
//   - X-SDK-Lang: node / X-SDK-Version: <ver>  — cross-SDK structured headers
//     so server-side analytics can group by SDK without parsing User-Agent
type SDKNode struct {
	cfg    HTTPBaseConfig
	client *http.Client
	url    string
}

// NewSDKNode constructs the Node SDK driver.
func NewSDKNode(cfg HTTPBaseConfig) (*SDKNode, error) {
	cfg.applyDefaults()
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("sdk_node: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &SDKNode{cfg: cfg, client: cfg.HTTPClient, url: url}, nil
}

// Name reports the driver identifier — used by metrics labels and by the
// scenario.IngestionPaths key resolver.
func (d *SDKNode) Name() string { return "sdk_node" }

// Submit dispatches one event using the Node SDK envelope.
func (d *SDKNode) Submit(ctx context.Context, e *generator.Event) Result {
	req, body, err := buildJSONIngestRequest(ctx, d.url, e, d.cfg.AdminToken, map[string]string{
		"User-Agent":    sdkNodeUserAgent,
		"X-SDK-Lang":    "node",
		"X-SDK-Version": SDKNodeVersion,
	})
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *SDKNode) Close() error {
	closeIdle(d.client)
	return nil
}
