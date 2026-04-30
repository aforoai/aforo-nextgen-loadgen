// Package validate is the Session 5 oracle. It reads a completed run's output
// (run.json + events.jsonl + scenario.yaml) plus the seed manifest, optionally
// queries the live Aforo backend (ClickHouse, PostgreSQL, billing-platform,
// Redis), and emits a ValidationReport with PASS/FAIL/SKIP per check.
//
// Two design rules are load-bearing:
//
//  1. Every check is independent — one FAIL never short-circuits another.
//     CI gates on the overall summary, but humans need every signal at once.
//  2. Backend calls are routed through a BackendClient interface. The
//     OfflineBackend implementation derives everything it can from run.json,
//     so checks 1, 6, 7 work without infra and ci-smoke validate exits 0
//     in pure-CI mode.
package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ReportVersion is bumped when the validation.json schema changes. Downstream
// dashboards key off this — never mutate an existing major shape, only add.
const ReportVersion = 2

// CheckStatus is the per-check verdict. SKIP is reserved for "could not run"
// (backend unreachable, opt-in flag off, dependency missing) — it MUST NOT
// be used to hide a real failure.
type CheckStatus string

const (
	StatusPass CheckStatus = "PASS"
	StatusFail CheckStatus = "FAIL"
	StatusSkip CheckStatus = "SKIP"
)

// CheckName enumerates the fixed set of checks. Stable strings — they appear
// in JSON output, --checks filter, and HTML headings.
const (
	CheckEventCount         = "event_count_per_tenant"
	CheckCrossTenant        = "cross_tenant_leakage"
	CheckHierarchy          = "billing_hierarchy_resolution"
	CheckCacheHitRatio      = "cache_hit_ratio"
	CheckBillingMatch       = "per_archetype_billing_match"
	CheckNegativePaths      = "negative_path_correctness"
	CheckInvariants         = "property_based_invariants"
	CheckBillRunConcurrency = "bill_run_concurrency"
)

// AllChecks is the canonical iteration order. The orchestrator runs them in
// this order and writes them to the report in this order so diffs across
// runs are stable.
var AllChecks = []string{
	CheckEventCount,
	CheckCrossTenant,
	CheckHierarchy,
	CheckCacheHitRatio,
	CheckBillingMatch,
	CheckNegativePaths,
	CheckInvariants,
	CheckBillRunConcurrency,
}

// CheckResult is a single check's verdict + reasons. Details is a free-form
// map so each check can report what's relevant; HTML rendering knows the
// canonical keys per check name.
type CheckResult struct {
	Name      string         `json:"name"`
	Status    CheckStatus    `json:"status"`
	StartedAt time.Time      `json:"started_at"`
	EndedAt   time.Time      `json:"ended_at"`
	Reason    string         `json:"reason,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Pass marks the result as PASS and stamps the end time.
func (c *CheckResult) Pass() *CheckResult {
	c.Status = StatusPass
	c.EndedAt = time.Now().UTC()
	return c
}

// Fail marks the result as FAIL with a human reason and stamps the end time.
// The reason is what shows up in CI logs; keep it actionable.
func (c *CheckResult) Fail(reason string, args ...any) *CheckResult {
	c.Status = StatusFail
	c.Reason = fmt.Sprintf(reason, args...)
	c.EndedAt = time.Now().UTC()
	return c
}

// Skip marks the result as SKIP with a reason. SKIP is not a pass — the
// summary counts it separately and CI may still gate on it.
func (c *CheckResult) Skip(reason string, args ...any) *CheckResult {
	c.Status = StatusSkip
	c.Reason = fmt.Sprintf(reason, args...)
	c.EndedAt = time.Now().UTC()
	return c
}

// Set inserts a key in Details, allocating the map lazily.
func (c *CheckResult) Set(key string, value any) *CheckResult {
	if c.Details == nil {
		c.Details = map[string]any{}
	}
	c.Details[key] = value
	return c
}

// NewCheckResult is the constructor — stamps StartedAt and seeds Details.
func NewCheckResult(name string) *CheckResult {
	return &CheckResult{
		Name:      name,
		Status:    StatusSkip, // overwritten by Pass/Fail/Skip
		StartedAt: time.Now().UTC(),
		Details:   map[string]any{},
	}
}

// Summary is the report's roll-up. Overall is PASS only if zero FAILs;
// SKIPs do not break the overall verdict but are surfaced in their own
// counter. CI exits non-zero on any FAIL.
type Summary struct {
	Total   int         `json:"total"`
	Passed  int         `json:"passed"`
	Failed  int         `json:"failed"`
	Skipped int         `json:"skipped"`
	Overall CheckStatus `json:"overall"`
}

// ValidationReport is the on-disk artifact written to <run-output>/validation.json.
type ValidationReport struct {
	ReportVersion int            `json:"report_version"`
	RunID         string         `json:"run_id"`
	Scenario      string         `json:"scenario"`
	Target        string         `json:"target"`
	StartedAt     time.Time      `json:"started_at"`
	EndedAt       time.Time      `json:"ended_at"`
	Checks        []*CheckResult `json:"checks"`
	Summary       Summary        `json:"summary"`
}

// Finalize sorts checks by their canonical order, then computes the summary.
// Mutates the receiver. Idempotent — safe to call multiple times.
func (r *ValidationReport) Finalize() {
	order := map[string]int{}
	for i, name := range AllChecks {
		order[name] = i
	}
	sort.SliceStable(r.Checks, func(i, j int) bool {
		oi, oj := order[r.Checks[i].Name], order[r.Checks[j].Name]
		return oi < oj
	})

	r.Summary = Summary{Total: len(r.Checks)}
	for _, c := range r.Checks {
		switch c.Status {
		case StatusPass:
			r.Summary.Passed++
		case StatusFail:
			r.Summary.Failed++
		case StatusSkip:
			r.Summary.Skipped++
		}
	}
	r.EndedAt = time.Now().UTC()
	if r.Summary.Failed > 0 {
		r.Summary.Overall = StatusFail
	} else if r.Summary.Passed == 0 {
		r.Summary.Overall = StatusSkip
	} else {
		r.Summary.Overall = StatusPass
	}
}

// Save writes the report as pretty-printed JSON to <dir>/validation.json.
// Returns the absolute path of the written file.
func (r *ValidationReport) Save(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("validate: output dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("validate: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "validation.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("validate: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G306 — readable artifact
		return "", fmt.Errorf("validate: write %s: %w", path, err)
	}
	return path, nil
}

// LoadValidationReport reads a previously-written validation.json. Used by
// the report subcommand and by tests.
func LoadValidationReport(path string) (*ValidationReport, error) {
	data, err := os.ReadFile(path) // #nosec G304 — caller-controlled path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var r ValidationReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &r, nil
}
