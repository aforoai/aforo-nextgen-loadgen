package driver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayDrivers_AllSendCanonicalEnvelope validates each gateway
// driver: same envelope body, /v1/ingest endpoint, gateway-specific
// identification headers, and the canonical X-Forwarded-By marker.
func TestGatewayDrivers_AllSendCanonicalEnvelope(t *testing.T) {
	cases := []struct {
		name              string
		newDriver         func(HTTPBaseConfig) (Driver, error)
		expectForwardedBy string
		// One vendor header that MUST appear so we know the profile took.
		expectVendorHdr string
	}{
		{
			"gateway_kong",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayKong(c); return d, err },
			"kong", "X-Kong-Plugin-Name",
		},
		{
			"gateway_apigee",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayApigee(c); return d, err },
			"apigee", "X-ApigeeProxy-Name",
		},
		{
			"gateway_aws",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayAWS(c); return d, err },
			"aws-apigw", "X-Apigateway-Api-Id",
		},
		{
			"gateway_azure",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayAzure(c); return d, err },
			"azure-apim", "X-AzureAPIM-Service",
		},
		{
			"gateway_mulesoft",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayMuleSoft(c); return d, err },
			"mulesoft-cloudhub", "X-MuleSoft-Org",
		},
		{
			"gateway_apisix",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayAPISIX(c); return d, err },
			"apisix", "X-APISIX-Route-Id",
		},
		{
			"gateway_tyk",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayTyk(c); return d, err },
			"tyk", "X-Tyk-Api-Id",
		},
		{
			"gateway_gravitee",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayGravitee(c); return d, err },
			"gravitee", "X-Gravitee-Api",
		},
		{
			"gateway_envoy",
			func(c HTTPBaseConfig) (Driver, error) { d, err := NewGatewayEnvoy(c); return d, err },
			"envoy", "X-Envoy-Cluster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured *http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = r.Clone(context.Background())
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()

			d, err := tc.newDriver(HTTPBaseConfig{Target: targetForServer(srv)})
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			defer func() { _ = d.Close() }()

			res := d.Submit(context.Background(), newTestEvent())
			if !res.IsSuccess() {
				t.Fatalf("expected 2xx, got status=%d err=%v", res.Status, res.TransportErr)
			}
			if d.Name() != tc.name {
				t.Errorf("driver name: got %q want %q", d.Name(), tc.name)
			}
			if captured == nil {
				t.Fatal("server captured no request")
			}
			if got := captured.URL.Path; got != "/v1/ingest" {
				t.Errorf("URL: got %q want /v1/ingest", got)
			}
			if got := captured.Header.Get("X-Forwarded-By"); got != tc.expectForwardedBy {
				t.Errorf("X-Forwarded-By: got %q want %q", got, tc.expectForwardedBy)
			}
			if got := captured.Header.Get("X-Gateway-Source"); got != tc.expectForwardedBy {
				t.Errorf("X-Gateway-Source: got %q want %q", got, tc.expectForwardedBy)
			}
			if got := captured.Header.Get(tc.expectVendorHdr); got == "" {
				t.Errorf("vendor header %s missing", tc.expectVendorHdr)
			}
			if got := captured.Header.Get("Authorization"); got != "Bearer tk_test_secret" {
				t.Errorf("auth: got %q", got)
			}
			if got := captured.Header.Get("X-Tenant-Id"); got != "tenant-alpha" {
				t.Errorf("X-Tenant-Id: got %q", got)
			}
		})
	}
}

// TestRegistry_KnownPaths ensures the registry can construct each
// supported driver by its scenario-yaml name. Catches typos between the
// switch in registry.construct and AllNames().
func TestRegistry_KnownPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	// mcp_jsonrpc requires an MCP endpoint URL — point it at the same
	// stub server so the driver can be constructed. The test only checks
	// that Get() succeeds, not that Submit() reaches a real MCP server.
	t.Setenv(MCPJsonRPCEnvURL, srv.URL)
	reg, err := NewRegistry(RegistryConfig{Target: targetForServer(srv)})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer func() { _ = reg.Close() }()
	for _, name := range AllNames() {
		t.Run(name, func(t *testing.T) {
			d, err := reg.Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			if d.Name() != name && !strings.HasPrefix(name, "csv") {
				// CSV reuses the registry name. Other drivers should report
				// their own name.
				if d.Name() != name {
					t.Errorf("driver %q reports name %q", name, d.Name())
				}
			}
		})
	}
}

// TestRegistry_UnknownPath ensures a typo in the scenario yields a
// targeted error rather than silent fallback to rest_direct.
func TestRegistry_UnknownPath(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	reg, _ := NewRegistry(RegistryConfig{Target: targetForServer(srv)})
	if _, err := reg.Get("gateway_made_up"); err == nil {
		t.Fatal("expected error for unknown driver name")
	}
}
