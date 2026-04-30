package erp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// Xero wraps the Xero Accounting API.
//
// Auth: OAuth 2.0 — XERO_ACCESS_TOKEN + XERO_TENANT_ID (Xero requires a
// tenant header on every call).
type Xero struct {
	commonClient
	tenantID string
}

// NewXero reads env vars; shadows when absent.
func NewXero() *Xero {
	x := &Xero{
		commonClient: commonClient{httpClient: &http.Client{Timeout: 10 * time.Second}},
	}
	token := strings.TrimSpace(os.Getenv("XERO_ACCESS_TOKEN"))
	tenantID := strings.TrimSpace(os.Getenv("XERO_TENANT_ID"))
	if token == "" || tenantID == "" {
		x.note = "XERO_ACCESS_TOKEN/XERO_TENANT_ID missing — shadow mode"
		return x
	}
	x.enabled = true
	x.baseURL = strings.TrimRight(orDefault(os.Getenv("XERO_BASE_URL"), "https://api.xero.com"), "/")
	x.authHeader = "Bearer " + token
	x.tenantID = tenantID
	return x
}

// Name returns "xero".
func (x *Xero) Name() string { return "xero" }

// IsLive reports live vs shadow.
func (x *Xero) IsLive() bool { return x.enabled }

// Verify GETs /api.xro/2.0/Invoices/{guid}. 404 → missing.
func (x *Xero) Verify(ctx context.Context, externalID string) (bool, string, error) {
	if !x.enabled {
		return true, "shadow-mode", nil
	}
	endpoint := x.baseURL + "/api.xro/2.0/Invoices/" + externalID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "request: " + err.Error(), err
	}
	req.Header.Set("Authorization", x.authHeader)
	req.Header.Set("Xero-Tenant-Id", x.tenantID)
	req.Header.Set("Accept", "application/json")
	resp, err := x.httpClient.Do(req)
	if err != nil {
		return false, "transport: " + err.Error(), err
	}
	defer resp.Body.Close()
	if notFound(resp) {
		return false, "Xero 404 — invoice not found", nil
	}
	if !statusOK(resp) {
		return false, "Xero " + resp.Status, nil
	}
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return false, "decode: " + err.Error(), err
	}
	if _, ok := parsed["Invoices"]; !ok {
		return false, "Xero body missing Invoices array", nil
	}
	return true, "", nil
}
