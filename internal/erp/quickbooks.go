package erp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// QuickBooks wraps the QuickBooks Online v3 sandbox.
//
// Auth: OAuth 2.0 — caller supplies an access token via QBO_ACCESS_TOKEN.
// Realm: QBO_COMPANY_ID. Endpoint: https://sandbox-quickbooks.api.intuit.com.
//
// The platform's QuickBooksAdapter performs invoice creation; this shim
// looks up the invoice by docNumber to confirm it landed.
type QuickBooks struct {
	commonClient
	companyID string
}

// NewQuickBooks reads env vars; falls back to shadow mode when absent.
func NewQuickBooks() *QuickBooks {
	q := &QuickBooks{
		commonClient: commonClient{httpClient: &http.Client{Timeout: 10 * time.Second}},
	}
	token := strings.TrimSpace(os.Getenv("QBO_ACCESS_TOKEN"))
	companyID := strings.TrimSpace(os.Getenv("QBO_COMPANY_ID"))
	if token == "" || companyID == "" {
		q.note = "QBO_ACCESS_TOKEN/QBO_COMPANY_ID missing — shadow mode"
		return q
	}
	q.enabled = true
	q.baseURL = strings.TrimRight(orDefault(os.Getenv("QBO_BASE_URL"), "https://sandbox-quickbooks.api.intuit.com"), "/")
	q.authHeader = "Bearer " + token
	q.companyID = companyID
	return q
}

// Name returns "quickbooks".
func (q *QuickBooks) Name() string { return "quickbooks" }

// IsLive reports live vs shadow.
func (q *QuickBooks) IsLive() bool { return q.enabled }

// Verify queries /v3/company/{cid}/query?query=SELECT * FROM Invoice WHERE DocNumber='X'.
// 404 → not found; 200 with empty result set → not found.
func (q *QuickBooks) Verify(ctx context.Context, externalID string) (bool, string, error) {
	if !q.enabled {
		return true, "shadow-mode", nil
	}
	path := "/v3/company/" + q.companyID + "/invoice/" + externalID
	resp, err := q.get(ctx, path)
	if err != nil {
		return false, "transport: " + err.Error(), err
	}
	defer resp.Body.Close()
	if notFound(resp) {
		return false, "QBO 404 — invoice doc not found", nil
	}
	if !statusOK(resp) {
		return false, "QBO " + resp.Status, nil
	}
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return false, "decode: " + err.Error(), err
	}
	if _, ok := parsed["Invoice"]; !ok {
		return false, "QBO body missing Invoice element", nil
	}
	return true, "", nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
