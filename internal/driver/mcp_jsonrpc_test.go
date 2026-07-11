package driver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// Helper: build a canonical MCP event with the metadata fields the driver reads.
func mcpEvent(tool, agent, session string) *generator.Event {
	return &generator.Event{
		TenantID: "tenant_test",
		EventID:  "evt_1",
		Envelope: generator.Envelope{
			CustomerID: "cust_1",
			MetricName: "MCP_SERVER",
			Metadata: map[string]any{
				"tool_name":  tool,
				"agent_id":   agent,
				"session_id": session,
			},
		},
	}
}

func TestMCPJsonRPC_EmitsWellFormedToolsCall(t *testing.T) {
	var captured struct {
		method       string
		body         []byte
		tenantHeader string
		sessionHdr   string
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.tenantHeader = r.Header.Get("X-Tenant-Id")
		captured.sessionHdr = r.Header.Get("Mcp-Session-Id")
		b, _ := io.ReadAll(r.Body)
		captured.body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer ts.Close()

	d, err := NewMCPJsonRPC(MCPJsonRPCConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewMCPJsonRPC: %v", err)
	}
	defer d.Close()

	res := d.Submit(context.Background(), mcpEvent("search_web", "agent-42", "sess-99"))
	if res.Status != 200 {
		t.Fatalf("expected 200, got %d (body=%q, transportErr=%v)", res.Status, res.BodyExcerpt, res.TransportErr)
	}
	if res.TransportErr != nil {
		t.Fatalf("unexpected transport err: %v", res.TransportErr)
	}

	if captured.method != "POST" {
		t.Errorf("expected POST, got %s", captured.method)
	}
	if captured.tenantHeader != "tenant_test" {
		t.Errorf("X-Tenant-Id: expected tenant_test, got %q", captured.tenantHeader)
	}
	if captured.sessionHdr != "sess-99" {
		t.Errorf("Mcp-Session-Id: expected sess-99, got %q", captured.sessionHdr)
	}

	var envelope map[string]any
	if err := json.Unmarshal(captured.body, &envelope); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, captured.body)
	}
	if envelope["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc: expected 2.0, got %v", envelope["jsonrpc"])
	}
	if envelope["method"] != "tools/call" {
		t.Errorf("method: expected tools/call, got %v", envelope["method"])
	}
	params, ok := envelope["params"].(map[string]any)
	if !ok {
		t.Fatalf("params: expected object, got %T", envelope["params"])
	}
	if params["name"] != "search_web" {
		t.Errorf("params.name: expected search_web, got %v", params["name"])
	}
	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params._meta: expected object, got %T", params["_meta"])
	}
	if meta["agent_id"] != "agent-42" {
		t.Errorf("params._meta.agent_id: expected agent-42, got %v", meta["agent_id"])
	}
	if meta["session_id"] != "sess-99" {
		t.Errorf("params._meta.session_id: expected sess-99, got %v", meta["session_id"])
	}
}

func TestMCPJsonRPC_RejectsEventWithoutToolName(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	d, err := NewMCPJsonRPC(MCPJsonRPCConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewMCPJsonRPC: %v", err)
	}
	defer d.Close()

	// API event — no tool_name in metadata.
	e := &generator.Event{
		TenantID: "tenant_test",
		Envelope: generator.Envelope{
			CustomerID: "cust_1",
			MetricName: "API",
			Metadata:   map[string]any{"endpoint": "/api/v1/x"},
		},
	}
	res := d.Submit(context.Background(), e)
	if res.TransportErr == nil {
		t.Fatalf("expected transport err for non-MCP event, got status=%d", res.Status)
	}
	if !strings.Contains(res.TransportErr.Error(), "tool_name") {
		t.Errorf("error message should mention tool_name; got: %v", res.TransportErr)
	}
}

func TestMCPJsonRPC_MissingURLReturnsError(t *testing.T) {
	// Ensure the env var is not set for this test — the CI environment
	// shouldn't have it, but tests should be robust regardless.
	t.Setenv(MCPJsonRPCEnvURL, "")
	_, err := NewMCPJsonRPC(MCPJsonRPCConfig{})
	if err == nil {
		t.Fatal("expected error when URL is empty and env var unset")
	}
	if !strings.Contains(err.Error(), MCPJsonRPCEnvURL) {
		t.Errorf("error should name the env var %q; got: %v", MCPJsonRPCEnvURL, err)
	}
}

func TestMCPJsonRPC_UsesEnvVarWhenConfigURLEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer ts.Close()

	t.Setenv(MCPJsonRPCEnvURL, ts.URL)
	d, err := NewMCPJsonRPC(MCPJsonRPCConfig{})
	if err != nil {
		t.Fatalf("NewMCPJsonRPC: %v", err)
	}
	defer d.Close()

	res := d.Submit(context.Background(), mcpEvent("read_file", "agent-1", "sess-1"))
	if res.Status != 200 {
		t.Fatalf("expected 200, got %d", res.Status)
	}
}

func TestMCPJsonRPC_EachSubmitGetsFreshJSONRPCId(t *testing.T) {
	var ids []float64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &envelope)
		if id, ok := envelope["id"].(float64); ok {
			ids = append(ids, id)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer ts.Close()

	d, err := NewMCPJsonRPC(MCPJsonRPCConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewMCPJsonRPC: %v", err)
	}
	defer d.Close()

	for i := 0; i < 3; i++ {
		_ = d.Submit(context.Background(), mcpEvent("search_web", "a", "s"))
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 captured ids, got %d", len(ids))
	}
	seen := map[float64]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate JSON-RPC id: %v", id)
		}
		seen[id] = true
	}
}

func TestMCPJsonRPC_RegisteredInAllNames(t *testing.T) {
	found := false
	for _, name := range AllNames() {
		if name == "mcp_jsonrpc" {
			found = true
			break
		}
	}
	if !found {
		t.Error("mcp_jsonrpc missing from driver.AllNames() — validator will reject scenarios that use it")
	}
	if !IsKnown("mcp_jsonrpc") {
		t.Error("IsKnown(mcp_jsonrpc) is false — same failure mode as above")
	}
}
