package erp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubDoer lets tests inject precise responses per-request.
type stubDoer struct {
	respond func(*http.Request) (*http.Response, error)
}

func (s *stubDoer) Do(r *http.Request) (*http.Response, error) {
	return s.respond(r)
}

func bodyOK(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func body404(body string) *http.Response {
	return &http.Response{
		StatusCode: 404,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestProviderRegistry(t *testing.T) {
	for _, name := range AllProviders {
		p, err := Build(name)
		if err != nil {
			t.Fatalf("Build(%s): %v", name, err)
		}
		if p.Name() != name {
			t.Errorf("p.Name()=%s; want %s", p.Name(), name)
		}
	}
	if _, err := Build("nope"); err == nil {
		t.Error("unknown provider must fail")
	}
}

func TestIsKnown(t *testing.T) {
	if !IsKnown("quickbooks") {
		t.Error("quickbooks should be known")
	}
	if IsKnown("nope") {
		t.Error("'nope' should not be known")
	}
}

func TestQuickBooks_ShadowMode(t *testing.T) {
	t.Setenv("QBO_ACCESS_TOKEN", "")
	t.Setenv("QBO_COMPANY_ID", "")
	q := NewQuickBooks()
	if q.IsLive() {
		t.Fatal("expected shadow mode without env")
	}
	ok, _, err := q.Verify(context.Background(), "doc-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("shadow mode must return ok=true")
	}
}

func TestQuickBooks_LiveOK(t *testing.T) {
	q := &QuickBooks{
		commonClient: commonClient{
			enabled:    true,
			baseURL:    "https://x",
			authHeader: "Bearer token",
			httpClient: &stubDoer{respond: func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.URL.Path, "/v3/company/realm/invoice/doc-1") {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				return bodyOK(`{"Invoice":{"Id":"doc-1","DocNumber":"INV-1"}}`), nil
			}},
		},
		companyID: "realm",
	}
	ok, reason, err := q.Verify(context.Background(), "doc-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true; got reason %q", reason)
	}
}

func TestQuickBooks_NotFound(t *testing.T) {
	q := &QuickBooks{
		commonClient: commonClient{
			enabled:    true,
			baseURL:    "https://x",
			authHeader: "Bearer token",
			httpClient: &stubDoer{respond: func(r *http.Request) (*http.Response, error) {
				return body404("{}"), nil
			}},
		},
		companyID: "realm",
	}
	ok, reason, _ := q.Verify(context.Background(), "missing")
	if ok || !strings.Contains(reason, "404") {
		t.Errorf("expected 404 reason; got ok=%v reason=%q", ok, reason)
	}
}

func TestXero_LiveOK(t *testing.T) {
	x := &Xero{
		commonClient: commonClient{
			enabled:    true,
			baseURL:    "https://api.xero.com",
			authHeader: "Bearer token",
			httpClient: &stubDoer{respond: func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Xero-Tenant-Id") != "tenant-x" {
					t.Errorf("missing Xero-Tenant-Id header")
				}
				return bodyOK(`{"Invoices":[{"InvoiceID":"abc"}]}`), nil
			}},
		},
		tenantID: "tenant-x",
	}
	ok, _, _ := x.Verify(context.Background(), "abc")
	if !ok {
		t.Error("expected ok")
	}
}

func TestNetSuite_ShadowMode(t *testing.T) {
	t.Setenv("NETSUITE_REST_TOKEN", "")
	t.Setenv("NETSUITE_ACCOUNT_ID", "")
	n := NewNetSuite()
	if n.IsLive() {
		t.Fatal("expected shadow")
	}
	ok, _, _ := n.Verify(context.Background(), "1")
	if !ok {
		t.Error("shadow mode must say ok")
	}
}

func TestCustomWebhook_RoundTrip(t *testing.T) {
	cw := NewCustomWebhook()
	defer cw.Close()
	if !cw.IsLive() {
		t.Fatal("expected live receiver")
	}
	body := []byte(`{"invoice_id":"INV-42","amount_usd":100}`)
	sig := SignBody(body, cw.Secret())

	req, _ := http.NewRequest(http.MethodPost, cw.URL(), strings.NewReader(string(body)))
	req.Header.Set("X-Aforo-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	captured := cw.receiver.Captured()
	if len(captured) != 1 {
		t.Fatalf("captured %d; want 1", len(captured))
	}
	if !captured[0].Verified {
		t.Error("HMAC must verify")
	}
	ok, _, _ := cw.Verify(context.Background(), "INV-42")
	if !ok {
		t.Error("Verify after capture should return ok")
	}
}

func TestCustomWebhook_BadSignature(t *testing.T) {
	cw := NewCustomWebhook()
	defer cw.Close()
	body := []byte(`{"invoice_id":"INV-bad"}`)
	req, _ := http.NewRequest(http.MethodPost, cw.URL(), strings.NewReader(string(body)))
	req.Header.Set("X-Aforo-Signature", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("expected 401 on bad sig; got %d", resp.StatusCode)
	}
	captured := cw.receiver.Captured()
	if len(captured) != 1 {
		t.Fatalf("captured %d; want 1 (rejected payloads still captured for forensics)", len(captured))
	}
	if captured[0].Verified {
		t.Error("bad sig must NOT verify")
	}
}

func TestCustomWebhook_AssertReceived(t *testing.T) {
	cw := NewCustomWebhook()
	defer cw.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		body := []byte(`{"invoice_id":"INV-async"}`)
		req, _ := http.NewRequest(http.MethodPost, cw.URL(), strings.NewReader(string(body)))
		req.Header.Set("X-Aforo-Signature", SignBody(body, cw.Secret()))
		http.DefaultClient.Do(req) //nolint:errcheck
	}()
	if err := cw.AssertReceived(context.Background(), "INV-async", 5*time.Second); err != nil {
		t.Errorf("expected receive: %v", err)
	}
}

func TestSyncLog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	sl, err := NewSyncLog(dir + "/erp_sync.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rec := SyncRecord{
		InvoiceID: "i1", TenantID: "t1", Provider: "quickbooks", Status: "synced",
		ExternalID: "qbo_doc_1", Verified: true, LatencySeconds: 1.5,
	}
	if err := sl.Append(rec); err != nil {
		t.Fatal(err)
	}
	sl.Close()
	got, err := LoadSyncLog(dir)
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v / count %d", err, len(got))
	}
	if got[0].Verified != true || got[0].ExternalID != "qbo_doc_1" {
		t.Fatalf("loaded mismatch: %+v", got[0])
	}
}

// jsonValid prevents a "imported but unused" warning when test JSON lib
// changes shape. Compile-only.
var _ = json.Valid
