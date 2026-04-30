// Package doctor implements the pre-flight diagnostic check used by the
// `aforo-loadgen doctor` subcommand and by the `e2e` orchestrator before
// any seed work begins.
//
// Goals:
//
//  1. Verify every microservice the e2e flow touches is reachable. Doctor
//     probes /actuator/health on each Spring Boot service and a lightweight
//     liveness path on ai-service (which has no actuator today).
//  2. Verify the admin bearer token is present and that at least one
//     organization tenant exists (or the operator has admin permission to
//     create one). The doctor's auth probe hits a low-cost authenticated
//     endpoint — listing the first page of tenants on organization-service.
//  3. Surface infra dependencies (PostgreSQL, Kafka, ClickHouse, Redis) by
//     reading the components map inside /actuator/health responses where
//     the platform exposes them. We do NOT open direct DB connections —
//     local docker-compose binds those ports loopback-only and the doctor
//     intentionally stays inside the HTTP plane.
//
// Failure ergonomics matter here: this is the first thing a new dev runs
// and the error messages must be actionable. Every failed check carries a
// `Remedy` string that names the exact next command (e.g. "cd
// aforo-nextgen-docker && docker-compose up -d") so the operator can copy
// and paste rather than guess.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// Status is the per-check verdict.
type Status string

const (
	StatusOK   Status = "OK"
	StatusFail Status = "FAIL"
	// StatusSkip is reserved for cases where a check could not be run (target
	// has no URL configured for that service, e.g. ai-service in staging).
	// SKIP is not a pass — the overall verdict still considers them.
	StatusSkip Status = "SKIP"
)

// Severity classifies a check's importance to the e2e flow. CRITICAL
// failures fail the doctor; WARNING failures are logged but do not flip
// the overall verdict to FAIL. Used for ai-service, which is required for
// admin AI features but not for any base e2e archetype.
type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityWarning  Severity = "WARNING"
)

// CheckResult is the outcome of one named diagnostic.
type CheckResult struct {
	Name     string        `json:"name"`
	Service  aforo.Service `json:"service,omitempty"`
	Status   Status        `json:"status"`
	Severity Severity      `json:"severity"`
	Detail   string        `json:"detail,omitempty"`
	Remedy   string        `json:"remedy,omitempty"`
	Duration time.Duration `json:"duration_ms"`
	URL      string        `json:"url,omitempty"`

	// components captures the actuator's per-component map. NOT
	// serialized to JSON — it's only used internally by
	// summarizeInfraComponents. Without this, summarization had to
	// re-fetch every actuator response, doubling HTTP traffic and
	// using context.Background() instead of the caller's ctx.
	components map[string]string `json:"-"`
}

// Report is the full doctor result.
type Report struct {
	Target  string        `json:"target"`
	Checks  []CheckResult `json:"checks"`
	Overall Status        `json:"overall"`
	Elapsed time.Duration `json:"elapsed_ms"`
}

// HasCritical reports whether any check failed at CRITICAL severity.
// Callers use this to decide whether to bail before seed.
func (r *Report) HasCritical() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail && c.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// Config wires the runner.
type Config struct {
	Target aforo.Target
	// BearerToken is required if you want auth + tenant existence
	// checks to run. Empty token → those probes SKIP.
	BearerToken string
	// HTTPClient — nil falls back to a sensible default with timeouts.
	HTTPClient *http.Client
	// PerCheckTimeout is the hard cap on any one HTTP request the doctor
	// makes. Zero → 5s, which is enough for a cold-start docker container.
	PerCheckTimeout time.Duration
	// Now — clock injection for tests; nil → time.Now.
	Now func() time.Time
}

// Doctor is the diagnostic runner.
type Doctor struct {
	cfg Config
	hc  *http.Client
	now func() time.Time
}

// New constructs a Doctor. Returns an error if Target is unconfigured.
func New(cfg Config) (*Doctor, error) {
	if len(cfg.Target.URLs) == 0 {
		return nil, errors.New("doctor: Target has no URLs")
	}
	if cfg.PerCheckTimeout <= 0 {
		cfg.PerCheckTimeout = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.PerCheckTimeout}
	}
	return &Doctor{cfg: cfg, hc: hc, now: cfg.Now}, nil
}

// Run executes every diagnostic in parallel and returns an aggregated
// report. Per-service health checks fan out concurrently because they
// independently hit different microservices; the auth + tenant probes run
// after the org-service health probe completes (they share a service
// dependency).
func (d *Doctor) Run(ctx context.Context) *Report {
	start := d.now()
	rep := &Report{
		Target: d.cfg.Target.Name,
	}

	type taskOut struct {
		idx int
		res CheckResult
	}

	probeOrder := aforo.AllProbeServices
	out := make(chan taskOut, len(probeOrder))

	var wg sync.WaitGroup
	for i, svc := range probeOrder {
		i, svc := i, svc
		wg.Add(1)
		go func() {
			defer wg.Done()
			out <- taskOut{idx: i, res: d.probeService(ctx, svc)}
		}()
	}
	wg.Wait()
	close(out)

	results := make([]CheckResult, len(probeOrder))
	for o := range out {
		results[o.idx] = o.res
	}
	rep.Checks = append(rep.Checks, results...)

	// Auth + tenant existence run sequentially after service health is
	// done — they depend on org-service responding to authenticated calls.
	rep.Checks = append(rep.Checks, d.probeAuth(ctx))
	rep.Checks = append(rep.Checks, d.probeTenantExistence(ctx))

	// Infra dependencies: parsed out of /actuator/health components on the
	// services that expose them. We only emit a check for an infra
	// component if at least one service reported it — otherwise we'd
	// false-FAIL on a target that doesn't expose component health.
	rep.Checks = append(rep.Checks, d.summarizeInfraComponents(rep.Checks)...)

	rep.Elapsed = d.now().Sub(start)
	rep.Overall = StatusOK
	if rep.HasCritical() {
		rep.Overall = StatusFail
	}
	return rep
}

// probeService hits the actuator health endpoint and classifies the
// response. ai-service uses a different probe path because it lacks a
// Spring Boot actuator surface.
func (d *Doctor) probeService(ctx context.Context, svc aforo.Service) CheckResult {
	res := CheckResult{
		Name:     fmt.Sprintf("service:%s", svc),
		Service:  svc,
		Severity: severityFor(svc),
	}

	base, err := d.cfg.Target.URL(svc)
	if err != nil || base == "" {
		res.Status = StatusSkip
		res.Detail = "no URL configured for this service in target"
		res.Remedy = fmt.Sprintf("configure %s for target %s, or use --target=local", svc, d.cfg.Target.Name)
		return res
	}

	probe := healthProbeFor(svc, base)
	res.URL = probe

	t0 := d.now()
	body, code, err := d.httpGet(ctx, probe)
	res.Duration = d.now().Sub(t0)

	if err != nil {
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("transport error: %v", err)
		res.Remedy = remedyForUnreachable(svc, base)
		return res
	}
	if code >= 500 {
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("HTTP %d from %s", code, probe)
		res.Remedy = fmt.Sprintf("inspect logs for %s — service is up but unhealthy", svc)
		return res
	}
	if code == http.StatusNotFound {
		// ai-service: no actuator wired; 404 on /actuator/health is the
		// expected baseline. We re-probe the canonical liveness path.
		if svc == aforo.ServiceAIService {
			res.Status = StatusOK
			res.Detail = "reachable (no actuator)"
			return res
		}
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("HTTP 404 from %s — actuator likely disabled", probe)
		res.Remedy = fmt.Sprintf("ensure management.endpoints.web.exposure.include=health on %s", svc)
		return res
	}
	if code >= 400 {
		// Auth-protected actuators return 401/403 — that still proves
		// reachability, which is what doctor cares about.
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			res.Status = StatusOK
			res.Detail = fmt.Sprintf("reachable (HTTP %d — actuator is auth-protected)", code)
			return res
		}
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("HTTP %d from %s", code, probe)
		res.Remedy = fmt.Sprintf("inspect %s — unexpected actuator status", svc)
		return res
	}

	// 200 OK — parse status field if present and capture component map.
	parsed := parseActuator(body)
	res.components = parsed.components
	if parsed.status != "" && !strings.EqualFold(parsed.status, "UP") {
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("actuator reports %s", parsed.status)
		res.Remedy = fmt.Sprintf("inspect /actuator/health on %s — components: %s", svc, strings.Join(parsed.failingComponents, ", "))
		return res
	}
	res.Status = StatusOK
	if parsed.status != "" {
		res.Detail = "UP"
	} else {
		res.Detail = "reachable"
	}
	return res
}

// probeAuth verifies the bearer token works against the organization
// service. The cheapest authenticated endpoint that exists across every
// environment is the same internal tenants list the seed harness uses —
// 200 means we're authorized, 401 means the token is bad, anything else
// is a transport oddity.
func (d *Doctor) probeAuth(ctx context.Context) CheckResult {
	res := CheckResult{
		Name:     "auth:bearer-token",
		Service:  aforo.ServiceOrganization,
		Severity: SeverityCritical,
	}
	if d.cfg.BearerToken == "" {
		res.Status = StatusSkip
		res.Detail = "no bearer token supplied"
		res.Remedy = "export AFORO_ADMIN_TOKEN=<jwt> before running doctor or e2e"
		return res
	}

	url, err := d.cfg.Target.Path(aforo.ServiceOrganization, aforo.PathInternalTenants)
	if err != nil {
		res.Status = StatusSkip
		res.Detail = fmt.Sprintf("organization-service URL unavailable: %v", err)
		return res
	}
	res.URL = url

	t0 := d.now()
	_, code, err := d.httpGetAuth(ctx, url, d.cfg.BearerToken)
	res.Duration = d.now().Sub(t0)

	switch {
	case err != nil:
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("transport error: %v", err)
		res.Remedy = remedyForUnreachable(aforo.ServiceOrganization, url)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("HTTP %d — token rejected", code)
		res.Remedy = "verify AFORO_ADMIN_TOKEN is current and not expired"
	case code >= 500:
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("HTTP %d on auth probe", code)
		res.Remedy = "organization-service responded with 5xx; check service logs"
	case code >= 400:
		// 404 here is unusual but doesn't mean auth is broken — the path
		// may just have changed. Treat as warning.
		res.Status = StatusFail
		res.Severity = SeverityWarning
		res.Detail = fmt.Sprintf("HTTP %d on auth probe (path may have changed)", code)
		res.Remedy = "ensure /api/v1/internal/tenants is exposed on organization-service"
	default:
		res.Status = StatusOK
		res.Detail = fmt.Sprintf("HTTP %d", code)
	}
	return res
}

// probeTenantExistence checks whether at least one tenant exists in the
// system. The seed harness creates per-archetype tenants in scope of the
// scenario, but the platform requires a baseline tenant to anchor most
// admin operations. If none exist, we surface a remedy pointing at the
// Control Tower provisioning flow.
func (d *Doctor) probeTenantExistence(ctx context.Context) CheckResult {
	res := CheckResult{
		Name:     "auth:tenant-bootstrap",
		Service:  aforo.ServiceOrganization,
		Severity: SeverityWarning,
	}
	if d.cfg.BearerToken == "" {
		res.Status = StatusSkip
		res.Detail = "no bearer token supplied — cannot list tenants"
		return res
	}
	url, err := d.cfg.Target.Path(aforo.ServiceOrganization, aforo.PathInternalTenants)
	if err != nil {
		res.Status = StatusSkip
		res.Detail = fmt.Sprintf("organization-service URL unavailable: %v", err)
		return res
	}
	res.URL = url

	t0 := d.now()
	body, code, err := d.httpGetAuth(ctx, url, d.cfg.BearerToken)
	res.Duration = d.now().Sub(t0)

	if err != nil || code >= 400 {
		res.Status = StatusSkip
		res.Detail = "could not list tenants — covered by auth check"
		return res
	}

	// Cheap pass: count "tenantId" occurrences. Robust enough; we don't
	// care about precise schema, only "more than zero".
	count := strings.Count(body, "\"tenantId\"") + strings.Count(body, "\"tenant_id\"")
	if count == 0 {
		res.Status = StatusOK
		res.Severity = SeverityWarning
		res.Detail = "no tenants exist yet — seed will create them per scenario"
		res.Remedy = "this is fine for fresh installs; e2e will provision per archetype"
		return res
	}
	res.Status = StatusOK
	res.Detail = fmt.Sprintf("%d tenant(s) reachable", count)
	return res
}

// summarizeInfraComponents collapses per-service component maps into one
// row per infra dependency. Reads the components captured during the
// first fan-out (ServiceCheckResult.components) — no second HTTP round.
//
// We assume the operator cares about "PostgreSQL anywhere is down" not
// "PostgreSQL on service X is down" — the latter is implicit in the
// per-service health rows.
func (d *Doctor) summarizeInfraComponents(rows []CheckResult) []CheckResult {
	wantComponents := []string{"db", "kafka", "redis", "clickhouse"}

	componentCounts := make(map[string]int)
	componentDown := make(map[string][]string) // component → service names that reported DOWN

	for _, r := range rows {
		if r.components == nil {
			continue
		}
		for k, v := range r.components {
			low := strings.ToLower(k)
			for _, want := range wantComponents {
				if strings.Contains(low, want) {
					componentCounts[want]++
					if !strings.EqualFold(v, "UP") {
						componentDown[want] = append(componentDown[want], string(r.Service))
					}
				}
			}
		}
	}

	out := make([]CheckResult, 0, len(wantComponents))
	for _, c := range wantComponents {
		if componentCounts[c] == 0 {
			// No service reported this dependency; skip silently.
			continue
		}
		row := CheckResult{
			Name:     fmt.Sprintf("infra:%s", c),
			Severity: SeverityCritical,
		}
		if len(componentDown[c]) > 0 {
			sort.Strings(componentDown[c])
			row.Status = StatusFail
			row.Detail = fmt.Sprintf("DOWN reported by: %s", strings.Join(componentDown[c], ", "))
			row.Remedy = remedyForInfra(c)
		} else {
			row.Status = StatusOK
			row.Detail = fmt.Sprintf("UP across %d service(s)", componentCounts[c])
		}
		out = append(out, row)
	}
	return out
}

// httpGet issues a GET and returns body + status. Body is read up to 64
// KiB (more than enough for an actuator response).
func (d *Doctor) httpGet(ctx context.Context, url string) (string, int, error) {
	return d.httpGetAuth(ctx, url, "")
}

func (d *Doctor) httpGetAuth(ctx context.Context, url, token string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PerCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	const cap = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, cap))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

// healthProbeFor returns the URL doctor probes for liveness on a service.
// All Spring Boot services use /actuator/health. ai-service has no
// actuator today, so we hit /v1/healthz which is the closest analogue;
// the service returns 404 on that path which doctor treats as "reachable
// (no actuator)" rather than failure.
func healthProbeFor(svc aforo.Service, base string) string {
	switch svc {
	case aforo.ServiceAIService:
		return strings.TrimRight(base, "/") + "/healthz"
	default:
		return strings.TrimRight(base, "/") + "/actuator/health"
	}
}

// severityFor classifies how strict doctor is about a service. Today only
// ai-service is WARNING — every other service is required for the e2e
// flow's seed/run/validate stages.
func severityFor(svc aforo.Service) Severity {
	if svc == aforo.ServiceAIService {
		return SeverityWarning
	}
	return SeverityCritical
}

// remedyForUnreachable returns an actionable next step when the doctor
// can't reach a service. Local target gets the docker-compose hint;
// staging/prod gets a network-routing hint.
func remedyForUnreachable(svc aforo.Service, url string) string {
	if strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") {
		return fmt.Sprintf("%s not reachable at %s. Run cd aforo-nextgen-docker && docker-compose up -d first.",
			svc, url)
	}
	return fmt.Sprintf("%s not reachable at %s. Verify VPN/network access and that the service is deployed.", svc, url)
}

// remedyForInfra suggests where to look when a platform infra component
// reports DOWN. Each remedy points at the docker-compose service name so
// the operator can run a targeted command.
func remedyForInfra(component string) string {
	switch component {
	case "db":
		return "PostgreSQL DOWN. Try: docker-compose -f aforo-nextgen-docker/docker-compose.yml restart postgres"
	case "kafka":
		return "Kafka DOWN. Try: docker-compose -f aforo-nextgen-docker/docker-compose.yml restart kafka"
	case "redis":
		return "Redis DOWN. Try: docker-compose -f aforo-nextgen-docker/docker-compose.yml restart redis"
	case "clickhouse":
		return "ClickHouse DOWN. Try: docker-compose -f aforo-nextgen-docker/docker-compose.yml restart clickhouse"
	default:
		return fmt.Sprintf("infra component %q DOWN — inspect docker-compose stack", component)
	}
}

// actuatorParsed is the slice of /actuator/health we care about.
type actuatorParsed struct {
	status            string
	components        map[string]string // component name → status (UP/DOWN/OUT_OF_SERVICE)
	failingComponents []string
}

// parseActuator extracts what we need from a Spring Boot actuator
// response. Returns zero values on parse failure — callers must treat the
// empty status as "service is reachable, status not asserted".
func parseActuator(body string) actuatorParsed {
	var p actuatorParsed
	if body == "" {
		return p
	}
	type rawComponent struct {
		Status string `json:"status"`
	}
	var raw struct {
		Status     string                  `json:"status"`
		Components map[string]rawComponent `json:"components,omitempty"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return p
	}
	p.status = raw.Status
	if len(raw.Components) > 0 {
		p.components = make(map[string]string, len(raw.Components))
		for k, v := range raw.Components {
			p.components[k] = v.Status
			if !strings.EqualFold(v.Status, "UP") {
				p.failingComponents = append(p.failingComponents, k)
			}
		}
	}
	return p
}
