package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayGravitee wraps gatewayDriver with the Gravitee-specific profile.
//
// Gravitee is a stub adapter — no aforo-metering Gravitee policy in source yet.
// Header set follows gravitee.io's standard policy plugin envelope
// (gravitee-policy-* convention) per docs.gravitee.io.
type GatewayGravitee struct {
	*gatewayDriver
}

// NewGatewayGravitee constructs the Gravitee driver.
func NewGatewayGravitee(cfg HTTPBaseConfig) (*GatewayGravitee, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_gravitee",
		UserAgent:   "Gravitee-Gateway/4.4 (policy=aforo-metering-stub)",
		ForwardedBy: "gravitee",
		VendorHeaders: map[string]string{
			"X-Gravitee-Api":          "aforo-ingest",
			"X-Gravitee-Api-Version":  "1",
			"X-Gravitee-Plan":         "default",
			"X-Gravitee-Subscription": "aforo-prod",
			"X-Gravitee-Plugin":       "aforo-metering-stub",
			"X-Gravitee-Org":          "aforo",
			"X-Gravitee-Env":          "prod",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-Gravitee-Transaction-Id": genRequestID(e),
				"X-Gravitee-Request-Id":     genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayGravitee{gatewayDriver: d}, nil
}
