package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// fakeServer hosts an /actuator/health endpoint with operator-controlled
// status code + body. We use one server per service to avoid cross-test
// pollution and to keep the URL maps explicit.
type fakeServer struct {
	mu     sync.Mutex
	status int
	body   string
	hits   int
	srv    *httptest.Server
}

func newFakeServer(status int, body string) *fakeServer {
	f := &fakeServer{status: status, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	return f
}

func (f *fakeServer) URL() string { return f.srv.URL }
func (f *fakeServer) Close()      { f.srv.Close() }

// targetFromServers builds an aforo.Target whose URLs all point at the
// same fake server. Convenient for tests that only care about a subset.
func targetFromServers(byService map[aforo.Service]string) aforo.Target {
	return aforo.Target{Name: "test", URLs: byService}
}

func TestDoctor_AllUp_ReportsOK(t *testing.T) {
	upBody := `{"status":"UP","components":{"db":{"status":"UP"},"redis":{"status":"UP"}}}`
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	urls := map[aforo.Service]string{}
	for _, svc := range aforo.AllProbeServices {
		s := newFakeServer(http.StatusOK, upBody)
		servers = append(servers, s)
		urls[svc] = s.URL()
	}
	d, err := New(Config{
		Target:          targetFromServers(urls),
		BearerToken:     "test-token",
		PerCheckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rep := d.Run(context.Background())
	if rep.Overall != StatusOK {
		t.Fatalf("Overall = %s, want OK", rep.Overall)
	}
	if rep.HasCritical() {
		t.Fatalf("HasCritical true; expected zero critical fails")
	}
	// At least one infra:db row should exist (every fake server returns
	// the db component) — proves component summarization fires.
	var hasDB bool
	for _, c := range rep.Checks {
		if c.Name == "infra:db" && c.Status == StatusOK {
			hasDB = true
		}
	}
	if !hasDB {
		t.Errorf("expected infra:db OK row in report; got %#v", rep.Checks)
	}
}

func TestDoctor_OrgServiceDown_FailsCritical(t *testing.T) {
	upBody := `{"status":"UP"}`
	urls := map[aforo.Service]string{}
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	for _, svc := range aforo.AllProbeServices {
		var s *fakeServer
		if svc == aforo.ServiceOrganization {
			// Closed server simulates connection refused.
			s = newFakeServer(http.StatusOK, upBody)
			s.Close() // intentionally closed before test runs
		} else {
			s = newFakeServer(http.StatusOK, upBody)
			servers = append(servers, s)
		}
		urls[svc] = s.URL()
	}
	d, _ := New(Config{
		Target:          targetFromServers(urls),
		BearerToken:     "tok",
		PerCheckTimeout: 500 * time.Millisecond,
	})
	rep := d.Run(context.Background())
	if !rep.HasCritical() {
		t.Fatalf("expected critical failure; report=%#v", rep.Checks)
	}
}

func TestDoctor_AIServiceWarning_DoesNotFlipOverall(t *testing.T) {
	upBody := `{"status":"UP"}`
	urls := map[aforo.Service]string{}
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	for _, svc := range aforo.AllProbeServices {
		var s *fakeServer
		if svc == aforo.ServiceAIService {
			// 404 on /healthz: doctor recognizes ai-service has no
			// actuator and returns OK with a "no actuator" detail.
			// To force a real failure path, point at a closed server.
			s = newFakeServer(http.StatusOK, upBody)
			s.Close()
		} else {
			s = newFakeServer(http.StatusOK, upBody)
			servers = append(servers, s)
		}
		urls[svc] = s.URL()
	}
	d, _ := New(Config{
		Target:          targetFromServers(urls),
		BearerToken:     "tok",
		PerCheckTimeout: 500 * time.Millisecond,
	})
	rep := d.Run(context.Background())
	if rep.HasCritical() {
		t.Fatalf("ai-service is WARNING-only; expected no critical failure. Report: %#v", rep.Checks)
	}
	// The ai-service row itself should be FAIL/WARNING (transport closed).
	var aiRow *CheckResult
	for i := range rep.Checks {
		if rep.Checks[i].Service == aforo.ServiceAIService {
			aiRow = &rep.Checks[i]
			break
		}
	}
	if aiRow == nil {
		t.Fatal("no ai-service row found")
	}
	if aiRow.Severity != SeverityWarning {
		t.Errorf("ai-service severity = %s, want WARNING", aiRow.Severity)
	}
}

func TestDoctor_AuthBadToken_FailsCritical(t *testing.T) {
	upBody := `{"status":"UP"}`
	urls := map[aforo.Service]string{}
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	for _, svc := range aforo.AllProbeServices {
		s := newFakeServer(http.StatusOK, upBody)
		servers = append(servers, s)
		urls[svc] = s.URL()
	}
	// Replace org-service with one that 401s on the auth probe path.
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/internal/tenants") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upBody))
	}))
	defer authServer.Close()
	urls[aforo.ServiceOrganization] = authServer.URL

	d, _ := New(Config{
		Target:          targetFromServers(urls),
		BearerToken:     "bogus",
		PerCheckTimeout: 1 * time.Second,
	})
	rep := d.Run(context.Background())
	if !rep.HasCritical() {
		t.Fatalf("expected critical fail on bad token; report=%#v", rep.Checks)
	}
	var found bool
	for _, c := range rep.Checks {
		if c.Name == "auth:bearer-token" && c.Status == StatusFail {
			found = true
			if !strings.Contains(c.Remedy, "AFORO_ADMIN_TOKEN") {
				t.Errorf("bad-token remedy should mention AFORO_ADMIN_TOKEN: %s", c.Remedy)
			}
		}
	}
	if !found {
		t.Errorf("expected auth:bearer-token FAIL row")
	}
}

func TestDoctor_NoBearer_AuthChecksSkip(t *testing.T) {
	upBody := `{"status":"UP"}`
	urls := map[aforo.Service]string{}
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	for _, svc := range aforo.AllProbeServices {
		s := newFakeServer(http.StatusOK, upBody)
		servers = append(servers, s)
		urls[svc] = s.URL()
	}
	d, _ := New(Config{
		Target:          targetFromServers(urls),
		BearerToken:     "", // no token — auth checks should SKIP, not FAIL
		PerCheckTimeout: 1 * time.Second,
	})
	rep := d.Run(context.Background())
	if rep.HasCritical() {
		t.Fatalf("expected no critical failure when token is empty; got %#v", rep.Checks)
	}
	var sawSkip bool
	for _, c := range rep.Checks {
		if c.Name == "auth:bearer-token" && c.Status == StatusSkip {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Errorf("expected auth:bearer-token SKIP when token absent")
	}
}

func TestDoctor_ActuatorDOWN_FailsWithFailingComponents(t *testing.T) {
	downBody := `{"status":"DOWN","components":{"db":{"status":"DOWN"},"redis":{"status":"UP"}}}`
	urls := map[aforo.Service]string{}
	servers := []*fakeServer{}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	for _, svc := range aforo.AllProbeServices {
		body := `{"status":"UP","components":{"db":{"status":"UP"},"redis":{"status":"UP"}}}`
		// One service reports DOWN to exercise failure path.
		if svc == aforo.ServiceCatalog {
			body = downBody
		}
		s := newFakeServer(http.StatusOK, body)
		servers = append(servers, s)
		urls[svc] = s.URL()
	}
	d, _ := New(Config{Target: targetFromServers(urls), BearerToken: "tok", PerCheckTimeout: time.Second})
	rep := d.Run(context.Background())
	if !rep.HasCritical() {
		t.Fatalf("expected critical fail when one service reports DOWN")
	}
	// Component summarizer should also report db DOWN (because catalog
	// reports its db as DOWN).
	var infraDBStatus Status
	for _, c := range rep.Checks {
		if c.Name == "infra:db" {
			infraDBStatus = c.Status
		}
	}
	if infraDBStatus != StatusFail {
		t.Errorf("infra:db should be FAIL when any service's db is DOWN; got %s", infraDBStatus)
	}
}

func TestParseActuator_Empty(t *testing.T) {
	p := parseActuator("")
	if p.status != "" || len(p.components) != 0 {
		t.Errorf("empty body should parse to zero value; got %#v", p)
	}
}

func TestParseActuator_Malformed(t *testing.T) {
	p := parseActuator("not json")
	if p.status != "" {
		t.Errorf("malformed body should yield empty status; got %q", p.status)
	}
}

func TestRemedyForUnreachable_LocalSuggestsDocker(t *testing.T) {
	got := remedyForUnreachable(aforo.ServiceCatalog, "http://localhost:8081/actuator/health")
	if !strings.Contains(got, "docker-compose up -d") {
		t.Errorf("local-target remedy should suggest docker-compose; got %q", got)
	}
}

func TestRemedyForUnreachable_RemoteSuggestsNetwork(t *testing.T) {
	got := remedyForUnreachable(aforo.ServiceCatalog, "https://catalog.aforo.io/actuator/health")
	if !strings.Contains(got, "VPN") {
		t.Errorf("remote-target remedy should mention network/VPN; got %q", got)
	}
}
