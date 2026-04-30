package driver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// SDKJavaVersion mirrors the version the com.aforo:metering Java SDK
// reports. Synthetic until the SDK ships real source.
const SDKJavaVersion = "1.2.5"

// sdkJavaUserAgent follows the OkHttp convention:
//
//	"<package>/<version> Java/<jdk> okhttp/<v>"
const sdkJavaUserAgent = "com.aforo.metering/" + SDKJavaVersion + " Java/17 okhttp/4.12.0"

// SDKJava mimics the com.aforo:metering Java SDK on-wire shape. Same JSON
// envelope as the others; identification headers differ.
type SDKJava struct {
	cfg    HTTPBaseConfig
	client *http.Client
	url    string
}

// NewSDKJava constructs the Java SDK driver.
func NewSDKJava(cfg HTTPBaseConfig) (*SDKJava, error) {
	cfg.applyDefaults()
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("sdk_java: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &SDKJava{cfg: cfg, client: cfg.HTTPClient, url: url}, nil
}

// Name reports the driver identifier.
func (d *SDKJava) Name() string { return "sdk_java" }

// Submit dispatches one event using the Java SDK envelope.
func (d *SDKJava) Submit(ctx context.Context, e *generator.Event) Result {
	req, body, err := buildJSONIngestRequest(ctx, d.url, e, d.cfg.AdminToken, map[string]string{
		"User-Agent":    sdkJavaUserAgent,
		"X-SDK-Lang":    "java",
		"X-SDK-Version": SDKJavaVersion,
	})
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *SDKJava) Close() error {
	closeIdle(d.client)
	return nil
}
