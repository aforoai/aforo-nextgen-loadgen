package seed

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/serviceauth"
)

// G375: the fleet-wide X-Internal-Api-Key used to be forwarded on every
// seed request. Internal endpoints now get loadgen's own service token and
// the shared key is never sent by default.
func TestClient_G375_TokenOnInternalOnly_NoSharedKey(t *testing.T) {
	var got []http.Header
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	// AFORO_INTERNAL_API_KEY set, but the fallback not opted in.
	auth, err := serviceauth.NewSignerFromEnv(func(k string) string {
		if k == serviceauth.EnvSharedKey {
			return "fleet-shared-secret"
		}
		return ""
	}, singleHostTarget(srv.URL), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if auth.HasCredential() {
		t.Fatal("shared key without opt-in must not count as a credential")
	}

	c, err := NewClient(ClientConfig{Target: singleHostTarget(srv.URL), BearerToken: "t", ServiceAuth: testServiceAuth(t),
		MinInterval: time.Microsecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if err := c.Do(ctx, http.MethodGet, srv.URL+"/api/v1/products", nil, nil, RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, srv.URL+"/internal/v1/billing-entities/provision-default", nil, nil,
		RequestOptions{TenantID: "lg-1", Service: aforo.ServiceBilling}); err != nil {
		t.Fatal(err)
	}
	for i, h := range got {
		if h.Get(serviceauth.SharedKeyHeader) != "" {
			t.Errorf("%s: fleet-wide X-Internal-Api-Key forwarded", paths[i])
		}
	}
	if got[0].Get(serviceauth.Header) != "" {
		t.Error("service token sent to a non-internal path")
	}
	if got[1].Get(serviceauth.Header) == "" {
		t.Error("internal path without service token")
	}
}

func TestClient_G375_NoCredentialFailsBeforeSendingWithoutRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c, err := NewClient(ClientConfig{Target: singleHostTarget(srv.URL), BearerToken: "t",
		MaxRetries: 3, BaseBackoff: time.Millisecond, MinInterval: time.Microsecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Do(context.Background(), http.MethodGet, srv.URL+"/api/v1/internal/tenants", nil, nil,
		RequestOptions{Service: aforo.ServiceOrganization})
	if !errors.Is(err, serviceauth.ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("request reached the server %d time(s); must fail before sending", n)
	}
}

func TestClient_G375_SharedKeyFallbackOptIn(t *testing.T) {
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	auth := serviceauth.NewSigner(serviceauth.Config{SharedKey: "fleet-shared", SendSharedKey: true},
		singleHostTarget(srv.URL), log.New(io.Discard, "", 0))
	c, err := NewClient(ClientConfig{Target: singleHostTarget(srv.URL), BearerToken: "t", ServiceAuth: auth,
		MinInterval: time.Microsecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	_ = c.Do(ctx, http.MethodGet, srv.URL+"/api/v1/customers", nil, nil, RequestOptions{})
	_ = c.Do(ctx, http.MethodGet, srv.URL+"/api/v1/internal/tenants", nil, nil, RequestOptions{Service: aforo.ServiceOrganization})
	if got[0].Get(serviceauth.SharedKeyHeader) != "" {
		t.Error("shared key must not go to non-internal paths even with the fallback on")
	}
	if got[1].Get(serviceauth.SharedKeyHeader) != "fleet-shared" {
		t.Error("opted-in fallback must send the shared key on internal paths")
	}
}

func TestClient_G375_DryRunRecordsMaskedToken(t *testing.T) {
	c, err := NewClient(ClientConfig{Target: aforo.LocalTarget, DryRun: true, ServiceAuth: testServiceAuth(t),
		Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Do(context.Background(), http.MethodPost, "http://localhost:8086/api/v1/internal/tenants", map[string]string{}, nil,
		RequestOptions{Service: aforo.ServiceOrganization})
	rec := c.DryRunRecords()
	if len(rec) != 1 || rec[0].Headers.Get(serviceauth.Header) != "***" || rec[0].Headers.Get(serviceauth.SharedKeyHeader) != "" {
		t.Errorf("dry-run record headers: %v", rec)
	}
}
