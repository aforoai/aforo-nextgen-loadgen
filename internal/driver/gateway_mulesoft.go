package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayMuleSoft wraps gatewayDriver with the MuleSoft-specific profile.
//
// Wire shape mirrored from mulesoft-policy-aforo-metering:
//   - Anypoint platform applies the custom policy at the API instance level.
//   - Policy adds X-MuleSoft-* identification + correlation headers and
//     forwards the request unchanged to /v1/ingest.
type GatewayMuleSoft struct {
	*gatewayDriver
}

// NewGatewayMuleSoft constructs the MuleSoft driver.
func NewGatewayMuleSoft(cfg HTTPBaseConfig) (*GatewayMuleSoft, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_mulesoft",
		UserAgent:   "MuleSoft-Anypoint/aforo-metering-policy",
		ForwardedBy: "mulesoft-cloudhub",
		VendorHeaders: map[string]string{
			"X-MuleSoft-Org":           "aforo",
			"X-MuleSoft-Env":           "prod",
			"X-MuleSoft-Api":           "aforo-ingest",
			"X-MuleSoft-Asset-Version": "1.0.0",
			"X-MuleSoft-Policy":        "aforo-metering",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-MuleSoft-Correlation-Id": genRequestID(e),
				"X-Correlation-Id":          genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayMuleSoft{gatewayDriver: d}, nil
}
