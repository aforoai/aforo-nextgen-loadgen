package aforo

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestResolveTarget_Predefined(t *testing.T) {
	tests := []struct {
		name string
		want Target
	}{
		{"local", LocalTarget},
		{"staging", StagingTarget},
		{"prod", ProdTarget},
	}
	for _, tc := range tests {
		got, err := ResolveTarget(tc.name)
		if err != nil {
			t.Errorf("ResolveTarget(%s): %v", tc.name, err)
		}
		if got.Name != tc.want.Name {
			t.Errorf("ResolveTarget(%s).Name = %s, want %s", tc.name, got.Name, tc.want.Name)
		}
	}
}

func TestResolveTarget_CustomURL(t *testing.T) {
	got, err := ResolveTarget("http://localhost:9999")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	for _, svc := range AllServices {
		u, err := got.URL(svc)
		if err != nil {
			t.Errorf("URL(%s): %v", svc, err)
		}
		if u != "http://localhost:9999" {
			t.Errorf("svc %s URL = %s", svc, u)
		}
	}
}

func TestResolveTarget_Unknown(t *testing.T) {
	_, err := ResolveTarget("unknown-target")
	if err == nil {
		t.Errorf("expected error for unknown target")
	}
}

func TestResolveTarget_CI_NoEnv_FallsBackToStagingURLs(t *testing.T) {
	// Clear all CI env vars to ensure we measure the staging-fallback path.
	t.Setenv("AFORO_CI_BASE_URL", "")
	for _, svc := range AllProbeServices {
		t.Setenv("AFORO_CI_"+ciEnvSafeServiceName(svc)+"_URL", "")
	}

	got, err := ResolveTarget("ci")
	if err != nil {
		t.Fatalf("ResolveTarget(ci): %v", err)
	}
	if got.Name != "ci" {
		t.Errorf("Name = %q, want %q", got.Name, "ci")
	}
	// Staging URLs are inherited verbatim.
	for svc, want := range StagingTarget.URLs {
		got, err := got.URL(svc)
		if want == "" {
			// ai-service has no public hostname; URL("") would error and
			// that's expected — see StagingTarget commentary.
			continue
		}
		if err != nil {
			t.Errorf("URL(%s): %v", svc, err)
			continue
		}
		if got != want {
			t.Errorf("svc %s URL = %s, want %s", svc, got, want)
		}
	}
}

func TestResolveTarget_CI_BaseURL_FansAllServices(t *testing.T) {
	t.Setenv("AFORO_CI_BASE_URL", "https://pr-42.aforo.dev/")

	got, err := ResolveTarget("ci")
	if err != nil {
		t.Fatalf("ResolveTarget(ci): %v", err)
	}
	for _, svc := range AllProbeServices {
		u, err := got.URL(svc)
		if err != nil {
			t.Errorf("URL(%s): %v", svc, err)
			continue
		}
		// Trailing slash should be trimmed for clean joins.
		if u != "https://pr-42.aforo.dev" {
			t.Errorf("svc %s URL = %s, want trimmed base", svc, u)
		}
	}
}

func TestResolveTarget_CI_PerServiceOverride(t *testing.T) {
	t.Setenv("AFORO_CI_BASE_URL", "")
	t.Setenv("AFORO_CI_USAGE_INGESTOR_URL", "https://usage-pr-99.aforo.dev")
	defer t.Setenv("AFORO_CI_USAGE_INGESTOR_URL", "")

	got, err := ResolveTarget("ci")
	if err != nil {
		t.Fatalf("ResolveTarget(ci): %v", err)
	}
	u, err := got.URL(ServiceUsageIngestor)
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://usage-pr-99.aforo.dev" {
		t.Errorf("usage-ingestor URL = %s, want override", u)
	}
	// Other services keep their staging defaults.
	u2, err := got.URL(ServicePricing)
	if err != nil {
		t.Fatal(err)
	}
	if u2 != "https://pricing.aforo.space" {
		t.Errorf("pricing URL = %s, want staging default", u2)
	}
}

func TestCIEnvSafeServiceName(t *testing.T) {
	tests := []struct {
		in   Service
		want string
	}{
		{ServiceUsageIngestor, "USAGE_INGESTOR"},
		{ServiceAIService, "AI_SERVICE"},
		{ServicePricing, "PRICING"},
	}
	for _, tc := range tests {
		if got := ciEnvSafeServiceName(tc.in); got != tc.want {
			t.Errorf("ciEnvSafeServiceName(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestTargetPath(t *testing.T) {
	tgt := LocalTarget
	got, err := tgt.Path(ServiceOrganization, PathInternalTenants)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://localhost:8086/api/v1/internal/tenants"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestTargetPath_TrimsTrailingSlash(t *testing.T) {
	tgt := Target{
		Name: "test",
		URLs: map[Service]string{ServicePricing: "http://x:8083/"},
	}
	got, _ := tgt.Path(ServicePricing, "/api/v1/rate-plans")
	if got != "http://x:8083/api/v1/rate-plans" {
		t.Errorf("trailing slash not trimmed: %s", got)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		isNF   bool
		isConf bool
		isAuth bool
		isRetr bool
	}{
		{"404", &APIError{Status: 404}, true, false, false, false},
		{"409", &APIError{Status: 409}, false, true, false, false},
		{"401", &APIError{Status: 401}, false, false, true, false},
		{"403", &APIError{Status: 403}, false, false, true, false},
		{"500", &APIError{Status: 500}, false, false, false, true},
		{"429", &APIError{Status: 429}, false, false, false, true},
		{"408", &APIError{Status: 408}, false, false, false, true},
		{"422", &APIError{Status: 422}, false, false, false, false},
		{"transport err", &APIError{UnderlyingErr: errors.New("dial: refused")}, false, false, false, true},
		{"non APIError nil", nil, false, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.isNF {
				t.Errorf("IsNotFound = %v, want %v", got, tc.isNF)
			}
			if got := IsConflict(tc.err); got != tc.isConf {
				t.Errorf("IsConflict = %v, want %v", got, tc.isConf)
			}
			if got := IsUnauthorized(tc.err); got != tc.isAuth {
				t.Errorf("IsUnauthorized = %v, want %v", got, tc.isAuth)
			}
			if got := IsRetryable(tc.err); got != tc.isRetr {
				t.Errorf("IsRetryable = %v, want %v", got, tc.isRetr)
			}
		})
	}
}

func TestAPIErrorMessageDoesNotLeakSecrets(t *testing.T) {
	// Defense-in-depth: error rendering shouldn't accidentally include the
	// bearer token even if it was somehow added to the URL.
	e := &APIError{Method: http.MethodPost, URL: "http://x", Status: 500, Body: "internal error"}
	msg := e.Error()
	if strings.Contains(msg, "Bearer") {
		t.Errorf("error message contains 'Bearer': %s", msg)
	}
}
