package generator

import (
	mathrand "math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

func TestIngestKeyOf(t *testing.T) {
	if ingestKeyOf(nil) != "" || ingestKeyOf(&seed.ManifestTenant{}) != "" {
		t.Fatal("no tenant / no key must yield empty")
	}
	tn := &seed.ManifestTenant{IngestKey: &seed.ManifestIngestKey{RawKey: "sk_test_x"}}
	if ingestKeyOf(tn) != "sk_test_x" {
		t.Fatal("want the tenant's raw ingest key")
	}
}

// TestWrongAuthClearsIngestKey: the fabricated key must be what is sent,
// otherwise the valid ingest key wins and the negative path is accepted.
func TestWrongAuthClearsIngestKey(t *testing.T) {
	e := &Event{Auth: EventAuth{Token: "sk_live_real", IngestKey: "sk_test_valid"}}
	if err := injectWrongAuth(e, mathrand.New(mathrand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	if e.Auth.IngestKey != "" || e.Auth.Token == "sk_live_real" {
		t.Fatalf("want fabricated token and no ingest key, got %+v", e.Auth)
	}
}
