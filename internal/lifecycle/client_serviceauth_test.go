package lifecycle

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/serviceauth"
)

func singleTarget(u string) aforo.Target {
	t := aforo.Target{Name: "test", URLs: map[aforo.Service]string{}}
	for _, s := range aforo.AllProbeServices {
		t.URLs[s] = u
	}
	return t
}

// G375: lifecycle/payments calls to internal endpoints (e.g. the
// /api/v1/internal/payments/drive fallback) carry loadgen's service token,
// never the fleet-wide shared key; public endpoints carry neither.
func TestClient_G375_ServiceTokenOnInternalPaths(t *testing.T) {
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(context.Background()))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	auth := serviceauth.NewSigner(serviceauth.Config{PrivateKey: key, KeyID: "loadgen-test", SharedKey: "fleet"},
		singleTarget(srv.URL), log.New(io.Discard, "", 0))
	c, _ := NewClient(ClientConfig{Target: singleTarget(srv.URL), Token: "t", ServiceAuth: auth})

	ctx := context.Background()
	if _, err := c.PostJSON(ctx, aforo.ServiceBilling, "/api/v1/invoices/i1/charge", "t1", "k1", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PostJSON(ctx, aforo.ServiceBilling, "/api/v1/internal/payments/drive", "t1", "k2", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if got[0].Header.Get(serviceauth.Header) != "" || got[0].Header.Get(serviceauth.SharedKeyHeader) != "" {
		t.Error("public endpoint must carry no internal credential")
	}
	if got[1].Header.Get(serviceauth.Header) == "" {
		t.Error("internal endpoint without service token")
	}
	if got[1].Header.Get(serviceauth.SharedKeyHeader) != "" {
		t.Error("shared key sent without opt-in")
	}
}

func TestClient_G375_NoCredentialFailsBeforeSending(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{Target: singleTarget(srv.URL), Token: "t"})
	_, err := c.PostJSON(context.Background(), aforo.ServiceBilling, "/api/v1/internal/payments/drive", "t1", "k", map[string]any{}, nil)
	if !errors.Is(err, serviceauth.ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
	if hits != 0 {
		t.Errorf("request sent %d time(s) without a credential", hits)
	}
}
