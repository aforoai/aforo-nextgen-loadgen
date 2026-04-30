package tax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// AvalaraEngine wraps the AvaTax v2 REST API.
//
// Auth: HTTP basic — `<account_id>:<license_key>` base64-encoded.
// Endpoint: https://sandbox-rest.avatax.com/api/v2/transactions/create
//
// Env vars (read at construction):
//
//	AVATAX_ACCOUNT_ID    — numeric account id
//	AVATAX_LICENSE_KEY   — license key
//	AVATAX_BASE_URL      — defaults to sandbox; set production URL when ready
//	AVATAX_COMPANY_CODE  — required by API; "DEFAULT" if unset
//
// When the account id or license key is missing, NewAvalaraEngine returns
// a fallback engine that delegates to MockEngine — load tests in CI without
// AvaTax sandbox creds still run; the Response.Note field carries the reason
// so the validator surfaces it cleanly.
type AvalaraEngine struct {
	mock        *MockEngine
	enabled     bool
	baseURL     string
	companyCode string
	httpClient  *http.Client
	authHeader  string
	note        string // populated when fallback fires
}

// NewAvalaraEngine constructs the engine. Reads env vars; never fails — falls
// back to the mock engine when creds are absent. Honor the
// `note` on every response so dashboards can tell which engine actually ran.
func NewAvalaraEngine(t scenario.Tax) *AvalaraEngine {
	mock := NewMockEngine(t)
	accountID := strings.TrimSpace(os.Getenv("AVATAX_ACCOUNT_ID"))
	licenseKey := strings.TrimSpace(os.Getenv("AVATAX_LICENSE_KEY"))
	if accountID == "" || licenseKey == "" {
		return &AvalaraEngine{
			mock:    mock,
			enabled: false,
			note:    "AVATAX_ACCOUNT_ID/LICENSE_KEY missing — falling back to mock",
		}
	}
	baseURL := strings.TrimRight(os.Getenv("AVATAX_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "https://sandbox-rest.avatax.com"
	}
	companyCode := os.Getenv("AVATAX_COMPANY_CODE")
	if companyCode == "" {
		companyCode = "DEFAULT"
	}
	cred := accountID + ":" + licenseKey
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	return &AvalaraEngine{
		mock:        mock,
		enabled:     true,
		baseURL:     baseURL,
		companyCode: companyCode,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		authHeader:  auth,
	}
}

// Name returns "avalara" when the engine is wired to the API; "avalara-mock"
// when it's running the fallback. Distinct names so dashboards can count the
// fallback rate.
func (a *AvalaraEngine) Name() string {
	if !a.enabled {
		return "avalara-mock"
	}
	return "avalara"
}

// Calculate posts a CreateTransactionModel to AvaTax /transactions/create
// when enabled; otherwise delegates to the mock engine and stamps the
// fallback reason on Response.Note.
func (a *AvalaraEngine) Calculate(ctx context.Context, req Request) (Response, error) {
	if err := validateRequest(req); err != nil {
		return Response{}, err
	}
	if !a.enabled {
		resp, err := a.mock.Calculate(ctx, req)
		if err != nil {
			return resp, err
		}
		resp.Engine = a.Name()
		if resp.Note == "" {
			resp.Note = a.note
		}
		return resp, nil
	}
	return a.callAPI(ctx, req)
}

// callAPI is the live-call path. POSTs the canonical AvaTax v2 transaction
// shape and decodes the totalTax + jurisdictionCode out of the response.
//
// On any transport / 5xx / parse error, falls back to mock so a flaky
// sandbox doesn't fail the load run. The fallback path stamps the error on
// Response.Note for observability.
func (a *AvalaraEngine) callAPI(ctx context.Context, req Request) (Response, error) {
	endpoint := a.baseURL + "/api/v2/transactions/create"
	payload := map[string]any{
		"type":        "SalesInvoice",
		"companyCode": a.companyCode,
		"date":        time.Now().UTC().Format("2006-01-02"),
		"customerCode": req.CustomerID,
		"currencyCode": orDefault(req.Currency, "USD"),
		"lines": []map[string]any{{
			"number":      "1",
			"quantity":    1,
			"amount":      req.SubtotalUSD,
			"description": "loadgen-tax-probe",
			"itemCode":    orDefault(req.ProductType, "GENERIC"),
		}},
	}
	if req.Jurisdiction != "" {
		payload["addresses"] = map[string]any{
			"singleLocation": map[string]any{"region": req.Jurisdiction},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return a.fallback(ctx, req, fmt.Sprintf("marshal: %v", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return a.fallback(ctx, req, fmt.Sprintf("request: %v", err))
	}
	httpReq.Header.Set("Authorization", a.authHeader)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Avalara-Client", "aforo-loadgen")
	httpReq.URL.RawQuery = url.Values{"$include": []string{"Lines,TaxDetails"}}.Encode()

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return a.fallback(ctx, req, fmt.Sprintf("transport: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return a.fallback(ctx, req, fmt.Sprintf("avatax %d", resp.StatusCode))
	}
	var parsed avataxTransaction
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return a.fallback(ctx, req, fmt.Sprintf("decode: %v", err))
	}
	jurisdiction := req.Jurisdiction
	if len(parsed.Lines) > 0 && len(parsed.Lines[0].Details) > 0 {
		jurisdiction = parsed.Lines[0].Details[0].JurisCode
	}
	rate := 0.0
	if req.SubtotalUSD > 0 {
		rate = parsed.TotalTax / req.SubtotalUSD
	}
	return Response{
		JurisdictionCode: jurisdiction,
		Rate:             rate,
		TaxAmountUSD:     MultiplyAndRound(req.SubtotalUSD, rate),
		Engine:           a.Name(),
	}, nil
}

// fallback returns the mock engine's response with a note explaining why.
func (a *AvalaraEngine) fallback(ctx context.Context, req Request, reason string) (Response, error) {
	r, err := a.mock.Calculate(ctx, req)
	if err != nil {
		return r, err
	}
	r.Engine = "avalara-fallback"
	if r.Note == "" {
		r.Note = "avalara: " + reason + " — used mock"
	}
	return r, nil
}

// avataxTransaction is the subset of AvaTax v2 CreateTransactionResponse we
// care about. Per AvaTax docs: each line's Details array carries the
// jurisdiction codes that contributed to the tax.
type avataxTransaction struct {
	TotalTax float64        `json:"totalTax"`
	Lines    []avataxLine   `json:"lines"`
}

type avataxLine struct {
	Details []avataxDetail `json:"details"`
}

type avataxDetail struct {
	JurisCode string `json:"jurisCode"`
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
