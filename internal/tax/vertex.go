package tax

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// VertexEngine wraps the Vertex O Series Tax Calculation REST API.
//
// Auth: bearer token (VERTEX_OAUTH_TOKEN). When unset, falls back to mock,
// just like AvalaraEngine.
//
// Env vars (read at construction):
//
//	VERTEX_OAUTH_TOKEN — bearer token from Vertex OAuth flow
//	VERTEX_BASE_URL    — e.g. https://restconnect.vertexsmb.com
//	VERTEX_TRUST_ID    — required by Vertex; "default" if unset
//
// Real Vertex setup is significantly heavier than this stub (cert-based
// auth, OAuth flow, regulated cert renewal). For load-test purposes the
// HTTP shim is sufficient — the real API integration lives in the platform
// itself; this engine merely VALIDATES that the platform's response matches
// what an independent call would have produced.
type VertexEngine struct {
	mock       *MockEngine
	enabled    bool
	baseURL    string
	trustID    string
	authHeader string
	httpClient *http.Client
	note       string
}

// NewVertexEngine constructs the engine with optional fallback to mock.
func NewVertexEngine(t scenario.Tax) *VertexEngine {
	mock := NewMockEngine(t)
	token := strings.TrimSpace(os.Getenv("VERTEX_OAUTH_TOKEN"))
	if token == "" {
		return &VertexEngine{
			mock:    mock,
			enabled: false,
			note:    "VERTEX_OAUTH_TOKEN missing — falling back to mock",
		}
	}
	baseURL := strings.TrimRight(os.Getenv("VERTEX_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "https://restconnect.vertexsmb.com"
	}
	trustID := os.Getenv("VERTEX_TRUST_ID")
	if trustID == "" {
		trustID = "default"
	}
	return &VertexEngine{
		mock:       mock,
		enabled:    true,
		baseURL:    baseURL,
		trustID:    trustID,
		authHeader: "Bearer " + token,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name returns "vertex" or "vertex-mock" so dashboards can distinguish.
func (vx *VertexEngine) Name() string {
	if !vx.enabled {
		return "vertex-mock"
	}
	return "vertex"
}

// Calculate posts a quotation request to /vertex-restapi/v1/calculation/quote
// when wired; otherwise delegates to the mock and stamps the fallback note.
func (vx *VertexEngine) Calculate(ctx context.Context, req Request) (Response, error) {
	if err := validateRequest(req); err != nil {
		return Response{}, err
	}
	if !vx.enabled {
		resp, err := vx.mock.Calculate(ctx, req)
		if err != nil {
			return resp, err
		}
		resp.Engine = vx.Name()
		if resp.Note == "" {
			resp.Note = vx.note
		}
		return resp, nil
	}
	return vx.callAPI(ctx, req)
}

func (vx *VertexEngine) callAPI(ctx context.Context, req Request) (Response, error) {
	endpoint := vx.baseURL + "/vertex-restapi/v1/calculation/quote"
	payload := map[string]any{
		"trustedId":       vx.trustID,
		"transactionType": "SALE",
		"documentNumber":  req.InvoiceID,
		"customerCode":    req.CustomerID,
		"currency":        orDefault(req.Currency, "USD"),
		"lineItems": []map[string]any{{
			"lineItemNumber": 1,
			"productClass":   orDefault(req.ProductType, "DIGITAL_SERVICE"),
			"unitPrice":      req.SubtotalUSD,
			"quantity":       1,
		}},
	}
	if req.Jurisdiction != "" {
		payload["destination"] = map[string]any{"region": req.Jurisdiction}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return vx.fallback(ctx, req, fmt.Sprintf("marshal: %v", err))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return vx.fallback(ctx, req, fmt.Sprintf("request: %v", err))
	}
	httpReq.Header.Set("Authorization", vx.authHeader)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := vx.httpClient.Do(httpReq)
	if err != nil {
		return vx.fallback(ctx, req, fmt.Sprintf("transport: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return vx.fallback(ctx, req, fmt.Sprintf("vertex %d", resp.StatusCode))
	}
	var parsed vertexResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return vx.fallback(ctx, req, fmt.Sprintf("decode: %v", err))
	}
	jurisdiction := req.Jurisdiction
	if len(parsed.LineItems) > 0 && len(parsed.LineItems[0].TaxDetails) > 0 {
		jurisdiction = parsed.LineItems[0].TaxDetails[0].JurisdictionCode
	}
	rate := 0.0
	if req.SubtotalUSD > 0 {
		rate = parsed.TotalTax / req.SubtotalUSD
	}
	return Response{
		JurisdictionCode: jurisdiction,
		Rate:             rate,
		TaxAmountUSD:     MultiplyAndRound(req.SubtotalUSD, rate),
		Engine:           vx.Name(),
	}, nil
}

func (vx *VertexEngine) fallback(ctx context.Context, req Request, reason string) (Response, error) {
	r, err := vx.mock.Calculate(ctx, req)
	if err != nil {
		return r, err
	}
	r.Engine = "vertex-fallback"
	if r.Note == "" {
		r.Note = "vertex: " + reason + " — used mock"
	}
	return r, nil
}

type vertexResponse struct {
	TotalTax  float64      `json:"totalTax"`
	LineItems []vertexLine `json:"lineItems"`
}

type vertexLine struct {
	TaxDetails []vertexJur `json:"taxDetails"`
}

type vertexJur struct {
	JurisdictionCode string `json:"jurisdictionCode"`
}
