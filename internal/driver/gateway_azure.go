package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayAzure wraps gatewayDriver with Azure APIM-specific identification.
//
// Wire shape mirrored from azure-apim-policy-aforo-metering/policy.xml:
//   - Azure APIM applies the policy fragment in inbound + outbound; the
//     policy proxies to /v1/ingest with APIM identification headers.
//   - Always sets X-AzureAPIM-RequestId and the consumption tier subscription id.
type GatewayAzure struct {
	*gatewayDriver
}

// NewGatewayAzure constructs the Azure APIM driver.
func NewGatewayAzure(cfg HTTPBaseConfig) (*GatewayAzure, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_azure",
		UserAgent:   "Microsoft-APIM/Cloud (policy=aforo-metering)",
		ForwardedBy: "azure-apim",
		VendorHeaders: map[string]string{
			"X-AzureAPIM-Service":         "aforo-apim-prod",
			"X-AzureAPIM-Api":             "aforo-ingest",
			"X-AzureAPIM-Operation":       "ingest-event",
			"X-AzureAPIM-Region":          "westus2",
			"X-AzureAPIM-Subscription":    "tier-prod",
			"X-AzureAPIM-Policy-Fragment": "aforo-metering",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-AzureAPIM-RequestId": genRequestID(e),
				// Mirror Azure's standard correlation header.
				"X-Correlation-Id": genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayAzure{gatewayDriver: d}, nil
}
