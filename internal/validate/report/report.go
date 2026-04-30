// Package report renders self-contained HTML reports of a load run + its
// validation pass. "Self-contained" is load-bearing: the HTML must work
// offline, no Google Fonts, no CDN. Operators forward these to slack
// channels or attach them to PR comments — they MUST render anywhere.
//
// The renderer accepts a runner.RunResult + ValidationReport and writes
// <run-output>/report.html plus a small per-archetype CSV alongside.
package report

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

//go:embed assets/*.html assets/*.css
var assets embed.FS

// View is the template input — what report.html.tmpl renders against.
//
// The template is intentionally read-only. Compute everything HTML-related
// on the Go side; the template just renders. This keeps the report
// reproducible across runs (same data → byte-identical HTML).
type View struct {
	Title            string
	GeneratedAt      string
	Scenario         string
	Target           string
	RunID            string
	Duration         string
	TargetTPS        int
	StartedAt        string
	EndedAt          string
	Summary          validate.Summary
	OverallClass     string
	OverallText      string
	RunStats         []KV
	Latency          []KV
	NegativeRows     []NegativeRow
	ArchetypeRows    []ArchetypeRow
	TenantRows       []TenantRow
	BillingRows      []BillingRow
	Violations       []ViolationRow
	StalePositives   int64
	HasStaleProbe    bool
	Checks           []*validate.CheckResult
	LifecycleRows    []LifecycleRow
	StateMachineRows []StateMachineRow
	CSS              template.CSS
}

// LifecycleRow is one (transition kind × outcome) row in the HTML report.
type LifecycleRow struct {
	Kind          string
	Total         int
	OK            int
	Failures      int
	StateMatch    int
	StateMismatch int
}

// StateMachineRow is one violation row from Check 10.
type StateMachineRow struct {
	Index          int
	SubscriptionID string
	Transition     string
	FromState      string
	ExpectedTo     string
	Reason         string
}

// KV is a label / value pair used in summary tables.
type KV struct{ K, V string }

// NegativeRow is one negative-path category in the HTML table.
type NegativeRow struct {
	Category string
	Expected int64
	Actual   int64
	Match    bool
	Reason   string
}

// ArchetypeRow is one row in the per-archetype event-share table.
type ArchetypeRow struct {
	Name       string
	EventCount int64
	Pct        string
}

// TenantRow is one row in the per-tenant table (top-N).
type TenantRow struct {
	TenantID string
	Events   int64
	Pct      string
}

// BillingRow is one archetype × billing-match row.
type BillingRow struct {
	Archetype       string
	PricingModel    string
	BillingMode     string
	CustomersTested int
	AllMatch        bool
	MaxDriftPct     float64
}

// ViolationRow is one Check 7 violation surfaced in the report.
type ViolationRow struct {
	Invariant string
	Message   string
	Index     int
	Model     string
	Mode      string
	Events    int64
}

// Render writes <out>/report.html. The CSS is embedded inline so the
// resulting file is portable.
func Render(out string, run *runner.RunResult, valReport *validate.ValidationReport) (string, error) {
	if run == nil {
		return "", fmt.Errorf("report: nil run result")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", fmt.Errorf("report: mkdir %s: %w", out, err)
	}

	tplData, err := assets.ReadFile("assets/report.html")
	if err != nil {
		return "", fmt.Errorf("report: load template: %w", err)
	}
	tpl, err := template.New("report").Parse(string(tplData))
	if err != nil {
		return "", fmt.Errorf("report: parse template: %w", err)
	}

	cssData, err := assets.ReadFile("assets/report.css")
	if err != nil {
		return "", fmt.Errorf("report: load css: %w", err)
	}

	view := buildView(run, valReport, string(cssData))

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, view); err != nil {
		return "", fmt.Errorf("report: render: %w", err)
	}

	path := filepath.Join(out, "report.html")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil { // #nosec G306 — readable artifact
		return "", fmt.Errorf("report: write %s: %w", path, err)
	}
	return path, nil
}

// buildView is the data-prep step. Pure function — same inputs always
// produce the same View struct.
func buildView(run *runner.RunResult, valReport *validate.ValidationReport, css string) View {
	v := View{
		Title:       "aforo-loadgen run report",
		Scenario:    run.ScenarioName,
		Target:      run.Target,
		RunID:       run.RunID,
		Duration:    run.Duration.String(),
		TargetTPS:   run.TargetTPS,
		StartedAt:   run.StartedAt.Format(time.RFC3339),
		EndedAt:     run.StoppedAt.Format(time.RFC3339),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		CSS:         template.CSS(css),
	}
	if valReport != nil {
		v.Summary = valReport.Summary
		v.Checks = valReport.Checks
		v.OverallClass = string(valReport.Summary.Overall)
		v.OverallText = string(valReport.Summary.Overall)
	} else {
		v.OverallClass = "SKIP"
		v.OverallText = "no validation"
	}

	v.RunStats = []KV{
		{"events generated", fmt.Sprintf("%d", run.EventsGenerated)},
		{"events submitted", fmt.Sprintf("%d", run.EventsSubmitted)},
		{"events succeeded", fmt.Sprintf("%d", run.EventsSucceeded)},
		{"client errors (4xx)", fmt.Sprintf("%d", run.ClientErrors)},
		{"server errors (5xx)", fmt.Sprintf("%d", run.ServerErrors)},
		{"transport failures", fmt.Sprintf("%d", run.TransportFailures)},
		{"circuit-open skipped", fmt.Sprintf("%d", run.CircuitOpenSkipped)},
		{"expected failures", fmt.Sprintf("%d", run.ExpectedFailures)},
	}
	v.Latency = []KV{
		{"p50", fmt.Sprintf("%.1f ms", run.LatencyP50ms)},
		{"p90", fmt.Sprintf("%.1f ms", run.LatencyP90ms)},
		{"p99", fmt.Sprintf("%.1f ms", run.LatencyP99ms)},
		{"max", fmt.Sprintf("%.1f ms", run.LatencyMaxMs)},
	}

	// Negative paths — pull from validation report if present.
	if valReport != nil {
		for _, c := range valReport.Checks {
			if c.Name == validate.CheckNegativePaths {
				if cats, ok := c.Details["by_category"].(map[string]*validate.CategoryReport); ok {
					v.NegativeRows = negativeRowsFromMap(cats)
				} else if jsonish, ok := c.Details["by_category"].(map[string]any); ok {
					v.NegativeRows = negativeRowsFromJSON(jsonish)
				}
				if sp, ok := c.Details["by_category"].(map[string]*validate.CategoryReport); ok {
					if stale, ok := sp[string("stale_key")]; ok {
						v.StalePositives = stale.FalsePos
						v.HasStaleProbe = stale.FalsePos >= 0
					}
				}
			}
			if c.Name == validate.CheckBillingMatch {
				v.BillingRows = billingRowsFromCheck(c)
			}
			if c.Name == validate.CheckInvariants {
				v.Violations = violationRowsFromCheck(c)
			}
			if c.Name == validate.CheckLifecycleCorrectness {
				v.LifecycleRows = lifecycleRowsFromCheck(c)
			}
			if c.Name == validate.CheckStateMachineInvariants {
				v.StateMachineRows = stateMachineRowsFromCheck(c)
			}
		}
	}

	// Per-archetype + per-tenant — from run.json directly.
	v.ArchetypeRows = topArchetypes(run.PerArchetype, run.EventsGenerated)
	v.TenantRows = topTenants(run.PerTenant, run.EventsGenerated, 25)

	return v
}

func negativeRowsFromMap(m map[string]*validate.CategoryReport) []NegativeRow {
	rows := make([]NegativeRow, 0, len(m))
	for k, c := range m {
		rows = append(rows, NegativeRow{
			Category: k,
			Expected: c.Expected,
			Actual:   c.Actual,
			Match:    c.Match,
			Reason:   c.Reason,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Category < rows[j].Category })
	return rows
}

func negativeRowsFromJSON(m map[string]any) []NegativeRow {
	rows := make([]NegativeRow, 0, len(m))
	for k, raw := range m {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		row := NegativeRow{Category: k}
		if e, ok := entry["expected"].(float64); ok {
			row.Expected = int64(e)
		}
		if a, ok := entry["actual"].(float64); ok {
			row.Actual = int64(a)
		}
		if m, ok := entry["match"].(bool); ok {
			row.Match = m
		}
		if r, ok := entry["reason"].(string); ok {
			row.Reason = r
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Category < rows[j].Category })
	return rows
}

func billingRowsFromCheck(c *validate.CheckResult) []BillingRow {
	out := []BillingRow{}
	raw, ok := c.Details["by_archetype"]
	if !ok {
		return out
	}
	// raw is []archOutcome (custom struct); marshal/unmarshal via JSON for
	// portability when loaded from disk.
	data, err := json.Marshal(raw)
	if err != nil {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return out
	}
	for _, m := range arr {
		row := BillingRow{}
		if v, ok := m["archetype"].(string); ok {
			row.Archetype = v
		}
		if v, ok := m["pricing_model"].(string); ok {
			row.PricingModel = v
		}
		if v, ok := m["billing_mode"].(string); ok {
			row.BillingMode = v
		}
		if v, ok := m["customers_tested"].(float64); ok {
			row.CustomersTested = int(v)
		}
		if v, ok := m["all_match"].(bool); ok {
			row.AllMatch = v
		}
		if v, ok := m["max_drift_pct"].(float64); ok {
			row.MaxDriftPct = v
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Archetype < out[j].Archetype })
	return out
}

func lifecycleRowsFromCheck(c *validate.CheckResult) []LifecycleRow {
	out := []LifecycleRow{}
	raw, ok := c.Details["by_kind"]
	if !ok {
		return out
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return out
	}
	for _, m := range arr {
		row := LifecycleRow{}
		if v, ok := m["kind"].(string); ok {
			row.Kind = v
		}
		if v, ok := m["total"].(float64); ok {
			row.Total = int(v)
		}
		if v, ok := m["ok"].(float64); ok {
			row.OK = int(v)
		}
		if v, ok := m["failures"].(float64); ok {
			row.Failures = int(v)
		}
		if v, ok := m["state_match"].(float64); ok {
			row.StateMatch = int(v)
		}
		if v, ok := m["state_mismatch"].(float64); ok {
			row.StateMismatch = int(v)
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func stateMachineRowsFromCheck(c *validate.CheckResult) []StateMachineRow {
	out := []StateMachineRow{}
	raw, ok := c.Details["violations"]
	if !ok {
		return out
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return out
	}
	for _, m := range arr {
		row := StateMachineRow{}
		if v, ok := m["index"].(float64); ok {
			row.Index = int(v)
		}
		if v, ok := m["subscription_id"].(string); ok {
			row.SubscriptionID = v
		}
		if v, ok := m["transition"].(string); ok {
			row.Transition = v
		}
		if v, ok := m["from_state"].(string); ok {
			row.FromState = v
		}
		if v, ok := m["expected_to"].(string); ok {
			row.ExpectedTo = v
		}
		if v, ok := m["reason"].(string); ok {
			row.Reason = v
		}
		out = append(out, row)
	}
	return out
}

func violationRowsFromCheck(c *validate.CheckResult) []ViolationRow {
	out := []ViolationRow{}
	raw, ok := c.Details["violations"]
	if !ok {
		return out
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return out
	}
	for _, m := range arr {
		row := ViolationRow{}
		if v, ok := m["invariant"].(string); ok {
			row.Invariant = v
		}
		if v, ok := m["message"].(string); ok {
			row.Message = v
		}
		if s, ok := m["sample"].(map[string]any); ok {
			if v, ok := s["Index"].(float64); ok {
				row.Index = int(v)
			}
			if v, ok := s["Model"].(string); ok {
				row.Model = v
			}
			if v, ok := s["Mode"].(string); ok {
				row.Mode = v
			}
			if v, ok := s["Events"].(float64); ok {
				row.Events = int64(v)
			}
		}
		out = append(out, row)
	}
	return out
}

func topArchetypes(per map[string]int64, total int64) []ArchetypeRow {
	rows := make([]ArchetypeRow, 0, len(per))
	for k, v := range per {
		pct := "0.0%"
		if total > 0 {
			pct = fmt.Sprintf("%.1f%%", float64(v)/float64(total)*100)
		}
		rows = append(rows, ArchetypeRow{Name: k, EventCount: v, Pct: pct})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].EventCount > rows[j].EventCount })
	return rows
}

func topTenants(per map[string]int64, total int64, top int) []TenantRow {
	rows := make([]TenantRow, 0, len(per))
	for k, v := range per {
		pct := "0.0%"
		if total > 0 {
			pct = fmt.Sprintf("%.1f%%", float64(v)/float64(total)*100)
		}
		rows = append(rows, TenantRow{TenantID: k, Events: v, Pct: pct})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Events > rows[j].Events })
	if top > 0 && len(rows) > top {
		rows = rows[:top]
	}
	return rows
}
