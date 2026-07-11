package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// AIAgentRESTEnvURL is the environment variable that points the driver at
// the target ingest endpoint. Populated by docker-compose in the regression
// stack, or by the operator on the command line for local runs:
//
//	AFORO_LOADGEN_INGEST_URL=http://usage-ingestor:8084/v1/ingest
//
// Empty at driver-construct time means the scenario references
// ai_agent_rest but the operator forgot to wire the URL —
// NewAIAgentREST returns an error so the run fails loudly at startup
// instead of silently 404-ing every event.
//
// Named "AFORO_LOADGEN_INGEST_URL" (not "AFORO_LOADGEN_AI_AGENT_URL")
// because this driver POSTs the standard ingest envelope — the wire
// contract is IngestUsageEventRequest, same as rest_direct. The driver
// exists as a separate ingestion path so a scenario can point AI_AGENT
// traffic at a distinct URL (e.g. a staging usage-ingestor probing the
// descriptor-driven per-capability path) without disturbing every other
// path's target.
const AIAgentRESTEnvURL = "AFORO_LOADGEN_INGEST_URL"

// AIAgentREST POSTs AI_AGENT usage events to a configurable ingest endpoint
// so scenarios exercise the descriptor-driven per-capability path end-to-end.
// The wire envelope matches usage-ingestor's IngestUsageEventRequest, with
// capability_name / agent_id / session_id / execution_status /
// execution_duration_ms carried on metadata (matching the extractor contract
// at ProductTypeEventExtractor.extractAiAgentFields).
//
// Non-AI_AGENT events (envelope.ProductType != "AI_AGENT") are rejected with
// a transport-class error so a scenario writer catches a mis-paired
// product_mix + ingestion_paths on the first batch (mirrors mcp_jsonrpc's
// non-MCP rejection).
//
// Typical wiring:
//
//	loadgen (ai_agent_rest driver) → usage-ingestor /v1/ingest
type AIAgentREST struct {
	url    string
	client *http.Client
}

// AIAgentRESTConfig lets tests override construction defaults. Runtime code
// uses the env var + defaults.
type AIAgentRESTConfig struct {
	// URL overrides the env-var-derived endpoint. Non-empty wins.
	URL string
	// HTTPClient overrides the default; useful for tests to inject a
	// round-tripper. Runtime uses a stock http.Client with a 15s timeout.
	HTTPClient *http.Client
	// RequestTimeout applies only when HTTPClient is not supplied.
	RequestTimeout time.Duration
}

// NewAIAgentREST constructs the driver. Returns an error when the target
// URL is empty AND the env var is unset — loud failure beats silent 404s.
func NewAIAgentREST(cfg AIAgentRESTConfig) (*AIAgentREST, error) {
	url := cfg.URL
	if url == "" {
		url = os.Getenv(AIAgentRESTEnvURL)
	}
	if url == "" {
		return nil, fmt.Errorf("ai_agent_rest: target URL missing — set %s or pass URL in config", AIAgentRESTEnvURL)
	}

	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.RequestTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   100,
				MaxIdleConns:          400,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: timeout,
				ForceAttemptHTTP2:     true,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return &AIAgentREST{url: url, client: client}, nil
}

// Name reports the driver identifier — used by metrics labels and by the
// scenario.IngestionPaths key resolver.
func (d *AIAgentREST) Name() string { return "ai_agent_rest" }

// Submit serializes the event as a standard IngestUsageEventRequest and
// POSTs it. Non-AI_AGENT events short-circuit with a transport error so
// the scenario writer catches the misconfiguration on the first batch.
func (d *AIAgentREST) Submit(ctx context.Context, e *generator.Event) Result {
	res := Result{Event: e}
	startedAt := time.Now()

	// Guard: this driver only speaks AI_AGENT. Non-AI_AGENT events would land
	// on the ingest endpoint under the wrong product-type contract and either
	// route to the wrong descriptor path or 4xx on shape mismatch. Better to
	// fail here with a specific message than to let the scenario writer chase
	// a downstream mystery.
	if e == nil {
		res.TransportErr = fmt.Errorf("ai_agent_rest: nil event")
		res.Latency = time.Since(startedAt)
		return res
	}
	productType := e.Envelope.ProductType
	if !strings.EqualFold(productType, "AI_AGENT") {
		res.TransportErr = fmt.Errorf(
			"ai_agent_rest: event has product_type=%q — driver only handles AI_AGENT events",
			productType,
		)
		res.Latency = time.Since(startedAt)
		return res
	}

	// Marshal the standard envelope. Because this driver's contract is the
	// same IngestUsageEventRequest that rest_direct posts, we serialize the
	// envelope directly. capability_name / agent_id / session_id /
	// execution_status / execution_duration_ms all ride on Envelope.Metadata
	// where extractAiAgentFields expects to find them.
	body, err := json.Marshal(e.Envelope)
	if err != nil {
		res.TransportErr = fmt.Errorf("ai_agent_rest: marshal envelope: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	res.BytesSent = len(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		res.TransportErr = fmt.Errorf("ai_agent_rest: build request: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
	}
	// TenantID / CustomerID / EventID all flow via headers (mirrors
	// rest_direct) so the wire body matches the backend's DTO exactly and
	// TenantFilter can extract the tenant scope before validation.
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Envelope.CustomerID != "" {
		req.Header.Set("X-Customer-Id", e.Envelope.CustomerID)
	}
	if e.Auth.ClientID != "" {
		req.Header.Set("X-Client-Id", e.Auth.ClientID)
	}
	if e.EventID != "" {
		req.Header.Set("X-Loadgen-Event-Id", e.EventID)
	}
	if sessionID := stringField(e, "session_id", ""); sessionID != "" {
		// Not part of the ingest DTO — kept as an operator convenience so
		// downstream logs can correlate a synthetic AI_AGENT session with
		// its generated events. Mirrors mcp_jsonrpc's Mcp-Session-Id header.
		req.Header.Set("X-Aforo-Session-Id", sessionID)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		res.TransportErr = err
		res.Latency = time.Since(startedAt)
		return res
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	const maxRead = 4 << 10
	limited := io.LimitReader(resp.Body, maxRead)
	respBody, _ := io.ReadAll(limited)
	res.BytesRecv = len(respBody)
	res.Latency = time.Since(startedAt)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(respBody) > BodyExcerptMax {
			res.BodyExcerpt = string(respBody[:BodyExcerptMax]) + "…"
		} else {
			res.BodyExcerpt = string(respBody)
		}
	}
	return res
}

// Close releases idle connections.
func (d *AIAgentREST) Close() error {
	closeIdle(d.client)
	return nil
}

// canonicalAIAgentRESTName is used by the registry + validator + generator
// weighting logic. Exported as a const so drift between call sites is
// caught at compile time.
const canonicalAIAgentRESTName = "ai_agent_rest"

// Compile-time guard — Name() must return the canonical string.
var _ = func() bool {
	if !strings.EqualFold((&AIAgentREST{}).Name(), canonicalAIAgentRESTName) {
		panic("ai_agent_rest: Name() diverged from canonicalAIAgentRESTName")
	}
	return true
}()
