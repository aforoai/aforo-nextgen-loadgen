package driver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// SDKPythonVersion mirrors the version the aforo-metering Python SDK
// reports in its User-Agent. Synthetic until the SDK ships real source.
const SDKPythonVersion = "1.3.0"

// sdkPythonUserAgent follows the requests/httpx convention:
//
//	"<package>/<version> python/<py-ver>"
const sdkPythonUserAgent = "aforo-metering-python/" + SDKPythonVersion + " python/3.11"

// SDKPython mimics the aforo-metering Python SDK on-wire shape. Same
// envelope as Node; differs only in identification headers and
// User-Agent so server-side SDK analytics break out cleanly.
type SDKPython struct {
	cfg    HTTPBaseConfig
	client *http.Client
	url    string
}

// NewSDKPython constructs the Python SDK driver.
func NewSDKPython(cfg HTTPBaseConfig) (*SDKPython, error) {
	cfg.applyDefaults()
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("sdk_python: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &SDKPython{cfg: cfg, client: cfg.HTTPClient, url: url}, nil
}

// Name reports the driver identifier.
func (d *SDKPython) Name() string { return "sdk_python" }

// Submit dispatches one event using the Python SDK envelope.
func (d *SDKPython) Submit(ctx context.Context, e *generator.Event) Result {
	req, body, err := buildJSONIngestRequest(ctx, d.url, e, d.cfg.AdminToken, map[string]string{
		"User-Agent":    sdkPythonUserAgent,
		"X-SDK-Lang":    "python",
		"X-SDK-Version": SDKPythonVersion,
	})
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *SDKPython) Close() error {
	closeIdle(d.client)
	return nil
}
