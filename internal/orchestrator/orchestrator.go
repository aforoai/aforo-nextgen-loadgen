// Package orchestrator implements the Session 7 end-to-end workflow that
// `aforo-loadgen e2e` exposes:
//
//	doctor → seed → run + lifecycle (parallel) → validate → report → clean
//
// Each stage's artifacts feed the next; failures preserve everything
// produced so far so a debugging operator can inspect the partial state.
//
// Why this lives in its own package (not in internal/cli):
//
//  1. The CLI layer should remain a thin adapter — flag parsing, IO
//     wiring, exit-code translation. Orchestration logic carries enough
//     state (per-stage results, output dir, --keep-data flag, partial
//     failure semantics) that pulling it out makes both halves testable
//     in isolation.
//  2. Unit tests for stage sequencing become trivial when each stage is a
//     small struct method — tests inject fakes into the StageRunner
//     interface rather than spinning up a real cobra root.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/doctor"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

// StageName identifies one phase of the e2e flow. Used in logs and the
// summary printout. The slice form below is the canonical execution
// order.
type StageName string

const (
	StageDoctor    StageName = "doctor"
	StageSeed      StageName = "seed"
	StageRun       StageName = "run"
	StageLifecycle StageName = "lifecycle"
	StageValidate  StageName = "validate"
	StageReport    StageName = "report"
	StageClean     StageName = "clean"
)

// AllStages is the canonical order. Note that run + lifecycle execute
// concurrently inside StageRun's slot; lifecycle is presented as its own
// row in the summary so operators can see the per-stage timing.
var AllStages = []StageName{
	StageDoctor, StageSeed, StageRun, StageLifecycle, StageValidate, StageReport, StageClean,
}

// StageStatus is the per-stage verdict.
type StageStatus string

const (
	StagePass    StageStatus = "PASS"
	StageFail    StageStatus = "FAIL"
	StageSkip    StageStatus = "SKIP"
	StagePending StageStatus = "PENDING"
)

// StageResult is what each stage reports back. Detail is free-form per
// stage and may contain stage-native metrics (events/sec, transitions
// fired, checks-passed, etc.).
type StageResult struct {
	Name      StageName     `json:"name"`
	Status    StageStatus   `json:"status"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Duration  time.Duration `json:"duration_ms"`
	Detail    string        `json:"detail,omitempty"`
	Err       string        `json:"error,omitempty"`
}

// Result is the aggregate outcome of an e2e run.
type Result struct {
	Scenario  string        `json:"scenario"`
	Target    string        `json:"target"`
	OutputDir string        `json:"output_dir"`
	Stages    []StageResult `json:"stages"`
	Overall   StageStatus   `json:"overall"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Elapsed   time.Duration `json:"elapsed_ms"`
}

// Save writes the e2e summary to <out>/e2e.json.
func (r *Result) Save(out string) (string, error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(out, "e2e.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o644) // #nosec G306 — operator artifact
}

// Config wires the orchestrator. Most fields mirror their CLI flag
// counterparts; the StageRunner interface is the only injectable seam,
// used by unit tests to assert sequencing without spinning up real
// services.
type Config struct {
	Scenario         *scenario.Scenario
	ScenarioYAML     []byte // canonical bytes for run/seed to embed/reuse
	Target           aforo.Target
	OutputDir        string
	BearerToken      string

	IncludeBilling   bool
	IncludeLifecycle bool
	KeepData         bool

	// Workers + buffer for the runner; sensible defaults applied below.
	Workers    int
	BufferSize int

	// LifecyclePauseResumeDelay sets the gap between paired pause/resume
	// transitions. 30s is the default — short enough to fit inside a
	// 5-minute crawl window and exercise the resume code path.
	LifecyclePauseResumeDelay time.Duration

	// SkipDoctor is for tests + recovery flows that already know the
	// platform is healthy. Production callers leave this false.
	SkipDoctor bool

	// Stdout and Stderr — write progress + errors here. nil → real ones.
	Stdout io.Writer
	Stderr io.Writer

	// Runner — overrideable for tests. nil → DefaultStageRunner.
	Runner StageRunner

	// Now — clock injection for tests. nil → time.Now.
	Now func() time.Time
}

// StageRunner is the orchestrator's interface to each underlying
// subsystem. The default implementation calls real seeders/runners; tests
// inject fakes that record call order.
type StageRunner interface {
	Doctor(ctx context.Context, target aforo.Target, token string) (*doctor.Report, error)
	Seed(ctx context.Context, c SeedInput) (*seed.RunResult, error)
	RunLoad(ctx context.Context, c RunInput) (*runner.RunResult, error)
	Lifecycle(ctx context.Context, c LifecycleInput) (lifecycle.Snapshot, error)
	Validate(ctx context.Context, c ValidateInput) (*validate.ValidationReport, error)
	Report(ctx context.Context, runOut string, run *runner.RunResult, val *validate.ValidationReport) (string, error)
	Clean(ctx context.Context, c CleanInput) error
}

// SeedInput, RunInput, etc. are tiny bundles passed to StageRunner. They
// keep the interface focused on data, not state, so tests can construct
// inputs in-line.
type SeedInput struct {
	Target       aforo.Target
	BearerToken  string
	Scenario     *scenario.Scenario
	ManifestPath string
	RunID        string
}

type RunInput struct {
	Scenario    *scenario.Scenario
	Manifest    *seed.Manifest
	Target      aforo.Target
	OutputDir   string
	Workers     int
	BufferSize  int
	BearerToken string
}

type LifecycleInput struct {
	Scenario         *scenario.Scenario
	Manifest         *seed.Manifest
	Target           aforo.Target
	BearerToken      string
	OutputDir        string
	PauseResumeDelay time.Duration
}

type ValidateInput struct {
	RunOutputDir   string
	Manifest       *seed.Manifest
	Run            *runner.RunResult
	Scenario       *scenario.Scenario
	Backend        validate.BackendClient
	IncludeBilling bool
}

type CleanInput struct {
	Target       aforo.Target
	BearerToken  string
	ManifestPath string
}

// Orchestrator runs the e2e pipeline.
type Orchestrator struct {
	cfg Config
}

// New constructs an Orchestrator and validates the config.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.Scenario == nil {
		return nil, errors.New("orchestrator: Scenario is nil")
	}
	if cfg.OutputDir == "" {
		return nil, errors.New("orchestrator: OutputDir is required")
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 32
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 4096
	}
	if cfg.LifecyclePauseResumeDelay <= 0 {
		cfg.LifecyclePauseResumeDelay = 30 * time.Second
	}
	if cfg.Runner == nil {
		cfg.Runner = &DefaultStageRunner{}
	}
	return &Orchestrator{cfg: cfg}, nil
}

// Run executes every stage in order. The ctx may be canceled at any
// point; in-flight stages drain best-effort and the partial result is
// still saved to disk.
//
// Returns the aggregate Result and an error if any stage failed. The
// error wraps the first FAIL stage's error so operators see the
// proximate cause; subsequent stages still report PENDING in the
// returned Result so the summary tells the full story.
func (o *Orchestrator) Run(ctx context.Context) (*Result, error) {
	overallStart := o.cfg.Now()
	res := &Result{
		Scenario:  o.cfg.Scenario.Name,
		Target:    o.cfg.Target.Name,
		OutputDir: o.cfg.OutputDir,
		StartedAt: overallStart,
	}
	for _, s := range AllStages {
		res.Stages = append(res.Stages, StageResult{Name: s, Status: StagePending})
	}

	if err := os.MkdirAll(o.cfg.OutputDir, 0o755); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", o.cfg.OutputDir, err)
	}

	manifestPath := filepath.Join(o.cfg.OutputDir, "manifest.json")
	o.log("e2e — scenario=%s target=%s out=%s", res.Scenario, res.Target, o.cfg.OutputDir)

	var firstErr error
	defer func() {
		// Even on cancellation or early exit, write the e2e summary so
		// debugging operators have something to look at. We compute
		// Overall here based on stage rollups.
		res.EndedAt = o.cfg.Now()
		res.Elapsed = res.EndedAt.Sub(res.StartedAt)
		res.Overall = rollUpStatus(res.Stages)
		if path, err := res.Save(o.cfg.OutputDir); err != nil {
			fmt.Fprintf(o.cfg.Stderr, "warn: could not write e2e.json: %v\n", err)
		} else {
			o.log("e2e summary: %s (overall=%s elapsed=%s)", path, res.Overall, res.Elapsed.Round(time.Millisecond))
		}
	}()

	// Stage 1: doctor.
	if !o.cfg.SkipDoctor {
		st := o.startStage(StageDoctor, res)
		report, err := o.cfg.Runner.Doctor(ctx, o.cfg.Target, o.cfg.BearerToken)
		o.endStage(st, res, err, doctorDetail(report))
		if err != nil || (report != nil && report.HasCritical()) {
			if err == nil {
				err = fmt.Errorf("doctor failed: %d critical check(s)", critFailCount(report))
			}
			firstErr = err
			return res, firstErr
		}
	} else {
		setStage(res, StageDoctor, StageSkip, "skipped via SkipDoctor", nil)
	}

	// Stage 2: seed.
	st := o.startStage(StageSeed, res)
	seedRes, err := o.cfg.Runner.Seed(ctx, SeedInput{
		Target:       o.cfg.Target,
		BearerToken:  o.cfg.BearerToken,
		Scenario:     o.cfg.Scenario,
		ManifestPath: manifestPath,
	})
	if err != nil {
		o.endStage(st, res, err, "")
		firstErr = fmt.Errorf("seed: %w", err)
		// Even on seed failure, try clean — partial provisioning may
		// have written entities before erroring. The runCleanStage
		// helper SKIPs if no manifest is on disk, so this is safe when
		// nothing landed.
		o.runCleanStage(ctx, manifestPath, res)
		return res, firstErr
	}
	if seedRes == nil || seedRes.Manifest == nil {
		o.endStage(st, res, errors.New("seed produced no manifest"), "")
		firstErr = errors.New("seed produced no manifest — aborting")
		o.runCleanStage(ctx, manifestPath, res)
		return res, firstErr
	}
	o.endStage(st, res, nil, seedDetail(seedRes))

	// Stage 3+4: run + (optional) lifecycle, in parallel.
	runStartIdx := stageIndex(res.Stages, StageRun)
	lcStartIdx := stageIndex(res.Stages, StageLifecycle)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	type runOut struct {
		res *runner.RunResult
		err error
	}
	type lcOut struct {
		snap lifecycle.Snapshot
		err  error
	}
	runCh := make(chan runOut, 1)
	lcCh := make(chan lcOut, 1)

	res.Stages[runStartIdx].StartedAt = o.cfg.Now()
	res.Stages[runStartIdx].Status = StagePending

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, err := o.cfg.Runner.RunLoad(runCtx, RunInput{
			Scenario:    o.cfg.Scenario,
			Manifest:    seedRes.Manifest,
			Target:      o.cfg.Target,
			OutputDir:   o.cfg.OutputDir,
			Workers:     o.cfg.Workers,
			BufferSize:  o.cfg.BufferSize,
			BearerToken: o.cfg.BearerToken,
		})
		runCh <- runOut{res: out, err: err}
		// Run finished — signal the lifecycle goroutine to drain. The
		// real lifecycle agent blocks on ctx.Done(); without this
		// cancel the orchestrator's wg.Wait() below would hang for the
		// agent's own scenario duration timer instead of the run's.
		cancelRun()
	}()

	if o.cfg.IncludeLifecycle && o.cfg.Scenario.Lifecycle.Enabled {
		res.Stages[lcStartIdx].StartedAt = o.cfg.Now()
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := o.cfg.Runner.Lifecycle(runCtx, LifecycleInput{
				Scenario:         o.cfg.Scenario,
				Manifest:         seedRes.Manifest,
				Target:           o.cfg.Target,
				BearerToken:      o.cfg.BearerToken,
				OutputDir:        o.cfg.OutputDir,
				PauseResumeDelay: o.cfg.LifecyclePauseResumeDelay,
			})
			lcCh <- lcOut{snap: snap, err: err}
		}()
	} else {
		// Mark lifecycle SKIP up-front so the rollup is honest.
		setStage(res, StageLifecycle, StageSkip,
			lifecycleSkipReason(o.cfg.IncludeLifecycle, o.cfg.Scenario.Lifecycle.Enabled), nil)
	}

	wg.Wait()
	close(runCh)
	close(lcCh)

	rOut := <-runCh
	res.Stages[runStartIdx].EndedAt = o.cfg.Now()
	res.Stages[runStartIdx].Duration = res.Stages[runStartIdx].EndedAt.Sub(res.Stages[runStartIdx].StartedAt)
	if rOut.err != nil && !errors.Is(rOut.err, context.Canceled) {
		res.Stages[runStartIdx].Status = StageFail
		res.Stages[runStartIdx].Err = rOut.err.Error()
	} else {
		res.Stages[runStartIdx].Status = StagePass
		if rOut.res != nil {
			res.Stages[runStartIdx].Detail = runDetail(rOut.res)
		}
	}

	if o.cfg.IncludeLifecycle && o.cfg.Scenario.Lifecycle.Enabled {
		lOut := <-lcCh
		res.Stages[lcStartIdx].EndedAt = o.cfg.Now()
		res.Stages[lcStartIdx].Duration = res.Stages[lcStartIdx].EndedAt.Sub(res.Stages[lcStartIdx].StartedAt)
		if lOut.err != nil && !errors.Is(lOut.err, context.Canceled) {
			res.Stages[lcStartIdx].Status = StageFail
			res.Stages[lcStartIdx].Err = lOut.err.Error()
			// Surface lifecycle failure as a process-level error so the
			// CLI exits non-zero. Without this, lifecycle failure is
			// only visible in e2e.json — operators would miss it in CI.
			if firstErr == nil {
				firstErr = fmt.Errorf("lifecycle: %w", lOut.err)
			}
		} else {
			res.Stages[lcStartIdx].Status = StagePass
			res.Stages[lcStartIdx].Detail = lifecycleDetail(lOut.snap)
		}
	}

	if rOut.err != nil && !errors.Is(rOut.err, context.Canceled) {
		firstErr = fmt.Errorf("run: %w", rOut.err)
		// Run failure is fatal — skip validate + report (no run.json), but
		// still try the clean stage so we don't leak seeded entities.
		o.runCleanStage(ctx, manifestPath, res)
		return res, firstErr
	}
	if rOut.res == nil {
		firstErr = errors.New("run: no result returned")
		o.runCleanStage(ctx, manifestPath, res)
		return res, firstErr
	}

	// Stage 5: validate.
	st = o.startStage(StageValidate, res)
	valReport, err := o.cfg.Runner.Validate(ctx, ValidateInput{
		RunOutputDir:   o.cfg.OutputDir,
		Manifest:       seedRes.Manifest,
		Run:            rOut.res,
		Scenario:       o.cfg.Scenario,
		Backend:        validate.NewOfflineBackend(rOut.res),
		IncludeBilling: o.cfg.IncludeBilling,
	})
	if err != nil {
		o.endStage(st, res, err, "")
		firstErr = fmt.Errorf("validate: %w", err)
		// Continue to report + clean — partial state is still useful.
	} else {
		// Save the validation report so the report stage can pick it up.
		if _, saveErr := valReport.Save(o.cfg.OutputDir); saveErr != nil {
			fmt.Fprintf(o.cfg.Stderr, "warn: could not save validation.json: %v\n", saveErr)
		}
		detail := validateDetail(valReport)
		if valReport.Summary.Failed > 0 {
			res.Stages[stageIndex(res.Stages, StageValidate)].Status = StageFail
			res.Stages[stageIndex(res.Stages, StageValidate)].EndedAt = o.cfg.Now()
			res.Stages[stageIndex(res.Stages, StageValidate)].Duration = res.Stages[stageIndex(res.Stages, StageValidate)].EndedAt.Sub(st.StartedAt)
			res.Stages[stageIndex(res.Stages, StageValidate)].Detail = detail
			if firstErr == nil {
				firstErr = fmt.Errorf("validate: %d check(s) failed", valReport.Summary.Failed)
			}
		} else {
			o.endStage(st, res, nil, detail)
		}
	}

	// Stage 6: report. We try to render even when validate FAILed —
	// having the HTML around is more useful than not.
	st = o.startStage(StageReport, res)
	reportPath, err := o.cfg.Runner.Report(ctx, o.cfg.OutputDir, rOut.res, valReport)
	if err != nil {
		o.endStage(st, res, err, "")
		fmt.Fprintf(o.cfg.Stderr, "warn: report rendering failed: %v\n", err)
	} else {
		o.endStage(st, res, nil, "report.html: "+reportPath)
	}

	// Stage 7: clean (skipped when --keep-data). On the happy path, we
	// archive seeded entities so the operator can re-run e2e idempotently.
	o.runCleanStage(ctx, manifestPath, res)

	return res, firstErr
}

// runCleanStage centralizes the clean-or-skip logic so both happy and
// failure paths converge on the same behavior: clean unless --keep-data.
func (o *Orchestrator) runCleanStage(ctx context.Context, manifestPath string, res *Result) {
	if o.cfg.KeepData {
		setStage(res, StageClean, StageSkip, "skipped via --keep-data", nil)
		return
	}
	if _, err := os.Stat(manifestPath); err != nil {
		setStage(res, StageClean, StageSkip, "no manifest written — nothing to clean", nil)
		return
	}
	st := o.startStage(StageClean, res)
	err := o.cfg.Runner.Clean(ctx, CleanInput{
		Target:       o.cfg.Target,
		BearerToken:  o.cfg.BearerToken,
		ManifestPath: manifestPath,
	})
	o.endStage(st, res, err, "")
}

// startStage marks a stage as in-flight and returns a pointer to its
// StageResult so endStage can finalize timing.
func (o *Orchestrator) startStage(name StageName, res *Result) *StageResult {
	idx := stageIndex(res.Stages, name)
	res.Stages[idx].StartedAt = o.cfg.Now()
	res.Stages[idx].Status = StagePending
	o.log("stage %s: starting", name)
	return &res.Stages[idx]
}

// endStage stamps end time and classifies as PASS/FAIL.
func (o *Orchestrator) endStage(st *StageResult, _ *Result, err error, detail string) {
	st.EndedAt = o.cfg.Now()
	st.Duration = st.EndedAt.Sub(st.StartedAt)
	if err != nil && !errors.Is(err, context.Canceled) {
		st.Status = StageFail
		st.Err = err.Error()
		o.log("stage %s: FAIL (%s) — %v", st.Name, st.Duration.Round(time.Millisecond), err)
		return
	}
	st.Status = StagePass
	if detail != "" {
		st.Detail = detail
	}
	o.log("stage %s: PASS (%s) %s", st.Name, st.Duration.Round(time.Millisecond), detail)
}

func (o *Orchestrator) log(format string, a ...any) {
	fmt.Fprintf(o.cfg.Stdout, "["+time.Now().UTC().Format("15:04:05")+"] "+format+"\n", a...)
}

// rollUpStatus computes the overall verdict:
//   - any FAIL → FAIL
//   - all SKIP / PENDING → SKIP
//   - else PASS
func rollUpStatus(stages []StageResult) StageStatus {
	allSkip := true
	for _, s := range stages {
		if s.Status == StageFail {
			return StageFail
		}
		if s.Status == StagePass {
			allSkip = false
		}
		if s.Status == StagePending {
			// Pending stages were never reached due to early exit — that's
			// a failure of the pipeline as a whole, but only if some
			// previous stage hasn't already flipped us to FAIL above.
			return StageFail
		}
	}
	if allSkip {
		return StageSkip
	}
	return StagePass
}

// stageIndex returns the index of a named stage in a stage list. Panics
// if not found — that would indicate orchestrator bug, not user input.
func stageIndex(stages []StageResult, name StageName) int {
	for i, s := range stages {
		if s.Name == name {
			return i
		}
	}
	panic(fmt.Sprintf("orchestrator: missing stage %q", name))
}

// setStage is a small helper that sets status + detail on a named stage
// and stamps current timestamps.
func setStage(res *Result, name StageName, status StageStatus, detail string, err error) {
	idx := stageIndex(res.Stages, name)
	now := time.Now()
	if res.Stages[idx].StartedAt.IsZero() {
		res.Stages[idx].StartedAt = now
	}
	res.Stages[idx].EndedAt = now
	res.Stages[idx].Duration = res.Stages[idx].EndedAt.Sub(res.Stages[idx].StartedAt)
	res.Stages[idx].Status = status
	res.Stages[idx].Detail = detail
	if err != nil {
		res.Stages[idx].Err = err.Error()
	}
}

// --- Detail formatters: kept tiny and pure so tests can assert them. ---

func doctorDetail(r *doctor.Report) string {
	if r == nil {
		return ""
	}
	pass, fail, skip := 0, 0, 0
	for _, c := range r.Checks {
		switch c.Status {
		case doctor.StatusOK:
			pass++
		case doctor.StatusFail:
			fail++
		case doctor.StatusSkip:
			skip++
		}
	}
	return fmt.Sprintf("checks: %d pass / %d fail / %d skip", pass, fail, skip)
}

func critFailCount(r *doctor.Report) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, c := range r.Checks {
		if c.Status == doctor.StatusFail && c.Severity == doctor.SeverityCritical {
			n++
		}
	}
	return n
}

func seedDetail(s *seed.RunResult) string {
	if s == nil || s.Manifest == nil {
		return ""
	}
	sum := s.Manifest.Summary
	return fmt.Sprintf("tenants=%d customers=%d subs=%d errors=%d",
		sum.TotalTenants, sum.TotalCustomers, sum.TotalSubs, len(s.Errors))
}

func runDetail(r *runner.RunResult) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("events=%d ok=%d 4xx=%d 5xx=%d transport_fail=%d p99=%.0fms",
		r.EventsSubmitted, r.EventsSucceeded, r.ClientErrors, r.ServerErrors,
		r.TransportFailures, r.LatencyP99ms)
}

func lifecycleDetail(snap lifecycle.Snapshot) string {
	total := 0
	for _, n := range snap.ByKind {
		total += n
	}
	return fmt.Sprintf("transitions=%d", total)
}

func validateDetail(r *validate.ValidationReport) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("checks: %d pass / %d fail / %d skip / %d total",
		r.Summary.Passed, r.Summary.Failed, r.Summary.Skipped, r.Summary.Total)
}

func lifecycleSkipReason(includeLifecycle, scenarioEnabled bool) string {
	switch {
	case !includeLifecycle:
		return "skipped: --include-lifecycle not set"
	case !scenarioEnabled:
		return "skipped: scenario.lifecycle.enabled=false"
	default:
		return "skipped"
	}
}

// PrintSummary writes a human-readable stage summary table to out.
func (r *Result) PrintSummary(out io.Writer) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "e2e summary")
	fmt.Fprintf(out, "scenario: %s   target: %s   out: %s\n", r.Scenario, r.Target, r.OutputDir)
	fmt.Fprintln(out, strings.Repeat("─", 76))
	for _, s := range r.Stages {
		dur := s.Duration.Round(time.Millisecond)
		detail := s.Detail
		if s.Err != "" {
			detail = "ERR: " + s.Err
		}
		fmt.Fprintf(out, "  %-12s %-8s %-10s %s\n", s.Name, s.Status, dur, detail)
	}
	fmt.Fprintln(out, strings.Repeat("─", 76))
	fmt.Fprintf(out, "  overall=%s   total=%s\n", r.Overall, r.Elapsed.Round(time.Millisecond))
}
