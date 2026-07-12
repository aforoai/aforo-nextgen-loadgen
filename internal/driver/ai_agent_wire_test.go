package driver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// Helper: build a canonical AI_AGENT event with the metadata fields the
// driver reads (agentId camelCase, capability_name snake_case) plus a few
// hints downstream capabilities key on.
func aiAgentWireEvent(agentID, capability string) *generator.Event {
	return &generator.Event{
		TenantID: "tenant_test",
		EventID:  "evt_wire_1",
		Envelope: generator.Envelope{
			CustomerID:  "cust_wire_1",
			MetricName:  "ai_agent.capability_invocations",
			ProductType: "AI_AGENT",
			Metadata: map[string]any{
				"agentId":         agentID,
				"capability_name": capability,
				"model":           "claude-sonnet-4-6",
				"input_tokens":    240,
				"output_tokens":   62,
				"session_id":      "sess-hint",
				"executionStatus": "SUCCESS",
			},
		},
	}
}

// fakeAgentServer is a minimal HTTP stand-in for @aforo/agent-test-server —
// enough to exercise the driver's create-session + invoke + end-session
// paths without booting the real thing.
type fakeAgentServer struct {
	mu           sync.Mutex
	sessions     map[string]string // sessionId → agentId
	invocations  atomic.Int64
	createReqs   atomic.Int64
	deleteReqs   atomic.Int64
	capturedBody []byte
	// nextStatus lets tests inject a specific status code for the NEXT
	// invoke response. 0 means "use 200".
	nextStatus int
}

func newFakeAgentServer() *fakeAgentServer {
	return &fakeAgentServer{sessions: make(map[string]string)}
}

func (f *fakeAgentServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/session":
			f.createReqs.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req struct {
				AgentID string `json:"agentId"`
			}
			_ = json.Unmarshal(body, &req)
			// Deterministic session id derived from agent id so tests
			// can assert on the mapping without dealing with UUIDs.
			sessID := "sess_" + req.AgentID
			f.sessions[sessID] = req.AgentID
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Session-Id", sessID)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId":       sessID,
				"agentId":         req.AgentID,
				"startedAt":       "2026-07-12T00:00:00Z",
				"idleTimeoutSec":  900,
			})

		case r.Method == http.MethodPost && r.URL.Path == "/agent/invoke":
			f.invocations.Add(1)
			body, _ := io.ReadAll(r.Body)
			f.capturedBody = body
			status := f.nextStatus
			if status == 0 {
				status = http.StatusOK
			}
			f.nextStatus = 0
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"invocationId":        "inv_test",
					"sessionId":           "sess_x",
					"capability":          "summarize_url",
					"output":              map[string]any{"summary": "ok"},
					"executionStatus":     "SUCCESS",
					"executionDurationMs": 100,
					"tokensIn":            10,
					"tokensOut":           20,
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "unknown_session", "message": "gone"},
				})
			}

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/agent/session/"):
			f.deleteReqs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId": strings.TrimPrefix(r.URL.Path, "/agent/session/"),
				"endedAt":   "2026-07-12T00:00:01Z",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestAIAgentWire_CreateSessionOnFirstEventAndReuseOnSecond(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	// First event for agent_x — should create + invoke.
	res1 := d.Submit(context.Background(), aiAgentWireEvent("agent_x", "summarize_url"))
	if res1.Status != 200 {
		t.Fatalf("first invoke status: %d (transportErr=%v, body=%s)", res1.Status, res1.TransportErr, res1.BodyExcerpt)
	}
	if got := fake.createReqs.Load(); got != 1 {
		t.Errorf("expected 1 create-session request, got %d", got)
	}
	if got := fake.invocations.Load(); got != 1 {
		t.Errorf("expected 1 invoke, got %d", got)
	}

	// Second event for same agent — should REUSE, only 1 invoke, no new create.
	res2 := d.Submit(context.Background(), aiAgentWireEvent("agent_x", "extract_entities"))
	if res2.Status != 200 {
		t.Fatalf("second invoke status: %d", res2.Status)
	}
	if got := fake.createReqs.Load(); got != 1 {
		t.Errorf("expected create-session STILL 1 (session reused), got %d", got)
	}
	if got := fake.invocations.Load(); got != 2 {
		t.Errorf("expected 2 invocations, got %d", got)
	}
}

func TestAIAgentWire_InvokeBodyCarriesSessionAndCapability(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	e := aiAgentWireEvent("agent_shape", "verify_claim")
	res := d.Submit(context.Background(), e)
	if res.Status != 200 {
		t.Fatalf("submit status: %d (transportErr=%v)", res.Status, res.TransportErr)
	}

	var body map[string]any
	if err := json.Unmarshal(fake.capturedBody, &body); err != nil {
		t.Fatalf("captured body not JSON: %v", err)
	}
	if body["sessionId"] != "sess_agent_shape" {
		t.Errorf("sessionId: expected sess_agent_shape, got %v", body["sessionId"])
	}
	if body["capability"] != "verify_claim" {
		t.Errorf("capability: expected verify_claim, got %v", body["capability"])
	}
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input: expected object, got %T", body["input"])
	}
	if input["model"] != "claude-sonnet-4-6" {
		t.Errorf("input.model: expected claude-sonnet-4-6, got %v", input["model"])
	}
	// Fallback claim/sources should be present because the event's metadata
	// didn't carry them explicitly.
	if _, ok := input["claim"]; !ok {
		t.Error("input.claim: expected fallback claim for verify_claim capability")
	}
}

func TestAIAgentWire_RejectsNonAIAgentEvents(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		TenantID: "tenant_test",
		Envelope: generator.Envelope{
			CustomerID:  "cust_1",
			MetricName:  "api.calls",
			ProductType: "API",
			Metadata:    map[string]any{"endpoint": "/v1/x"},
		},
	}
	res := d.Submit(context.Background(), e)
	if res.TransportErr == nil {
		t.Fatalf("expected transport err for non-AI_AGENT event, got status=%d", res.Status)
	}
	if !strings.Contains(res.TransportErr.Error(), "AI_AGENT") {
		t.Errorf("error should mention AI_AGENT; got: %v", res.TransportErr)
	}
	if got := fake.createReqs.Load(); got != 0 {
		t.Errorf("no create-session should have been issued; got %d", got)
	}
}

func TestAIAgentWire_MissingURLReturnsError(t *testing.T) {
	t.Setenv(AIAgentWireEnvURL, "")
	_, err := NewAIAgentWire(AIAgentWireConfig{})
	if err == nil {
		t.Fatal("expected error when URL is empty and env var unset")
	}
	if !strings.Contains(err.Error(), AIAgentWireEnvURL) {
		t.Errorf("error should name the env var %q; got: %v", AIAgentWireEnvURL, err)
	}
}

func TestAIAgentWire_UsesEnvVarWhenConfigURLEmpty(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	t.Setenv(AIAgentWireEnvURL, ts.URL)
	d, err := NewAIAgentWire(AIAgentWireConfig{})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	res := d.Submit(context.Background(), aiAgentWireEvent("agent_env", "summarize_url"))
	if res.Status != 200 {
		t.Fatalf("expected 200, got %d (transportErr=%v)", res.Status, res.TransportErr)
	}
}

func TestAIAgentWire_EndSessionAfterCap(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL, EndSessionAfter: 3})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	// Send 3 events → should hit the cap and DELETE the session, then a
	// 4th event should CREATE a fresh one.
	for i := 0; i < 3; i++ {
		res := d.Submit(context.Background(), aiAgentWireEvent("agent_cap", "summarize_url"))
		if res.Status != 200 {
			t.Fatalf("event %d status: %d (transportErr=%v)", i, res.Status, res.TransportErr)
		}
	}
	if got := fake.createReqs.Load(); got != 1 {
		t.Errorf("expected 1 create at this point, got %d", got)
	}
	if got := fake.deleteReqs.Load(); got != 1 {
		t.Errorf("expected 1 delete after cap, got %d", got)
	}

	// 4th event → new session opened.
	res := d.Submit(context.Background(), aiAgentWireEvent("agent_cap", "summarize_url"))
	if res.Status != 200 {
		t.Fatalf("4th event status: %d", res.Status)
	}
	if got := fake.createReqs.Load(); got != 2 {
		t.Errorf("expected 2 creates after cap-rotate, got %d", got)
	}
}

func TestAIAgentWire_EvictsSessionOn404(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	// First event — happy path, session cached.
	res := d.Submit(context.Background(), aiAgentWireEvent("agent_404", "summarize_url"))
	if res.Status != 200 {
		t.Fatalf("first status: %d", res.Status)
	}

	// Force the NEXT invoke to 404 (simulating server-side idle sweep).
	fake.mu.Lock()
	fake.nextStatus = http.StatusNotFound
	fake.mu.Unlock()
	res2 := d.Submit(context.Background(), aiAgentWireEvent("agent_404", "summarize_url"))
	if res2.Status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", res2.Status)
	}

	// Third event — cache should have been evicted, so a fresh create runs.
	res3 := d.Submit(context.Background(), aiAgentWireEvent("agent_404", "summarize_url"))
	if res3.Status != 200 {
		t.Fatalf("third status: %d", res3.Status)
	}
	if got := fake.createReqs.Load(); got != 2 {
		t.Errorf("expected 2 creates after 404 eviction, got %d", got)
	}
}

// TestAIAgentWire_SessionCacheIsTenantScoped locks in Rule #18: two events
// with the same agent id but DIFFERENT tenant ids must open separate
// server-side sessions. Pre-fix the cache was keyed by bare agent id so
// tenant A's session would be handed to tenant B's event — the classic
// tenant-isolation cross-leak the RLS + cache-key rule exists to prevent.
func TestAIAgentWire_SessionCacheIsTenantScoped(t *testing.T) {
	fake := newFakeAgentServer()
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d, err := NewAIAgentWire(AIAgentWireConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("NewAIAgentWire: %v", err)
	}
	defer d.Close()

	// Same agent id, different tenants — hand-craft the events so the
	// tenant field is the only difference.
	eA := aiAgentWireEvent("shared_agent_id", "summarize_url")
	eA.TenantID = "tenant_A"
	eB := aiAgentWireEvent("shared_agent_id", "summarize_url")
	eB.TenantID = "tenant_B"

	// First event for tenant A — should CREATE a session.
	resA := d.Submit(context.Background(), eA)
	if resA.Status != 200 {
		t.Fatalf("tenant A submit status: %d", resA.Status)
	}
	if got := fake.createReqs.Load(); got != 1 {
		t.Errorf("expected 1 create after tenant A's first event, got %d", got)
	}

	// First event for tenant B with the SAME agent id — MUST create a
	// NEW session (2 total) instead of reusing tenant A's.
	resB := d.Submit(context.Background(), eB)
	if resB.Status != 200 {
		t.Fatalf("tenant B submit status: %d", resB.Status)
	}
	if got := fake.createReqs.Load(); got != 2 {
		t.Errorf("expected 2 creates after tenant B's first event (rule #18 tenant scope), got %d — cache leaked across tenants", got)
	}

	// Second event for tenant A should REUSE its session (still 2 total).
	resA2 := d.Submit(context.Background(), eA)
	if resA2.Status != 200 {
		t.Fatalf("tenant A 2nd submit status: %d", resA2.Status)
	}
	if got := fake.createReqs.Load(); got != 2 {
		t.Errorf("expected creates to stay at 2 after tenant A reuse, got %d", got)
	}
}

func TestAIAgentWire_RegisteredInAllNames(t *testing.T) {
	found := false
	for _, name := range AllNames() {
		if name == "ai_agent_wire" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ai_agent_wire missing from driver.AllNames() — validator will reject scenarios that use it")
	}
	if !IsKnown("ai_agent_wire") {
		t.Error("IsKnown(ai_agent_wire) is false — same failure mode as above")
	}
}
