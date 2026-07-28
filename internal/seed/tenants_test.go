package seed

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// postBody finds the recorded POST (tenant-create) request body among a
// dry-run client's records. provisionTenant issues a GET (lookup) first, then a
// POST (create), so we scan for the POST rather than assuming an index.
func postBody(t *testing.T, c *Client) map[string]json.RawMessage {
	t.Helper()
	for _, rec := range c.DryRunRecords() {
		if rec.Method == http.MethodPost {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body, &body); err != nil {
				t.Fatalf("unmarshal POST body %q: %v", string(rec.Body), err)
			}
			return body
		}
	}
	t.Fatalf("no POST record found in %d dry-run records", len(c.DryRunRecords()))
	return nil
}

func dryRunClient(t *testing.T, cfg ClientConfig) *Client {
	t.Helper()
	cfg.Target = aforo.LocalTarget
	cfg.DryRun = true
	cfg.MinInterval = 1 * time.Microsecond
	cfg.Logger = log.New(io.Discard, "", 0)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestProvisionTenant_ThreadsOperatorOwner asserts that when a real operator
// identity is configured, the tenant-create payload carries the ownerUserId /
// ownerEmail / ownerName fields so org-service seeds a sign-in-able owner.
func TestProvisionTenant_ThreadsOperatorOwner(t *testing.T) {
	c := dryRunClient(t, ClientConfig{
		OperatorUserID: "usr_real_abc123",
		OperatorEmail:  "operator@aforo.ai",
		OperatorName:   "Real Operator",
	})

	if _, err := provisionTenant(context.Background(), c, "loadgen-ext-1", "Acme", "mtx-flat"); err != nil {
		t.Fatalf("provisionTenant: %v", err)
	}

	body := postBody(t, c)
	assertJSONString(t, body, "ownerUserId", "usr_real_abc123")
	assertJSONString(t, body, "ownerEmail", "operator@aforo.ai")
	assertJSONString(t, body, "ownerName", "Real Operator")
}

// TestProvisionTenant_OmitsOwnerWhenUnconfigured asserts backward compatibility:
// with no operator configured, the owner fields are absent from the payload
// (omitempty), so org-service uses its synthetic loadgen owner fallback.
func TestProvisionTenant_OmitsOwnerWhenUnconfigured(t *testing.T) {
	c := dryRunClient(t, ClientConfig{})

	if _, err := provisionTenant(context.Background(), c, "loadgen-ext-2", "Acme", "mtx-flat"); err != nil {
		t.Fatalf("provisionTenant: %v", err)
	}

	body := postBody(t, c)
	for _, k := range []string{"ownerUserId", "ownerEmail", "ownerName"} {
		if _, present := body[k]; present {
			t.Errorf("expected %q to be omitted from unconfigured payload, but it was present", k)
		}
	}
	// The synthetic fallback fields are still present, unchanged.
	assertJSONString(t, body, "externalId", "loadgen-ext-2")
	assertJSONString(t, body, "name", "Acme")
}

func assertJSONString(t *testing.T, body map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Errorf("expected %q in payload, missing", key)
		return
	}
	got := strings.Trim(string(raw), `"`)
	if got != want {
		t.Errorf("%q = %q, want %q", key, got, want)
	}
}
