package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// HTTPBaseConfig is the shared knob set every HTTP-backed driver consumes.
// Concrete drivers (rest_direct, sdk_*, gateway_*, webhook, csv_upload) embed
// this so connection pooling, redirect handling, and timeouts are uniform.
type HTTPBaseConfig struct {
	Target              aforo.Target
	HTTPClient          *http.Client
	RequestTimeout      time.Duration
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	AdminToken          string
}

// applyDefaults backfills the zero-valued knobs.
func (c *HTTPBaseConfig) applyDefaults() {
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = 100
	}
	if c.IdleConnTimeout <= 0 {
		c.IdleConnTimeout = 90 * time.Second
	}
	if c.HTTPClient == nil {
		transport := &http.Transport{
			MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
			MaxIdleConns:          c.MaxIdleConnsPerHost * 4,
			IdleConnTimeout:       c.IdleConnTimeout,
			ResponseHeaderTimeout: c.RequestTimeout,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
		}
		c.HTTPClient = &http.Client{
			Timeout:   c.RequestTimeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
}

// closeIdle releases pooled connections from the embedded transport.
// All HTTP drivers share this in their Close().
func closeIdle(c *http.Client) {
	if c == nil {
		return
	}
	if t, ok := c.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// marshalEvent serializes the generator's Envelope to JSON, or returns the
// raw body when the negative-path injector pre-corrupted it (malformed).
//
// Returned (bodyBytes, isJSON, err).
func marshalEvent(e *generator.Event) ([]byte, bool, error) {
	if len(e.RawBody) > 0 {
		return e.RawBody, false, nil
	}
	b, err := json.Marshal(e.Envelope)
	if err != nil {
		return nil, false, fmt.Errorf("marshal envelope: %w", err)
	}
	return b, true, nil
}

// applyAuthHeaders sets the credential headers consistently across drivers.
// Per-event Auth.Token is used when set, otherwise the AdminToken fallback.
func applyAuthHeaders(req *http.Request, e *generator.Event, adminToken string) {
	if e.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
	} else if adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	if e.Auth.ClientID != "" {
		req.Header.Set("X-Client-Id", e.Auth.ClientID)
	}
}

// applyTenantHeaders sets the tenant identity headers used by the platform's
// TenantFilter and per-tenant rate limiter.
func applyTenantHeaders(req *http.Request, e *generator.Event) {
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Envelope.CustomerID != "" {
		req.Header.Set("X-Customer-Id", e.Envelope.CustomerID)
	}
}

// applyTraceHeaders sets the loadgen correlation headers so server logs can
// be cross-referenced with run.json post-run.
func applyTraceHeaders(req *http.Request, e *generator.Event) {
	req.Header.Set("X-Loadgen-Event-Id", e.EventID)
	if e.NegativePath != "" {
		req.Header.Set("X-Loadgen-Negative-Path", string(e.NegativePath))
	}
}

// doHTTPRequest sends the request, drains a small response prefix for byte
// accounting, and returns the populated Result. A timing-out or aborted
// request lands as TransportErr (Status==0).
func doHTTPRequest(client *http.Client, req *http.Request, e *generator.Event, bodyLen int) Result {
	res := Result{Event: e, BytesSent: bodyLen}
	startedAt := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		res.TransportErr = err
		res.Latency = time.Since(startedAt)
		return res
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	const maxRead = 4 << 10
	limited := io.LimitReader(resp.Body, maxRead)
	body, _ := io.ReadAll(limited)
	res.BytesRecv = len(body)
	res.Latency = time.Since(startedAt)
	return res
}

// buildJSONIngestRequest constructs a POST to the resolved URL with the
// event body marshaled as JSON, applying tenant + auth + trace headers.
// Used by every JSON-bodied driver (rest_direct, sdk_*, gateway_*).
//
// Caller may pass extraHeaders to layer driver-specific identification on
// top (e.g. X-Forwarded-By for gateway drivers, User-Agent for SDKs).
func buildJSONIngestRequest(ctx context.Context, url string, e *generator.Event, adminToken string, extraHeaders map[string]string) (*http.Request, []byte, error) {
	bodyBytes, _, err := marshalEvent(e)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyAuthHeaders(req, e, adminToken)
	applyTenantHeaders(req, e)
	applyTraceHeaders(req, e)
	for k, v := range extraHeaders {
		// Caller wins — driver identity headers like User-Agent should stick.
		req.Header.Set(k, v)
	}
	return req, bodyBytes, nil
}
