package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/doctor"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

// fakeRunner implements StageRunner with operator-controlled behavior.
// Records the order each stage was invoked so tests can assert sequencing
// without standing up real services. Methods are concurrency-safe — the
// orchestrator runs RunLoad and Lifecycle in parallel goroutines.
type fakeRunner struct {
	mu sync.Mutex

	doctorReport *doctor.Report
	doctorErr    error

	seedRes *seed.RunResult
	seedErr error

	runRes *runner.RunResult
	runErr error

	lcSnap lifecycle.Snapshot
	lcErr  error

	valReport *validate.ValidationReport
	valErr    error

	reportPath string
	reportErr  error

	cleanErr error

	calls atomic.Value // []string — read after run via Calls()
	// runDelay simulates run-stage runtime; lifecycle gets the same delay
	// so orchestrator's parallel-cancel logic is exercised.
	runDelay time.Duration
}

func (f *fakeRunner) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev, _ := f.calls.Load().([]string)
	f.calls.Store(append(append([]string{}, prev...), name))
}

func (f *fakeRunner) Calls() []string {
	prev, _ := f.calls.Load().([]string)
	return append([]string{}, prev...)
}

func (f *fakeRunner) Doctor(_ context.Context, _ aforo.Target, _ string) (*doctor.Report, error) {
	f.record("doctor")
	return f.doctorReport, f.doctorErr
}

func (f *fakeRunner) Seed(_ context.Context, in SeedInput) (*seed.RunResult, error) {
	f.record("seed")
	// Mirror the real seeder's contract: write a manifest file to
	// disk before returning so subsequent stages (notably clean) can
	// pick it up. We always do this when seedRes.Manifest is non-nil,
	// even if the seed pretends to error — partial provisioning is a
	// real failure mode.
	if f.seedRes != nil && f.seedRes.Manifest != nil && in.ManifestPath != "" {
		if err := os.MkdirAll(filepath.Dir(in.ManifestPath), 0o755); err == nil {
			data, marshalErr := json.Marshal(f.seedRes.Manifest)
			if marshalErr == nil {
				_ = os.WriteFile(in.ManifestPath, data, 0o644)
			}
		}
	}
	return f.seedRes, f.seedErr
}

func (f *fakeRunner) RunLoad(ctx context.Context, _ RunInput) (*runner.RunResult, error) {
	f.record("run")
	if f.runDelay > 0 {
		select {
		case <-time.After(f.runDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.runRes, f.runErr
}

func (f *fakeRunner) Lifecycle(ctx context.Context, _ LifecycleInput) (lifecycle.Snapshot, error) {
	f.record("lifecycle")
	if f.runDelay > 0 {
		// Lifecycle in real orchestrator runs until ctx done; simulate.
		select {
		case <-time.After(f.runDelay):
		case <-ctx.Done():
			// emulate the real agent: drain on cancel and return its
			// snapshot (no error).
			return f.lcSnap, nil
		}
	}
	return f.lcSnap, f.lcErr
}

func (f *fakeRunner) Validate(_ context.Context, _ ValidateInput) (*validate.ValidationReport, error) {
	f.record("validate")
	return f.valReport, f.valErr
}

func (f *fakeRunner) Report(_ context.Context, _ string, _ *runner.RunResult, _ *validate.ValidationReport) (string, error) {
	f.record("report")
	return f.reportPath, f.reportErr
}

func (f *fakeRunner) Clean(_ context.Context, _ CleanInput) error {
	f.record("clean")
	return f.cleanErr
}

// minimalScenario is enough scenario shape to satisfy New() — every test
// uses this except the lifecycle one.
func minimalScenario() *scenario.Scenario {
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "test",
		TargetTPS:     50,
		Duration:      scenario.Duration(time.Second),
	}
}

func minimalSeedResult() *seed.RunResult {
	return &seed.RunResult{
		Manifest: &seed.Manifest{
			RunID: "test-run",
			Summary: seed.ManifestSummary{
				TotalTenants:   1,
				TotalCustomers: 5,
				TotalSubs:      5,
			},
		},
	}
}

func minimalRunResult() *runner.RunResult {
	return &runner.RunResult{
		EventsSubmitted: 100,
		EventsSucceeded: 100,
	}
}

func passingValidation() *validate.ValidationReport {
	r := &validate.ValidationReport{
		Checks: []*validate.CheckResult{
			{Name: "test", Status: validate.StatusPass},
		},
	}
	r.Finalize()
	return r
}

func makeOrch(t *testing.T, fr *fakeRunner, mut func(*Config)) (*Orchestrator, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	stdout := &bytes.Buffer{}
	cfg := Config{
		Scenario:    minimalScenario(),
		Target:      aforo.LocalTarget,
		OutputDir:   filepath.Join(dir, "out"),
		BearerToken: "tok",
		Stdout:      stdout,
		Stderr:      stdout,
		Runner:      fr,
		Now:         func() time.Time { return time.Unix(1714500000, 0).UTC() },
	}
	if mut != nil {
		mut(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, cfg.OutputDir, stdout
}

func TestOrchestrator_HappyPath_ChainsAllStages(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
		reportPath:   "/tmp/report.html",
	}
	o, outDir, _ := makeOrch(t, fr, nil)

	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != StagePass {
		t.Errorf("Overall = %s, want PASS", res.Overall)
	}
	want := []string{"doctor", "seed", "run", "validate", "report", "clean"}
	got := fr.Calls()
	if !sliceContainsAll(got, want) {
		t.Errorf("calls = %v, want all of %v", got, want)
	}
	// e2e.json should be present.
	if _, err := os.Stat(filepath.Join(outDir, "e2e.json")); err != nil {
		t.Errorf("expected e2e.json: %v", err)
	}
}

func TestOrchestrator_DoctorCriticalFailure_AbortsBeforeSeed(t *testing.T) {
	failing := &doctor.Report{
		Overall: doctor.StatusFail,
		Checks: []doctor.CheckResult{
			{Name: "service:catalog", Status: doctor.StatusFail, Severity: doctor.SeverityCritical},
		},
	}
	fr := &fakeRunner{
		doctorReport: failing,
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
	}
	o, _, _ := makeOrch(t, fr, nil)

	res, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from doctor failure; got nil")
	}
	if res.Overall != StageFail {
		t.Errorf("Overall = %s, want FAIL", res.Overall)
	}
	got := fr.Calls()
	for _, name := range got {
		if name == "seed" || name == "run" {
			t.Errorf("doctor failure should abort before %s; got calls %v", name, got)
		}
	}
}

func TestOrchestrator_SeedFailure_RunsClean(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedErr:      errors.New("seed boom"),
		seedRes:      minimalSeedResult(), // manifest non-nil so clean has something to act on
	}
	o, outDir, _ := makeOrch(t, fr, nil)

	// Manifest must exist on disk for clean to engage. Pre-create it as
	// the orchestrator's contract assumes the seed stage wrote one before
	// failing — Seed in this fake returns a non-nil manifest in seedRes.
	manifestPath := filepath.Join(outDir, "manifest.json")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected seed failure to bubble up")
	}
	got := fr.Calls()
	if !contains(got, "clean") {
		t.Errorf("seed failure should still trigger clean; got calls %v", got)
	}
	if contains(got, "run") {
		t.Errorf("seed failure should abort before run; got calls %v", got)
	}
	if res.Overall != StageFail {
		t.Errorf("Overall = %s, want FAIL", res.Overall)
	}
}

func TestOrchestrator_KeepDataSkipsClean(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
		reportPath:   "/tmp/report.html",
	}
	o, _, _ := makeOrch(t, fr, func(c *Config) { c.KeepData = true })
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if contains(fr.Calls(), "clean") {
		t.Errorf("--keep-data should skip clean; got calls %v", fr.Calls())
	}
	cleanIdx := stageIndex(res.Stages, StageClean)
	if res.Stages[cleanIdx].Status != StageSkip {
		t.Errorf("clean stage status = %s, want SKIP", res.Stages[cleanIdx].Status)
	}
}

func TestOrchestrator_LifecycleSkippedWhenFlagOff(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
	}
	o, _, _ := makeOrch(t, fr, func(c *Config) {
		c.IncludeLifecycle = false
		c.Scenario.Lifecycle.Enabled = true // even if scenario says yes
	})
	_, _ = o.Run(context.Background())
	if contains(fr.Calls(), "lifecycle") {
		t.Errorf("--include-lifecycle off should skip lifecycle; got calls %v", fr.Calls())
	}
}

func TestOrchestrator_LifecycleSkippedWhenScenarioOff(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
	}
	o, _, _ := makeOrch(t, fr, func(c *Config) {
		c.IncludeLifecycle = true
		c.Scenario.Lifecycle.Enabled = false
	})
	_, _ = o.Run(context.Background())
	if contains(fr.Calls(), "lifecycle") {
		t.Errorf("scenario.lifecycle.enabled=false should skip lifecycle; got calls %v", fr.Calls())
	}
}

func TestOrchestrator_LifecycleRunsWhenBothOn(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
		runDelay:     50 * time.Millisecond,
	}
	o, _, _ := makeOrch(t, fr, func(c *Config) {
		c.IncludeLifecycle = true
		c.Scenario.Lifecycle.Enabled = true
		c.Scenario.Lifecycle.UpgradesPerHourPct = 0.01
	})
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(fr.Calls(), "lifecycle") {
		t.Errorf("lifecycle should run when both flags + scenario enabled; got calls %v", fr.Calls())
	}
	if res.Overall != StagePass {
		t.Errorf("Overall = %s, want PASS", res.Overall)
	}
}

func TestOrchestrator_ValidateFailureRendersReport(t *testing.T) {
	failed := &validate.ValidationReport{
		Checks: []*validate.CheckResult{
			{Name: "test", Status: validate.StatusFail, Reason: "things broke"},
		},
	}
	failed.Finalize()
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    failed,
		reportPath:   "/tmp/report.html",
	}
	o, _, _ := makeOrch(t, fr, nil)

	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error on validate failure")
	}
	// Report should still be rendered — that's how operators triage.
	if !contains(fr.Calls(), "report") {
		t.Errorf("report stage should run on validate failure; got calls %v", fr.Calls())
	}
}

func TestOrchestrator_PartialFailurePreservesArtifacts(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runErr:       errors.New("run boom"),
	}
	o, outDir, _ := makeOrch(t, fr, nil)

	// Pre-create the manifest as the seed stage would.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected run error to bubble up")
	}
	// e2e.json must be written even on failure.
	data, err := os.ReadFile(filepath.Join(outDir, "e2e.json"))
	if err != nil {
		t.Fatalf("e2e.json should exist on failure: %v", err)
	}
	var summary Result
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("e2e.json malformed: %v", err)
	}
	if summary.Overall != StageFail {
		t.Errorf("e2e.json overall = %s, want FAIL", summary.Overall)
	}
}

func TestOrchestrator_StageOrder_IsCanonical(t *testing.T) {
	fr := &fakeRunner{
		doctorReport: &doctor.Report{Overall: doctor.StatusOK},
		seedRes:      minimalSeedResult(),
		runRes:       minimalRunResult(),
		valReport:    passingValidation(),
		reportPath:   "/tmp/report.html",
	}
	o, _, _ := makeOrch(t, fr, nil)
	_, _ = o.Run(context.Background())

	calls := fr.Calls()
	// Doctor before seed before run before validate before report before clean.
	// (Lifecycle is concurrent with run; not asserted here.)
	want := []string{"doctor", "seed", "run", "validate", "report", "clean"}
	gotIdx := 0
	for _, c := range calls {
		if gotIdx < len(want) && c == want[gotIdx] {
			gotIdx++
		}
	}
	if gotIdx < len(want) {
		t.Errorf("expected stage order %v in calls %v", want, calls)
	}
}

func TestOrchestrator_SkipDoctor(t *testing.T) {
	fr := &fakeRunner{
		seedRes:    minimalSeedResult(),
		runRes:     minimalRunResult(),
		valReport:  passingValidation(),
		reportPath: "/tmp/report.html",
	}
	o, _, _ := makeOrch(t, fr, func(c *Config) { c.SkipDoctor = true })
	_, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if contains(fr.Calls(), "doctor") {
		t.Errorf("SkipDoctor should bypass doctor stage; got calls %v", fr.Calls())
	}
}

func TestOrchestrator_StageDetailFormatters(t *testing.T) {
	d := doctorDetail(&doctor.Report{
		Checks: []doctor.CheckResult{
			{Status: doctor.StatusOK},
			{Status: doctor.StatusOK},
			{Status: doctor.StatusFail},
		},
	})
	if !strings.Contains(d, "2 pass") || !strings.Contains(d, "1 fail") {
		t.Errorf("doctorDetail = %q", d)
	}

	s := seedDetail(&seed.RunResult{
		Manifest: &seed.Manifest{
			Summary: seed.ManifestSummary{TotalTenants: 4, TotalCustomers: 400, TotalSubs: 400},
		},
	})
	if !strings.Contains(s, "tenants=4") || !strings.Contains(s, "customers=400") {
		t.Errorf("seedDetail = %q", s)
	}

	r := runDetail(&runner.RunResult{
		EventsSubmitted: 1000,
		EventsSucceeded: 990,
		ClientErrors:    10,
		ServerErrors:    0,
		LatencyP99ms:    150,
	})
	if !strings.Contains(r, "events=1000") || !strings.Contains(r, "p99=150ms") {
		t.Errorf("runDetail = %q", r)
	}

	v := validateDetail(passingValidation())
	if !strings.Contains(v, "1 pass") {
		t.Errorf("validateDetail = %q", v)
	}
}

func TestRollUpStatus(t *testing.T) {
	tests := []struct {
		name   string
		stages []StageResult
		want   StageStatus
	}{
		{"all pass", []StageResult{{Status: StagePass}, {Status: StagePass}}, StagePass},
		{"one fail", []StageResult{{Status: StagePass}, {Status: StageFail}}, StageFail},
		{"all skip", []StageResult{{Status: StageSkip}, {Status: StageSkip}}, StageSkip},
		{"pending = fail", []StageResult{{Status: StagePass}, {Status: StagePending}}, StageFail},
		{"mixed pass/skip", []StageResult{{Status: StagePass}, {Status: StageSkip}}, StagePass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rollUpStatus(tc.stages)
			if got != tc.want {
				t.Errorf("rollUpStatus = %s, want %s", got, tc.want)
			}
		})
	}
}

// --- helpers ---

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func sliceContainsAll(s, want []string) bool {
	for _, w := range want {
		if !contains(s, w) {
			return false
		}
	}
	return true
}
