package seed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// TestSeederProvisionsIngestKeysInDryRun: with ProvisionIngestKeys every
// tenant in the manifest carries an ingest key; without it none does.
func TestSeederProvisionsIngestKeysInDryRun(t *testing.T) {
	for _, on := range []bool{true, false} {
		c, err := NewClient(ClientConfig{Target: aforo.LocalTarget, BearerToken: "x", DryRun: true, MinInterval: time.Microsecond})
		if err != nil {
			t.Fatal(err)
		}
		seeder, err := NewSeeder(SeederConfig{Client: c, Scenario: matrixMiniScenario(t), Concurrency: 2, ProvisionIngestKeys: on})
		if err != nil {
			t.Fatal(err)
		}
		res, err := seeder.Run(context.Background())
		_ = c.Close()
		if err != nil || len(res.Errors) > 0 {
			t.Fatalf("on=%v: run err=%v errors=%v", on, err, res.Errors)
		}
		for _, tn := range res.Manifest.Tenants {
			if on && (tn.IngestKey == nil || tn.IngestKey.RawKey == "") {
				t.Fatalf("tenant %s: want ingest key", tn.TenantID)
			}
			if !on && tn.IngestKey != nil {
				t.Fatalf("tenant %s: want no ingest key when disabled", tn.TenantID)
			}
		}
	}
}

// TestProvisionIngestKeyRequestShape locks the organization-service
// contract: POST /api/v1/api-keys with X-Tenant-Id, scope usage:ingest,
// SANDBOX environment; rawKey read from the {success,data} envelope.
func TestProvisionIngestKeyRequestShape(t *testing.T) {
	var gotPath, gotTenant string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant = r.URL.Path, r.Header.Get("X-Tenant-Id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"k1","tenantId":"t1","keyPrefix":"sk_test_abcd","scopes":["usage:ingest"],"rawKey":"sk_test_abcdSECRET"}}`))
	}))
	defer ts.Close()
	target := aforo.Target{Name: "test", URLs: map[aforo.Service]string{aforo.ServiceOrganization: ts.URL}}
	c, err := NewClient(ClientConfig{Target: target, BearerToken: "x", MinInterval: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	k, err := provisionIngestKey(context.Background(), c, "t1", "seed-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/api-keys" || gotTenant != "t1" {
		t.Fatalf("path=%q tenant=%q", gotPath, gotTenant)
	}
	scopes, _ := gotBody["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != IngestKeyScope || gotBody["environment"] != "SANDBOX" || gotBody["name"] == "" {
		t.Fatalf("unexpected body %v", gotBody)
	}
	if k.KeyID != "k1" || k.RawKey != "sk_test_abcdSECRET" {
		t.Fatalf("unexpected key %+v", k)
	}
}

func TestProvisionIngestKeyWithoutRawKeyFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"k1"}}`))
	}))
	defer ts.Close()
	target := aforo.Target{Name: "test", URLs: map[aforo.Service]string{aforo.ServiceOrganization: ts.URL}}
	c, err := NewClient(ClientConfig{Target: target, BearerToken: "x", MinInterval: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = provisionIngestKey(context.Background(), c, "t1", "seed-1")
	if !errors.Is(err, errNoRawKey) || !strings.Contains(err.Error(), "k1") {
		t.Fatalf("want errNoRawKey naming the key, got %v", err)
	}
}
