package erp

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// NetSuite wraps NetSuite SuiteTalk REST.
//
// Auth: Token-Based Authentication (TBA) is the standard production scheme.
// For load tests, a long-lived REST token (NETSUITE_REST_TOKEN) is enough;
// proper OAuth 1.0a + HMAC signing isn't reimplemented here. When the
// token is missing, the shim runs in shadow mode.
type NetSuite struct {
	commonClient
}

// NewNetSuite reads env vars; shadows when absent.
func NewNetSuite() *NetSuite {
	n := &NetSuite{
		commonClient: commonClient{httpClient: &http.Client{Timeout: 10 * time.Second}},
	}
	token := strings.TrimSpace(os.Getenv("NETSUITE_REST_TOKEN"))
	accountID := strings.TrimSpace(os.Getenv("NETSUITE_ACCOUNT_ID"))
	if token == "" || accountID == "" {
		n.note = "NETSUITE_REST_TOKEN/NETSUITE_ACCOUNT_ID missing — shadow mode"
		return n
	}
	n.enabled = true
	// Format: https://{accountId}.suitetalk.api.netsuite.com.
	n.baseURL = "https://" + accountID + ".suitetalk.api.netsuite.com"
	n.authHeader = "Bearer " + token
	return n
}

// Name returns "netsuite".
func (n *NetSuite) Name() string { return "netsuite" }

// IsLive reports live vs shadow.
func (n *NetSuite) IsLive() bool { return n.enabled }

// Verify GETs /services/rest/record/v1/invoice/{id}.
func (n *NetSuite) Verify(ctx context.Context, externalID string) (bool, string, error) {
	if !n.enabled {
		return true, "shadow-mode", nil
	}
	resp, err := n.get(ctx, "/services/rest/record/v1/invoice/"+externalID)
	if err != nil {
		return false, "transport: " + err.Error(), err
	}
	defer resp.Body.Close()
	if notFound(resp) {
		return false, "NetSuite 404 — invoice record not found", nil
	}
	if !statusOK(resp) {
		return false, "NetSuite " + resp.Status, nil
	}
	return true, "", nil
}
