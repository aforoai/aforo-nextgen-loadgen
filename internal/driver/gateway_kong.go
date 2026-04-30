package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayKong wraps gatewayDriver with the Kong-specific profile.
//
// Wire shape mirrored from kong-plugin-aforo-metering/handler.lua:
//   - Request body proxied unchanged to /v1/ingest.
//   - User-Agent rewritten to "kong/<v>".
//   - X-Forwarded-By: "kong" (Aforo plugin convention).
//   - X-Kong-Request-Id, X-Kong-Service-Id added per-request.
type GatewayKong struct {
	*gatewayDriver
}

// NewGatewayKong constructs the Kong gateway driver.
func NewGatewayKong(cfg HTTPBaseConfig) (*GatewayKong, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_kong",
		UserAgent:   "kong/3.9.0",
		ForwardedBy: "kong",
		VendorHeaders: map[string]string{
			"X-Kong-Service-Id":      "svc_aforo_ingest",
			"X-Kong-Plugin-Name":     "aforo-metering",
			"X-Kong-Plugin-Priority": "1000",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-Kong-Request-Id": genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayKong{gatewayDriver: d}, nil
}
