package validate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// Inputs is the bundle the orchestrator needs. Constructed by the CLI from
// flags and disk artifacts.
//
// RunOutputDir is the source of truth for what was sent — run.json,
// events.jsonl, scenario.yaml live there. Manifest is what was seeded.
// Backend is how to query live infra; pass NewOfflineBackend(run) when
// no infra is reachable.
type Inputs struct {
	RunOutputDir   string
	Manifest       *seed.Manifest
	Run            *runner.RunResult
	Scenario       *scenario.Scenario
	Backend        BackendClient
	IncludeBilling bool
	TolerancePct   float64 // billing-amount drift tolerance (0.001 default = 0.1%)
	OnlyChecks     []string
	OnlyArchetypes []string

	// Session 6 — lifecycle transition log loaded from <RunOutputDir>/transitions.jsonl.
	// nil/empty when the run was a vanilla `run` (no agent), in which case the
	// lifecycle checks SKIP cleanly.
	Transitions []lifecycle.TransitionRecord
}

// Validator is the orchestrator. Construct with New, then call Run.
//
// The struct is the lifecycle: NewBackend → Run() returns ValidationReport.
// Tests inject a fake Backend; production passes a live or offline one.
type Validator struct {
	in *Inputs
}

// New returns a Validator with cleaned-up inputs (defaults applied).
func New(in *Inputs) (*Validator, error) {
	if in == nil {
		return nil, errors.New("validate: nil inputs")
	}
	if in.Run == nil {
		return nil, errors.New("validate: run result is nil — load run.json first")
	}
	if in.Manifest == nil {
		return nil, errors.New("validate: manifest is nil — load seed.json first")
	}
	if in.Scenario == nil {
		return nil, errors.New("validate: scenario is nil — load scenario.yaml first")
	}
	if in.Backend == nil {
		in.Backend = NewOfflineBackend(in.Run)
	}
	if in.TolerancePct <= 0 {
		in.TolerancePct = 0.001
	}
	return &Validator{in: in}, nil
}

// Run executes the requested checks (filtered by --checks if set) and
// returns the report. Never returns nil on a successful structural pass —
// even fully-skipped runs return a report with the SKIP statuses recorded.
func (v *Validator) Run(ctx context.Context) (*ValidationReport, error) {
	report := &ValidationReport{
		ReportVersion: ReportVersion,
		RunID:         v.in.Run.RunID,
		Scenario:      v.in.Run.ScenarioName,
		Target:        v.in.Run.Target,
		StartedAt:     time.Now().UTC(),
	}

	wanted := wantedChecks(v.in.OnlyChecks)

	for _, name := range AllChecks {
		if !wanted[name] {
			continue
		}
		select {
		case <-ctx.Done():
			report.Checks = append(report.Checks,
				NewCheckResult(name).Skip("context cancelled before check ran"))
			continue
		default:
		}
		res := v.dispatch(ctx, name)
		report.Checks = append(report.Checks, res)
	}

	report.Finalize()
	return report, nil
}

// dispatch runs the single named check.
//
// Each check is its own method so adding one is mechanical: write a
// runCheckXxx and add a case below + add to AllChecks.
func (v *Validator) dispatch(ctx context.Context, name string) *CheckResult {
	switch name {
	case CheckEventCount:
		return v.runEventCount(ctx)
	case CheckCrossTenant:
		return v.runCrossTenantLeakage(ctx)
	case CheckHierarchy:
		return v.runBillingHierarchy(ctx)
	case CheckCacheHitRatio:
		return v.runCacheHitRatio(ctx)
	case CheckBillingMatch:
		return v.runBillingMatch(ctx)
	case CheckNegativePaths:
		return v.runNegativePathCorrectness(ctx)
	case CheckInvariants:
		return v.runInvariants(ctx)
	case CheckBillRunConcurrency:
		return v.runBillRunConcurrency(ctx)
	case CheckLifecycleCorrectness:
		return v.runLifecycleCorrectness(ctx)
	case CheckStateMachineInvariants:
		return v.runStateMachineInvariants(ctx)
	case CheckLifecycleVsBillRun:
		return v.runLifecycleVsBillRun(ctx)
	default:
		return NewCheckResult(name).Skip("unknown check name %q", name)
	}
}

// wantedChecks turns a --checks comma-list into a set. Empty input means
// "all checks" — represented by every name set to true.
func wantedChecks(only []string) map[string]bool {
	if len(only) == 0 {
		out := make(map[string]bool, len(AllChecks))
		for _, n := range AllChecks {
			out[n] = true
		}
		return out
	}
	out := map[string]bool{}
	for _, raw := range only {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		out[raw] = true
	}
	return out
}

// LoadFromRunOutput loads run.json, events.jsonl summary, and scenario.yaml
// from a run output directory. Returns the populated structs. Used by the
// CLI to construct Inputs.
func LoadFromRunOutput(dir string) (*runner.RunResult, []byte, *scenario.Document, error) {
	runPath := filepath.Join(dir, "run.json")
	runData, err := os.ReadFile(runPath) // #nosec G304 — caller-controlled
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", runPath, err)
	}
	var rr runner.RunResult
	if err := json.Unmarshal(runData, &rr); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", runPath, err)
	}

	scenarioPath := filepath.Join(dir, "scenario.yaml")
	scenarioBytes, err := os.ReadFile(scenarioPath) // #nosec G304
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", scenarioPath, err)
	}
	doc, err := scenario.LoadFromBytes(scenarioBytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", scenarioPath, err)
	}
	doc.Path = scenarioPath

	// events.jsonl is large + optional — load only when needed.
	return &rr, scenarioBytes, doc, nil
}

// ArchetypeMatches returns true if archetype is in the OnlyArchetypes filter
// or if no filter was set (empty = match all).
func (v *Validator) ArchetypeMatches(archetype string) bool {
	if len(v.in.OnlyArchetypes) == 0 {
		return true
	}
	for _, a := range v.in.OnlyArchetypes {
		if a == archetype {
			return true
		}
	}
	return false
}

// allTenantIDs returns every tenant id from the manifest, sorted (stable
// JSON output across runs).
func (v *Validator) allTenantIDs() []string {
	ids := make([]string, 0, len(v.in.Manifest.Tenants))
	for _, t := range v.in.Manifest.Tenants {
		ids = append(ids, t.TenantID)
	}
	sort.Strings(ids)
	return ids
}

// runWindow is the [start, end] time window of the run, used for backend
// queries. Pulled from RunResult.
func (v *Validator) runWindow() TimeWindow {
	return TimeWindow{Start: v.in.Run.StartedAt, End: v.in.Run.StoppedAt}
}
