package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/server"
)

// fixtureCatalog is a hand-rolled scenario list so tests don't depend
// on the embedded YAML — keeps the test fast and the scope narrow.
type fixtureCatalog struct{ scenarios []server.ScenarioInfo }

func (f *fixtureCatalog) List() []server.ScenarioInfo {
	out := make([]server.ScenarioInfo, len(f.scenarios))
	copy(out, f.scenarios)
	return out
}
func (f *fixtureCatalog) Has(name string) bool {
	for _, s := range f.scenarios {
		if s.Name == name {
			return true
		}
	}
	return false
}

func newServerForTest(t *testing.T, identity *server.Identity) (*httptest.Server, *server.MemoryIndex) {
	t.Helper()
	idx := server.NewMemoryIndex()
	tmp := t.TempDir()
	store, err := server.NewLocalStore(tmp + "/manifests")
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	runner, err := server.NewLocalRunner(tmp+"/runs", "/usr/bin/false", []string{"ci-smoke", "high-tps"})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	cat := &fixtureCatalog{scenarios: []server.ScenarioInfo{
		{Name: "ci-smoke", TargetTPS: 10, DurationSecs: 30, Tenants: 5, HighTPS: false},
		{Name: "high-tps", TargetTPS: 5000, DurationSecs: 60, Tenants: 50, HighTPS: true},
	}}
	srv, err := server.New(server.Config{
		ListenAddr:  ":0",
		Auth:        &server.StaticAuthenticator{Identity: identity},
		Index:       idx,
		Storage:     store,
		Runner:      runner,
		ScenarioCat: cat,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, idx
}

func adminIdentity() *server.Identity {
	return &server.Identity{UserID: "11111111-1111-1111-1111-111111111111", Email: "admin@aforo.io", Role: "platform_admin"}
}

func supportIdentity() *server.Identity {
	return &server.Identity{UserID: "22222222-2222-2222-2222-222222222222", Email: "support@aforo.io", Role: "support_agent"}
}

func unrolledIdentity() *server.Identity {
	return &server.Identity{UserID: "33333333-3333-3333-3333-333333333333", Email: "stranger@aforo.io", Role: ""}
}

func do(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, ts.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer fake-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestHealth_PublicNoAuth(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got server.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Status != "ok" {
		t.Errorf("status: %s", got.Status)
	}
	if got.Index != "memory" {
		t.Errorf("index kind: %s", got.Index)
	}
}

func TestListScenarios_OpenToAnyInternalRole(t *testing.T) {
	ts, _ := newServerForTest(t, supportIdentity())

	resp := do(t, ts, http.MethodGet, "/api/v1/scenarios", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got map[string][]server.ScenarioInfo
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got["scenarios"]) != 2 {
		t.Errorf("want 2 scenarios, got %d", len(got["scenarios"]))
	}
}

func TestListScenarios_RejectsUnrolled(t *testing.T) {
	ts, _ := newServerForTest(t, unrolledIdentity())
	resp := do(t, ts, http.MethodGet, "/api/v1/scenarios", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestTriggerRun_AcceptsPlatformAdmin(t *testing.T) {
	ts, idx := newServerForTest(t, adminIdentity())

	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{
		Scenario: "ci-smoke",
		Target:   "local",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var tr server.TriggerResponse
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	if !strings.HasPrefix(tr.RunID, "ci-smoke-") {
		t.Errorf("run id missing scenario prefix: %s", tr.RunID)
	}
	if tr.Status != server.RunStatusQueued {
		t.Errorf("status: %s", tr.Status)
	}

	// Index must have an immediate row so the polling UI sees it.
	_, err := idx.Get(context.Background(), tr.RunID)
	if err != nil {
		t.Fatalf("index lookup: %v", err)
	}
}

func TestTriggerRun_RejectsSupportAgent(t *testing.T) {
	ts, _ := newServerForTest(t, supportIdentity())

	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{Scenario: "ci-smoke", Target: "local"})
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestTriggerRun_RejectsUnknownScenario(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())
	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{Scenario: "phantom-scenario", Target: "local"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestTriggerRun_HighTPSRequiresAck(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())

	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{Scenario: "high-tps", Target: "local"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d", resp.StatusCode)
	}
}

func TestTriggerRun_HighTPSWithAckSucceeds(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())

	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{
		Scenario:    "high-tps",
		Target:      "local",
		Acknowledge: true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestTriggerRun_RejectsEmptyScenario(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())
	resp := do(t, ts, http.MethodPost, "/api/v1/runs", server.TriggerRequest{Target: "local"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())
	resp := do(t, ts, http.MethodGet, "/api/v1/runs/no-such-run", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestGetRun_RoundTrip(t *testing.T) {
	ts, idx := newServerForTest(t, adminIdentity())

	// Seed a row directly via the index — bypasses the worker spawn.
	_ = idx.Insert(context.Background(), server.Run{
		RunID:   "seeded-1",
		Scenario: "ci-smoke",
		Target:  "local",
		Status:  server.RunStatusCompleted,
	})

	resp := do(t, ts, http.MethodGet, "/api/v1/runs/seeded-1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got server.Run
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.RunID != "seeded-1" {
		t.Errorf("run id: %s", got.RunID)
	}
	if got.Status != server.RunStatusCompleted {
		t.Errorf("status: %s", got.Status)
	}
}

func TestListRuns_PaginationAndStatusFilter(t *testing.T) {
	ts, idx := newServerForTest(t, supportIdentity())
	for i, status := range []server.RunStatus{
		server.RunStatusQueued, server.RunStatusRunning, server.RunStatusCompleted,
		server.RunStatusFailed, server.RunStatusCancelled,
	} {
		_ = idx.Insert(context.Background(), server.Run{
			RunID:    "r" + string(rune('0'+i)),
			Scenario: "ci-smoke",
			Target:   "local",
			Status:   status,
		})
	}

	resp := do(t, ts, http.MethodGet, "/api/v1/runs?status=completed", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var lr server.ListResponse
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	if len(lr.Runs) != 1 {
		t.Fatalf("filter: want 1 run, got %d", len(lr.Runs))
	}
	if lr.Runs[0].Status != server.RunStatusCompleted {
		t.Errorf("filter mismatch: %s", lr.Runs[0].Status)
	}
}

func TestCancelRun_NotActive404(t *testing.T) {
	ts, _ := newServerForTest(t, adminIdentity())
	resp := do(t, ts, http.MethodPost, "/api/v1/runs/never-was/cancel", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestCancelRun_RejectsSupport(t *testing.T) {
	ts, _ := newServerForTest(t, supportIdentity())
	resp := do(t, ts, http.MethodPost, "/api/v1/runs/never-was/cancel", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestUnauthorizedWithoutBearer(t *testing.T) {
	// StaticAuthenticator ignores the bearer, so we spin up a
	// dedicated server with a SupabaseAuthenticator pointed at an
	// upstream that always rejects.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	auth, err := server.NewSupabaseAuthenticator(upstream.URL, "anon", "service")
	if err != nil {
		t.Fatalf("supabase auth: %v", err)
	}
	srv, err := server.New(server.Config{
		ListenAddr:  ":0",
		Auth:        auth,
		Index:       server.NewMemoryIndex(),
		Storage:     mustLocalStore(t),
		Runner:      mustRunner(t),
		ScenarioCat: &fixtureCatalog{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer bogus")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func mustLocalStore(t *testing.T) *server.LocalStore {
	s, err := server.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	return s
}

func mustRunner(t *testing.T) *server.LocalRunner {
	r, err := server.NewLocalRunner(t.TempDir(), "/usr/bin/false", []string{"ci-smoke"})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	return r
}
