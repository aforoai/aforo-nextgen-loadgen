package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayTyk wraps gatewayDriver with the Tyk-specific profile.
//
// Tyk is a stub adapter — no aforo-metering Tyk middleware in source yet.
// Header set is synthesized from tyk.io public docs for the standard
// pre/post middleware envelope, including the canonical X-Ratelimit-*
// breadcrumbs Tyk emits even when the plugin doesn't rate-limit itself.
type GatewayTyk struct {
	*gatewayDriver
}

// NewGatewayTyk constructs the Tyk driver.
func NewGatewayTyk(cfg HTTPBaseConfig) (*GatewayTyk, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_tyk",
		UserAgent:   "Tyk-Gateway/5.4 (middleware=aforo-metering-stub)",
		ForwardedBy: "tyk",
		VendorHeaders: map[string]string{
			"X-Tyk-Api-Id":          "aforo-ingest-api",
			"X-Tyk-Api-Slug":        "aforo-ingest",
			"X-Tyk-Org":             "aforo",
			"X-Tyk-Plugin":          "aforo-metering-stub",
			"X-Tyk-Auth-Type":       "auth_token",
			"X-Ratelimit-Limit":     "0",
			"X-Ratelimit-Remaining": "0",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-Tyk-Request-Id": genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayTyk{gatewayDriver: d}, nil
}
