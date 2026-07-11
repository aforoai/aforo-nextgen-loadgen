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

// Helper: build a canonical AI_AGENT event with the metadata fields the
// driver + downstream ProductTypeEventExtractor.extractAiAgentFields expect.
func aiAgentEvent(capability, agent, session string) *generator.Event {
	return &generator.Event{
		TenantID: "tenant_test",
		EventID:  "evt_ai_1",
		Auth: generator.EventAuth{
			Token: "test-token",
		},
		Envelope: generator.Envelope{
			CustomerID:     "cust_1",
			MetricName:     "ai_agent.capability_invocations",
			Quantity:       1.0,
			IdempotencyKey: "evt_ai_1",
			ProductType:    "AI_AGENT",
			// Metadata shape mirrors aiAgentTemplate: camelCase keys for
			// agentId/executionStatus/executionDurationMs (extractor reads
			// these camelCase from metadata as fallback for the top-level
			// DTO fields), snake_case for capability_name (extractor reads
			// this exact spelling). Session_id is not extracted — kept only
			// as an operator convenience for X-Aforo-Session-Id.
			Metadata: map[string]any{
				"capability_name":     capability,
				"agentId":             agent,
				"session_id":          session,
				"executionStatus":     "success",
				"executionDurationMs": 123,
			},
		},
	}
}

func TestAIAgentREST_EmitsWellFormedIngestEnvelope(t *testing.T) {
	var captured struct {
		method      string
		authHdr     string
		tenantHdr   string
		customerHdr string
		sessionHdr  string
		eventIDHdr  string
		body        []byte
		contentType string
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.authHdr = r.Header.Get("Authorization")
		captured.tenantHdr = r.Header.Get("X-Tenant-Id")
		captured.customerHdr = r.Header.Get("X-Customer-Id")
		captured.sessionHdr = r.Header.Get("X-Loadgen-Session-Id")
		captured.eventIDHdr = r.Header.Get("X-Loadgen-Event-Id")
		captured.contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		captured.body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer ts.Close()

	d, err := NewAIAgentREST(AIAgentRESTConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentREST: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), aiAgentEvent("summarize_email", "agent-42", "sess-99"))
	if res.Status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (body=%q, transportErr=%v)", res.Status, res.BodyExcerpt, res.TransportErr)
	}
	if res.TransportErr != nil {
		t.Fatalf("unexpected transport err: %v", res.TransportErr)
	}
	if captured.method != http.MethodPost {
		t.Errorf("expected POST, got %s", captured.method)
	}
	if captured.contentType != "application/json" {
		t.Errorf("Content-Type: expected application/json, got %q", captured.contentType)
	}
	if captured.authHdr != "Bearer test-token" {
		t.Errorf("Authorization: expected Bearer test-token, got %q", captured.authHdr)
	}
	if captured.tenantHdr != "tenant_test" {
		t.Errorf("X-Tenant-Id: expected tenant_test, got %q", captured.tenantHdr)
	}
	if captured.customerHdr != "cust_1" {
		t.Errorf("X-Customer-Id: expected cust_1, got %q", captured.customerHdr)
	}
	if captured.sessionHdr != "sess-99" {
		t.Errorf("X-Loadgen-Session-Id: expected sess-99, got %q", captured.sessionHdr)
	}
	if captured.eventIDHdr != "evt_ai_1" {
		t.Errorf("X-Loadgen-Event-Id: expected evt_ai_1, got %q", captured.eventIDHdr)
	}

	var envelope map[string]any
	if err := json.Unmarshal(captured.body, &envelope); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, captured.body)
	}
	if envelope["productType"] != "AI_AGENT" {
		t.Errorf("productType: expected AI_AGENT, got %v", envelope["productType"])
	}
	if envelope["customerId"] != "cust_1" {
		t.Errorf("customerId: expected cust_1, got %v", envelope["customerId"])
	}
	if envelope["metricName"] != "ai_agent.capability_invocations" {
		t.Errorf("metricName: expected ai_agent.capability_invocations, got %v", envelope["metricName"])
	}
	if envelope["idempotencyKey"] != "evt_ai_1" {
		t.Errorf("idempotencyKey: expected evt_ai_1, got %v", envelope["idempotencyKey"])
	}
	metadata, ok := envelope["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata: expected object, got %T", envelope["metadata"])
	}
	if metadata["capability_name"] != "summarize_email" {
		t.Errorf("metadata.capability_name: expected summarize_email, got %v", metadata["capability_name"])
	}
	if metadata["agentId"] != "agent-42" {
		t.Errorf("metadata.agentId: expected agent-42, got %v", metadata["agentId"])
	}
	if metadata["session_id"] != "sess-99" {
		t.Errorf("metadata.session_id: expected sess-99, got %v", metadata["session_id"])
	}
	if metadata["executionStatus"] != "success" {
		t.Errorf("metadata.executionStatus: expected success, got %v", metadata["executionStatus"])
	}
	// Duration serializes as float64 through encoding/json.
	if dur, ok := metadata["executionDurationMs"].(float64); !ok || dur != 123 {
		t.Errorf("metadata.executionDurationMs: expected 123, got %v (%T)", metadata["executionDurationMs"], metadata["executionDurationMs"])
	}
	// Regression lock: pre-fix snake_case keys for extractor-facing fields
	// must not appear — extractAiAgentFields would silently drop them.
	for _, k := range []string{"agent_id", "execution_status", "execution_duration_ms"} {
		if _, present := metadata[k]; present {
			t.Errorf("metadata must not contain %q — extractAiAgentFields reads camelCase, snake_case drops silently", k)
		}
	}
}

func TestAIAgentREST_RejectsNonAIAgentEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be hit for non-AI_AGENT event")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d, err := NewAIAgentREST(AIAgentRESTConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentREST: %v", err)
	}
	defer func() { _ = d.Close() }()

	// MCP_SERVER event — wrong product type.
	e := &generator.Event{
		TenantID: "tenant_test",
		Envelope: generator.Envelope{
			CustomerID:  "cust_1",
			MetricName:  "mcp.tool_invocations",
			ProductType: "MCP_SERVER",
			Metadata: map[string]any{
				"tool_name": "search_web",
			},
		},
	}
	res := d.Submit(context.Background(), e)
	if res.TransportErr == nil {
		t.Fatalf("expected transport err for non-AI_AGENT event, got status=%d", res.Status)
	}
	if !strings.Contains(res.TransportErr.Error(), "AI_AGENT") {
		t.Errorf("error message should mention AI_AGENT; got: %v", res.TransportErr)
	}

	// Empty product type — also rejected.
	e2 := &generator.Event{
		TenantID: "tenant_test",
		Envelope: generator.Envelope{
			CustomerID: "cust_1",
			MetricName: "api.requests",
			Metadata:   map[string]any{"endpoint": "/api/v1/x"},
		},
	}
	res2 := d.Submit(context.Background(), e2)
	if res2.TransportErr == nil {
		t.Fatalf("expected transport err for empty product_type, got status=%d", res2.Status)
	}
}

func TestAIAgentREST_MissingURLReturnsError(t *testing.T) {
	// Ensure the env var is not set for this test — the CI environment
	// shouldn't have it, but tests should be robust regardless.
	t.Setenv(AIAgentRESTEnvURL, "")
	_, err := NewAIAgentREST(AIAgentRESTConfig{})
	if err == nil {
		t.Fatal("expected error when URL is empty and env var unset")
	}
	if !strings.Contains(err.Error(), AIAgentRESTEnvURL) {
		t.Errorf("error should name the env var %q; got: %v", AIAgentRESTEnvURL, err)
	}
}

func TestAIAgentREST_UsesEnvVarWhenConfigURLEmpty(t *testing.T) {
	var hit int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	t.Setenv(AIAgentRESTEnvURL, ts.URL)
	d, err := NewAIAgentREST(AIAgentRESTConfig{})
	if err != nil {
		t.Fatalf("NewAIAgentREST: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), aiAgentEvent("plan_actions", "agent-1", "sess-1"))
	if res.Status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (transportErr=%v)", res.Status, res.TransportErr)
	}
	if hit != 1 {
		t.Errorf("expected 1 server hit, got %d", hit)
	}
}

func TestAIAgentREST_ConfigURLWinsOverEnvVar(t *testing.T) {
	// Env var points at a URL that would FAIL the test if hit.
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("env-var URL was hit but config URL should win")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()
	t.Setenv(AIAgentRESTEnvURL, failingServer.URL)

	var hit int
	winner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer winner.Close()

	d, err := NewAIAgentREST(AIAgentRESTConfig{URL: winner.URL})
	if err != nil {
		t.Fatalf("NewAIAgentREST: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), aiAgentEvent("verify_output", "a", "s"))
	if res.Status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", res.Status)
	}
	if hit != 1 {
		t.Errorf("expected 1 hit on config URL server, got %d", hit)
	}
}

func TestAIAgentREST_RegisteredInAllNames(t *testing.T) {
	found := false
	for _, name := range AllNames() {
		if name == "ai_agent_rest" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ai_agent_rest missing from driver.AllNames() — validator will reject scenarios that use it")
	}
	if !IsKnown("ai_agent_rest") {
		t.Error("IsKnown(ai_agent_rest) is false — same failure mode as above")
	}
}

func TestAIAgentREST_NilEventRejectedCleanly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be hit for nil event")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d, err := NewAIAgentREST(AIAgentRESTConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentREST: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), nil)
	if res.TransportErr == nil {
		t.Fatalf("expected transport err for nil event")
	}
}
