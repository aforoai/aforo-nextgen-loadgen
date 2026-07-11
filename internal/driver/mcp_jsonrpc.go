package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// MCPJsonRPCEnvURL is the environment variable that points the driver at
// the target MCP endpoint. Populated by docker-compose in the regression
// stack (aforo-nextgen-docker/mcp-test-stack.yml), or by the operator on
// the command line for local runs:
//
//	AFORO_LOADGEN_MCP_URL=http://mcp-test-server:8080/mcp
//
// Empty at driver-construct time means the scenario references
// mcp_jsonrpc but the operator forgot to wire the URL — NewMCPJsonRPC
// returns an error so the run fails loudly at startup instead of
// silently 404-ing every event.
const MCPJsonRPCEnvURL = "AFORO_LOADGEN_MCP_URL"

// MCPJsonRPC emits real JSON-RPC 2.0 tools/call payloads at a configurable
// MCP endpoint. Closes the gateway-plugin-detection gap in loadgen: the
// scenario's mcpServerTemplate metadata fields (tool_name, agent_id,
// session_id) become tools/call params, which the aforo-metering plugin
// (Kong/Apigee/AWS/Azure/MuleSoft) can see and meter — unlike every other
// driver which posts to /v1/ingest directly.
//
// Typical wiring:
//
//	loadgen → Kong (with aforo-metering plugin) → mcp-test-server
//
// Kong's handler.lua detect_mcp_tool_call() picks the call out of the
// POST body, extracts tool_name + agent_id, and generates the metering
// event. loadgen never talks to /v1/ingest directly on this path.
//
// Non-MCP events (product_type != MCP_SERVER) are rejected with a
// transport-class error so the scenario writer notices the mis-pairing.
type MCPJsonRPC struct {
	url    string
	client *http.Client
	// idSeq is the monotonic JSON-RPC id counter for this driver instance.
	// Every submission gets a fresh id so response correlation is trivial
	// even under concurrency.
	idSeq atomic.Int64
}

// MCPJsonRPCConfig lets tests override construction defaults. Runtime code
// uses the env var + defaults.
type MCPJsonRPCConfig struct {
	// URL overrides the env-var-derived endpoint. Non-empty wins.
	URL string
	// HTTPClient overrides the default; useful for tests to inject a
	// round-tripper. Runtime uses a stock http.Client with a 15s timeout.
	HTTPClient *http.Client
	// RequestTimeout applies only when HTTPClient is not supplied.
	RequestTimeout time.Duration
}

// NewMCPJsonRPC constructs the driver. Returns an error when the target
// URL is empty AND the env var is unset — loud failure beats silent 404s.
func NewMCPJsonRPC(cfg MCPJsonRPCConfig) (*MCPJsonRPC, error) {
	url := cfg.URL
	if url == "" {
		url = os.Getenv(MCPJsonRPCEnvURL)
	}
	if url == "" {
		return nil, fmt.Errorf("mcp_jsonrpc: target URL missing — set %s or pass URL in config", MCPJsonRPCEnvURL)
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

	return &MCPJsonRPC{url: url, client: client}, nil
}

// Name reports the driver identifier — used by metrics labels and by the
// scenario.IngestionPaths key resolver.
func (d *MCPJsonRPC) Name() string { return "mcp_jsonrpc" }

// Submit builds a real JSON-RPC 2.0 tools/call payload from the event's
// metadata and POSTs it. Non-MCP events short-circuit with a transport
// error so the scenario writer catches the misconfiguration on the first
// batch.
func (d *MCPJsonRPC) Submit(ctx context.Context, e *generator.Event) Result {
	res := Result{Event: e}
	startedAt := time.Now()

	// Guard: this driver only speaks MCP. Non-MCP events would produce
	// nonsense JSON-RPC (no tool_name, no agent_id) that the toy server
	// rejects with -32602 InvalidParams. Better to fail here with a
	// specific message.
	productType := stringField(e, "productType", e.Envelope.MetricName)
	toolName := stringField(e, "tool_name", "")
	if toolName == "" {
		res.TransportErr = fmt.Errorf(
			"mcp_jsonrpc: event has no metadata.tool_name (product_type=%s) — driver only handles MCP_SERVER events",
			productType,
		)
		res.Latency = time.Since(startedAt)
		return res
	}

	rpcID := d.idSeq.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      rpcID,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": buildArguments(toolName),
			// _meta carries the metering hints every gateway plugin looks
			// for. Kong handler.lua reads params._meta.agent_id, Apigee JS
			// reads _meta.session_id, etc. Mirrors the shape emitted by
			// real MCP clients like Claude Desktop.
			"_meta": map[string]any{
				"agent_id":   stringField(e, "agent_id", ""),
				"session_id": stringField(e, "session_id", ""),
			},
		},
	})
	if err != nil {
		res.TransportErr = fmt.Errorf("mcp_jsonrpc: marshal request: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	res.BytesSent = len(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		res.TransportErr = fmt.Errorf("mcp_jsonrpc: build request: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Aforo headers — gateway plugins may still forward these through even
	// though the primary metering trigger is the tools/call body.
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Envelope.CustomerID != "" {
		req.Header.Set("X-Customer-Id", e.Envelope.CustomerID)
	}
	if e.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
	}
	if sessionID := stringField(e, "session_id", ""); sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if e.EventID != "" {
		req.Header.Set("X-Loadgen-Event-Id", e.EventID)
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

	// The MCP spec always returns 200 for a well-formed JSON-RPC response,
	// even when the RESULT is a JSON-RPC error object. So a 200 with an
	// "error" field in the body is still a business-level failure — but
	// classifying that as a client error here would flap the circuit
	// breaker on tests that deliberately exercise the -32601 branch. Leave
	// business-error classification to the caller (report writer sees the
	// body excerpt below).
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
func (d *MCPJsonRPC) Close() error {
	closeIdle(d.client)
	return nil
}

// stringField pulls a metadata value out of the event envelope, coercing
// numeric types to their string form and returning the fallback for
// absent / non-scalar values.
func stringField(e *generator.Event, key, fallback string) string {
	if e == nil || e.Envelope.Metadata == nil {
		return fallback
	}
	v, ok := e.Envelope.Metadata[key]
	if !ok {
		return fallback
	}
	switch typed := v.(type) {
	case string:
		if typed == "" {
			return fallback
		}
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return fallback
}

// buildArguments produces a minimal set of arguments the toy MCP server
// expects for the given tool. We use fixed strings rather than random
// data so scenarios remain deterministic across runs (the seed already
// controls tool_name selection).
func buildArguments(tool string) map[string]any {
	switch tool {
	case "search_web", "vector_search":
		return map[string]any{"query": "loadgen-synthetic"}
	case "read_file":
		return map[string]any{"path": "/tmp/loadgen.txt"}
	case "write_file":
		return map[string]any{"path": "/tmp/loadgen.txt", "contents": "hello"}
	case "execute_query":
		return map[string]any{"sql": "SELECT 1"}
	case "summarize", "classify":
		return map[string]any{"text": "loadgen synthetic payload"}
	case "translate":
		return map[string]any{"text": "hello", "to": "es"}
	case "send_email":
		return map[string]any{"to": "loadgen@example.com", "subject": "test", "body": "synthetic"}
	case "create_record":
		return map[string]any{"collection": "loadgen", "data": map[string]any{"k": "v"}}
	default:
		return map[string]any{}
	}
}

// canonicalMcpJsonRPCName is used by the registry + validator + generator
// weighting logic. Exported as a const so drift between call sites is
// caught at compile time.
const canonicalMcpJsonRPCName = "mcp_jsonrpc"

// Compile-time guard — Name() must return the canonical string.
var _ = func() bool {
	if !strings.EqualFold((&MCPJsonRPC{}).Name(), canonicalMcpJsonRPCName) {
		panic("mcp_jsonrpc: Name() diverged from canonicalMcpJsonRPCName")
	}
	return true
}()
