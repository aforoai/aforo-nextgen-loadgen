package driver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// SDKGoVersion mirrors the version github.com/aforo/metering-go reports.
// Synthetic until the SDK ships real source.
const SDKGoVersion = "1.1.0"

// sdkGoUserAgent follows Go's net/http convention:
//
//	"<package>/<version> (Go/<go-ver>)"
const sdkGoUserAgent = "metering-go/" + SDKGoVersion + " (Go/1.22; loadgen)"

// SDKGo mimics the github.com/aforo/metering-go SDK on-wire shape.
type SDKGo struct {
	cfg    HTTPBaseConfig
	client *http.Client
	url    string
}

// NewSDKGo constructs the Go SDK driver.
func NewSDKGo(cfg HTTPBaseConfig) (*SDKGo, error) {
	cfg.applyDefaults()
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("sdk_go: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &SDKGo{cfg: cfg, client: cfg.HTTPClient, url: url}, nil
}

// Name reports the driver identifier.
func (d *SDKGo) Name() string { return "sdk_go" }

// Submit dispatches one event using the Go SDK envelope.
func (d *SDKGo) Submit(ctx context.Context, e *generator.Event) Result {
	req, body, err := buildJSONIngestRequest(ctx, d.url, e, d.cfg.AdminToken, map[string]string{
		"User-Agent":    sdkGoUserAgent,
		"X-SDK-Lang":    "go",
		"X-SDK-Version": SDKGoVersion,
	})
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *SDKGo) Close() error {
	closeIdle(d.client)
	return nil
}
