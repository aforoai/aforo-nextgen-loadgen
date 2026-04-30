package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayApigee wraps gatewayDriver with Apigee-specific identification.
//
// Wire shape mirrored from apigee-shared-flow-aforo-metering:
//   - Apigee Edge proxies the request through a Shared Flow that adds
//     identification + observability headers, then forwards to /v1/ingest.
//   - Apigee always sets X-ApigeeProxy-Name + a per-request message-id.
type GatewayApigee struct {
	*gatewayDriver
}

// NewGatewayApigee constructs the Apigee driver.
func NewGatewayApigee(cfg HTTPBaseConfig) (*GatewayApigee, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_apigee",
		UserAgent:   "Apigee-Edge/Cloud (sharedflow=aforo-metering)",
		ForwardedBy: "apigee",
		VendorHeaders: map[string]string{
			"X-ApigeeProxy-Name":    "aforo-ingest-proxy",
			"X-Apigee-Org":          "aforo",
			"X-Apigee-Env":          "prod",
			"X-Apigee-Sharedflow":   "aforo-metering",
			"X-Apigee-Plugin-Phase": "PostFlow",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-Apigee-Message-Id": genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayApigee{gatewayDriver: d}, nil
}
