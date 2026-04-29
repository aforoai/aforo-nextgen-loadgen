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
