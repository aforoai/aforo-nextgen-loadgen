package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayEnvoy wraps gatewayDriver with the Envoy-specific profile.
//
// Envoy is a stub adapter — no aforo-metering Envoy filter in source yet.
// Header set follows envoyproxy.io's standard ext_proc / Wasm filter
// envelope, plus the canonical X-Envoy-* request metadata Envoy emits in
// every proxied request (Original-Path, Internal, Upstream-Service-Time
// when wrapped in a metering filter).
type GatewayEnvoy struct {
	*gatewayDriver
}

// NewGatewayEnvoy constructs the Envoy driver.
func NewGatewayEnvoy(cfg HTTPBaseConfig) (*GatewayEnvoy, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_envoy",
		UserAgent:   "envoy/1.30.0 (filter=aforo-metering-stub)",
		ForwardedBy: "envoy",
		VendorHeaders: map[string]string{
			"X-Envoy-Original-Path":            "/v1/ingest",
			"X-Envoy-Internal":                 "true",
			"X-Envoy-Cluster":                  "aforo_usage_ingestor",
			"X-Envoy-Filter":                   "aforo-metering-stub",
			"X-Envoy-Upstream-Service-Time":    "0",
			"X-Envoy-Decorator-Operation":      "ingest-event",
			"X-Envoy-Peer-Metadata":            "aforo-loadgen",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			return map[string]string{
				"X-Request-Id":      genRequestID(e),
				"X-Envoy-Trace-Id":  genRequestID(e),
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayEnvoy{gatewayDriver: d}, nil
}
