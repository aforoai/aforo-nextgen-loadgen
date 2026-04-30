package driver

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// GatewayAWS wraps gatewayDriver with AWS API Gateway / Lambda profile.
//
// Wire shape mirrored from aws-lambda-aforo-metering/index.js:
//   - AWS API Gateway invokes the Aforo Lambda authorizer + metering Lambda.
//   - The Lambda forwards the request body to /v1/ingest with AWS-canonical
//     trace headers (X-Amzn-Trace-Id) and execution metadata.
type GatewayAWS struct {
	*gatewayDriver
}

// NewGatewayAWS constructs the AWS driver.
func NewGatewayAWS(cfg HTTPBaseConfig) (*GatewayAWS, error) {
	d, err := newGatewayDriver(cfg, GatewayProfile{
		Name:        "gateway_aws",
		UserAgent:   "AmazonAPIGateway/aforo-metering-lambda",
		ForwardedBy: "aws-apigw",
		VendorHeaders: map[string]string{
			"X-Apigateway-Api-Id":      "aforo-prod-api",
			"X-Apigateway-Stage":       "prod",
			"X-Amz-Apigw-Lambda-Phase": "metering",
			"X-Aws-Region":             "us-east-1",
		},
		HeaderForGen: func(e *generator.Event) map[string]string {
			rid := genRequestID(e)
			return map[string]string{
				// X-Amzn-Trace-Id format: "Root=1-<8 hex>-<24 hex>"
				"X-Amzn-Trace-Id":          "Root=1-" + safeSlice(rid, 0, 8) + "-" + safeSlice(rid, 8, 32),
				"X-Amzn-RequestId":         rid,
				"X-Amzn-Apigateway-Api-Id": "aforo-prod-api",
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &GatewayAWS{gatewayDriver: d}, nil
}

// safeSlice returns s[lo:hi] padded with zeros when too short. Keeps trace
// header construction robust to short event IDs (defensive — generator emits
// 32 hex chars, but tests inject short IDs).
func safeSlice(s string, lo, hi int) string {
	if hi <= lo {
		return ""
	}
	want := hi - lo
	if lo >= len(s) {
		return zeros(want)
	}
	end := hi
	if end > len(s) {
		end = len(s)
	}
	out := s[lo:end]
	if len(out) < want {
		out += zeros(want - len(out))
	}
	return out
}

func zeros(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
