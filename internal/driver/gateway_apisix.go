package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayAPISIX wraps gatewayDriver with the APISIX-specific profile.
//
// APISIX is a stub adapter on the Aforo platform — there's no aforo-metering
// APISIX plugin in source yet. Header set is synthesized from APISIX's
// public docs (apisix.apache.org), specifically the standard plugin envelope
// for proxy-rewrite + http-logger plugins. Real plugin integration will
// produce the same shape.
type GatewayAPISIX struct {
	*gatewayDriver
}

// NewGatewayAPISIX constructs the APISIX driver.
func NewGatewayAPISIX(cfg HTTPBaseConfig) (*GatewayAPISIX, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_apisix",
		UserAgent:   "Apache-APISIX/3.10 (plugin=aforo-metering-stub)",
		ForwardedBy: "apisix",
		VendorHeaders: map[string]string{
			"X-APISIX-Route-Id":   "aforo-ingest-route",
			"X-APISIX-Service-Id": "aforo-ingest-svc",
			"X-APISIX-Plugin":     "aforo-metering-stub",
			"X-APISIX-Upstream":   "aforo-usage-ingestor",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-APISIX-Request-Id": genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayAPISIX{gatewayDriver: d}, nil
}
