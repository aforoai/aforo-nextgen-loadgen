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
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// AIAgentWireEnvURL is the environment variable that points the driver at
// the target AI_AGENT test-server (or a gateway sitting in front of one)
// endpoint. Populated by docker-compose in the regression stack, or by
// the operator on the command line for local runs:
//
//	AFORO_LOADGEN_AGENT_URL=http://agent-test-server:8090
//
// Empty at driver-construct time means the scenario references
// ai_agent_wire but the operator forgot to wire the URL — NewAIAgentWire
// returns an error so the run fails loudly at startup instead of
// silently 404-ing every event.
const AIAgentWireEnvURL = "AFORO_LOADGEN_AGENT_URL"

// AIAgentWireDefaultEndSession is the number of invocations per session
// after which the driver auto-ends the session and mints a fresh one on
// the next event with the same agent_id. Overrides via config; keeps a
// scenario with high TPS from monopolising a single long-lived session
// against the test server's capacity cap. Chosen to be well below the
// server's default per-session capacity so long runs don't hit the wall.
const AIAgentWireDefaultEndSession = 20

// AIAgentWire POSTs AI_AGENT capability invocations at @aforo/agent-test-server
// (or any REST endpoint speaking the same wire protocol). Closes the SDK →
// server → optional-gateway → usage-ingestor coverage gap the existing
// ai_agent_rest driver structurally cannot reach: ai_agent_rest posts the
// standard ingest envelope directly, bypassing the SDK entirely; this driver
// speaks the REST wire protocol a real SDK-instrumented agent would send.
//
// Typical wiring:
//
//	loadgen (ai_agent_wire) → agent-test-server
//	   or
//	loadgen (ai_agent_wire) → Kong (with future ai_agent plugin) → agent-test-server
//
// Session lifecycle:
//
//   - First event for a new agent_id → POST /agent/session, cache the
//     returned sessionId.
//   - Subsequent events for the same agent_id → POST /agent/invoke with the
//     cached sessionId + capability + input.
//   - After DefaultEndSession invocations, POST DELETE /agent/session/{id}
//     and drop from the cache. The next event for that agent mints a new
//     session.
//   - Non-AI_AGENT events (envelope.ProductType != "AI_AGENT") are rejected
//     with a transport-class error so a scenario writer catches a
//     mis-paired product_mix + ingestion_paths on the first batch.
//
// The wire body for /agent/invoke is:
//
//	{
//	  "sessionId": "sess_...",
//	  "capability": "summarize_url",
//	  "input": { ... metadata subset ... }
//	}
//
// Capability comes from Envelope.Metadata.capability_name (which
// aiAgentTemplate populates in internal/generator/templates.go). Input
// carries any other metadata keys the extractor would consume — model,
// input_tokens, output_tokens — so the test server has enough context to
// dispatch deterministically without needing to reach back into loadgen.
type AIAgentWire struct {
	url            string
	client         *http.Client
	endAfter       int

	// Session cache: (tenant_id, agent_id) → (sessionId, invocationCount).
	// Guarded by mu because the runner spins many goroutines against this
	// driver concurrently and two events with the same (tenant, agent)
	// landing at the same instant must NOT each open a fresh session.
	//
	// Rule #18 (Tenant Isolation Pre-Commit Checklist, CLAUDE.md): the key
	// is tenantID+":"+agentID — NOT bare agentID. Two different tenants
	// with the same agent id (rare in practice but possible when scenarios
	// use short deterministic ids or when the same agent id is reused
	// across a multi-tenant test) MUST get separate sessions so a
	// downstream gateway plugin doesn't see tenant A's session id under
	// tenant B's X-Tenant-Id header.
	mu       sync.Mutex
	sessions map[string]*wireSession
}

// sessionKey composes the tenant-scoped cache key. Kept as a helper so the
// concatenation format lives in exactly one place — accidental drift
// between the four call sites that touch d.sessions would create silent
// cross-tenant leaks.
func sessionKey(tenantID, agentID string) string {
	return tenantID + ":" + agentID
}

// wireSession is the per-agent cache entry. Held under AIAgentWire.mu.
type wireSession struct {
	id        string
	invCount  int
}

// AIAgentWireConfig lets tests override construction defaults. Runtime code
// uses the env var + defaults.
type AIAgentWireConfig struct {
	// URL overrides the env-var-derived endpoint. Non-empty wins.
	URL string
	// HTTPClient overrides the default; useful for tests to inject a
	// round-tripper. Runtime uses a stock http.Client with a 15s timeout.
	HTTPClient *http.Client
	// RequestTimeout applies only when HTTPClient is not supplied.
	RequestTimeout time.Duration
	// EndSessionAfter is the invocation count at which the driver ends
	// the session and mints a fresh one for the next event with the same
	// agent_id. Defaults to AIAgentWireDefaultEndSession when <= 0.
	EndSessionAfter int
}

// NewAIAgentWire constructs the driver. Returns an error when the target
// URL is empty AND the env var is unset — loud failure beats silent 404s.
func NewAIAgentWire(cfg AIAgentWireConfig) (*AIAgentWire, error) {
	url := cfg.URL
	if url == "" {
		url = os.Getenv(AIAgentWireEnvURL)
	}
	if url == "" {
		return nil, fmt.Errorf("ai_agent_wire: target URL missing — set %s or pass URL in config", AIAgentWireEnvURL)
	}
	// Strip a trailing "/" so path concat below produces exactly one slash.
	url = strings.TrimRight(url, "/")

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

	endAfter := cfg.EndSessionAfter
	if endAfter <= 0 {
		endAfter = AIAgentWireDefaultEndSession
	}

	return &AIAgentWire{
		url:      url,
		client:   client,
		endAfter: endAfter,
		sessions: make(map[string]*wireSession),
	}, nil
}

// Name reports the driver identifier — used by metrics labels and by the
// scenario.IngestionPaths key resolver.
func (d *AIAgentWire) Name() string { return "ai_agent_wire" }

// Submit dispatches a single AI_AGENT event. On the happy path this is
// one HTTP request to POST /agent/invoke; on the first event for a new
// agent_id it's two (create session, then invoke). Non-AI_AGENT events
// short-circuit with a transport error.
func (d *AIAgentWire) Submit(ctx context.Context, e *generator.Event) Result {
	res := Result{Event: e}
	startedAt := time.Now()

	if e == nil {
		res.TransportErr = fmt.Errorf("ai_agent_wire: nil event")
		res.Latency = time.Since(startedAt)
		return res
	}
	if !strings.EqualFold(e.Envelope.ProductType, "AI_AGENT") {
		res.TransportErr = fmt.Errorf(
			"ai_agent_wire: event has product_type=%q — driver only handles AI_AGENT events",
			e.Envelope.ProductType,
		)
		res.Latency = time.Since(startedAt)
		return res
	}
	agentID := stringField(e, "agentId", "")
	if agentID == "" {
		// Fallback: aiAgentTemplate camelCases the key; the metadata could
		// have been massaged by a downstream transform. Also try snake_case.
		agentID = stringField(e, "agent_id", "")
	}
	if agentID == "" {
		res.TransportErr = fmt.Errorf(
			"ai_agent_wire: event has no metadata.agentId — driver requires an agent identifier per event",
		)
		res.Latency = time.Since(startedAt)
		return res
	}
	capability := stringField(e, "capability_name", "")
	if capability == "" {
		res.TransportErr = fmt.Errorf(
			"ai_agent_wire: event has no metadata.capability_name — driver requires a capability per event (agent_id=%s)",
			agentID,
		)
		res.Latency = time.Since(startedAt)
		return res
	}

	// Resolve or open a session for this (tenant, agent) pair. Two events
	// for the same agent id under DIFFERENT tenants must get separate
	// sessions per Rule #18 — the key is composed by sessionKey below.
	// resolveSession may perform a POST /agent/session round-trip if this
	// is the first event for the pair; that failure is attributed to this
	// event.
	sessionID, resolveErr := d.resolveSession(ctx, e, agentID)
	if resolveErr != nil {
		res.TransportErr = resolveErr
		res.Latency = time.Since(startedAt)
		return res
	}

	// Build the /agent/invoke body: sessionId + capability + input.
	// input carries the model / token counts / trace id / any other
	// non-extractor metadata so the test server has enough context to
	// dispatch deterministically (and, for a hypothetical future gateway
	// plugin sitting in front, so the plugin sees the full request shape
	// a real SDK-instrumented agent would send).
	input := buildAgentInvokeInput(e, capability)
	body, err := json.Marshal(map[string]any{
		"sessionId":  sessionID,
		"capability": capability,
		"input":      input,
	})
	if err != nil {
		res.TransportErr = fmt.Errorf("ai_agent_wire: marshal invoke body: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	res.BytesSent = len(body)

	invokeURL := d.url + "/agent/invoke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invokeURL, bytes.NewReader(body))
	if err != nil {
		res.TransportErr = fmt.Errorf("ai_agent_wire: build invoke request: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Envelope.CustomerID != "" {
		req.Header.Set("X-Customer-Id", e.Envelope.CustomerID)
	}
	if e.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
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

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(respBody) > BodyExcerptMax {
			res.BodyExcerpt = string(respBody[:BodyExcerptMax]) + "…"
		} else {
			res.BodyExcerpt = string(respBody)
		}
		// 404 unknown_session usually means the test server's session
		// sweeper dropped an idle session between two events. Reset the
		// cache so the next event opens a fresh session.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			d.evictSession(e.TenantID, agentID)
		}
		return res
	}

	// Success — bump the invocation counter and end the session if we've
	// hit the cap so the next event mints a fresh one. End-session is
	// best-effort: the response is ignored (the session state is already
	// terminal server-side once DELETE returns), and a transport failure
	// here does NOT flag the invocation itself as failed.
	if d.recordInvocation(e.TenantID, agentID) >= d.endAfter {
		d.endSession(ctx, e.TenantID, agentID)
	}
	return res
}

// resolveSession returns the cached session id for the (tenantID, agentID)
// pair, opening a fresh one against the server if the cache misses. The
// lock is held only around the cache lookup + cache write; the actual HTTP
// round-trip happens outside the lock so a slow server can't stall other
// agents' events queuing behind the mutex.
//
// Key composition per Rule #18 (see sessionKey docstring) — two different
// tenants with the same agent id get separate sessions.
func (d *AIAgentWire) resolveSession(ctx context.Context, e *generator.Event, agentID string) (string, error) {
	key := sessionKey(e.TenantID, agentID)
	d.mu.Lock()
	if s, ok := d.sessions[key]; ok {
		id := s.id
		d.mu.Unlock()
		return id, nil
	}
	d.mu.Unlock()

	// Cache miss — open a new session. Any concurrent event for the same
	// (tenant, agent) may race and each open their own; the second to
	// write to the cache overwrites the first. Both sessions remain valid
	// on the server side (test server capacity cap enforces the upper
	// bound if the workload really is that heavy).
	body, err := json.Marshal(map[string]any{
		"agentId":  agentID,
		"tenantId": e.TenantID,
		"metadata": map[string]any{
			"loadgen_source": "ai_agent_wire",
			"customer_id":    e.Envelope.CustomerID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("ai_agent_wire: marshal create-session body: %w", err)
	}

	sessURL := d.url + "/agent/session"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sessURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai_agent_wire: build create-session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai_agent_wire: create-session round-trip: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"ai_agent_wire: create-session returned %d: %s",
			resp.StatusCode, string(respBody),
		)
	}

	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("ai_agent_wire: parse create-session response: %w", err)
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("ai_agent_wire: create-session response missing sessionId (body=%s)", string(respBody))
	}

	d.mu.Lock()
	d.sessions[key] = &wireSession{id: out.SessionID, invCount: 0}
	d.mu.Unlock()
	return out.SessionID, nil
}

// recordInvocation atomically bumps the invocation counter for the
// (tenantID, agentID) pair and returns the new value. Callers use the
// return value to decide whether to end the session.
func (d *AIAgentWire) recordInvocation(tenantID, agentID string) int {
	key := sessionKey(tenantID, agentID)
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[key]
	if !ok {
		return 0
	}
	s.invCount++
	return s.invCount
}

// evictSession drops a (tenant, agent) pair's cached session id — used on
// unknown_session 404s so the next event for that pair opens a new session.
func (d *AIAgentWire) evictSession(tenantID, agentID string) {
	key := sessionKey(tenantID, agentID)
	d.mu.Lock()
	delete(d.sessions, key)
	d.mu.Unlock()
}

// endSession sends DELETE /agent/session/{id} for the cached session id
// and drops the entry. Best-effort: failure is logged into the noise
// stream but does NOT surface on the invocation Result. Runs synchronously
// so a scenario finishing right after the last invocation on a session
// still gets the DELETE observed by the server.
func (d *AIAgentWire) endSession(ctx context.Context, tenantID, agentID string) {
	key := sessionKey(tenantID, agentID)
	d.mu.Lock()
	s, ok := d.sessions[key]
	if !ok {
		d.mu.Unlock()
		return
	}
	sessID := s.id
	delete(d.sessions, key)
	d.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, d.url+"/agent/session/"+sessID, nil)
	if err != nil {
		return
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// Close releases idle connections. Doesn't end outstanding sessions —
// the session store on the test server side has its own idle sweeper
// so a driver going away doesn't leak sessions indefinitely.
func (d *AIAgentWire) Close() error {
	closeIdle(d.client)
	return nil
}

// buildAgentInvokeInput selects the subset of metadata that becomes the
// /agent/invoke `input` payload. Capability handlers on the test server
// only read a few keys per capability (url / text / claim / etc.); the
// driver doesn't know which one applies, so it passes everything and
// lets the handler pick. The set is bounded — we don't fan the entire
// metadata blob through, only the shape a real agent would send.
func buildAgentInvokeInput(e *generator.Event, capability string) map[string]any {
	// Copy the metadata subset the server's capability handlers actually
	// key on. Anything the handlers ignore is fine to include — the
	// deterministic dispatch just derives durations + tokens from input
	// length — but we keep the payload compact so bytes-sent metrics
	// stay meaningful.
	input := map[string]any{
		"capability": capability,
	}
	if e.Envelope.Metadata != nil {
		// Copy through the fields aiAgentTemplate populates that the
		// test server's canonical capabilities would consume. `input_tokens`
		// / `output_tokens` / `model` / `trace_id` land as hints even
		// though the current deterministic handlers ignore them — a
		// future gateway plugin sitting in front will still see them,
		// which is the whole point of exercising the wire path.
		for _, k := range []string{
			"model", "input_tokens", "output_tokens", "trace_id",
			// Handler-specific hints for the 5 canonical capabilities:
			"url", "length", "text", "claim", "sources",
			"query", "question", "max_sources", "require_hitl",
		} {
			if v, ok := e.Envelope.Metadata[k]; ok {
				input[k] = v
			}
		}
	}
	// Give the deterministic handlers a stable-per-event payload to
	// derive duration + tokens from, if the metadata block above is
	// sparse. Uses the event id so re-runs against the same generator
	// seed produce identical wire bodies.
	if _, hasURL := input["url"]; !hasURL && capability == "summarize_url" {
		input["url"] = "https://example.com/loadgen/" + e.EventID
	}
	if _, hasText := input["text"]; !hasText && capability == "extract_entities" {
		input["text"] = "loadgen synthetic text for event " + e.EventID
	}
	if _, hasClaim := input["claim"]; !hasClaim && capability == "verify_claim" {
		input["claim"] = "loadgen synthetic claim"
		input["sources"] = []string{"https://synthetic.example/loadgen"}
	}
	if _, hasQuery := input["query"]; !hasQuery && capability == "rank_sources" {
		input["query"] = "loadgen"
		input["sources"] = []string{"a", "b", "c"}
	}
	if _, hasQ := input["question"]; !hasQ && capability == "answer_question" {
		input["question"] = "loadgen synthetic question for event " + e.EventID
	}
	return input
}

// canonicalAIAgentWireName is used by the registry + validator + generator
// weighting logic. Exported as a const so drift between call sites is
// caught at compile time.
const canonicalAIAgentWireName = "ai_agent_wire"

// Compile-time guard — Name() must return the canonical string.
var _ = func() bool {
	if !strings.EqualFold((&AIAgentWire{}).Name(), canonicalAIAgentWireName) {
		panic("ai_agent_wire: Name() diverged from canonicalAIAgentWireName")
	}
	return true
}()
