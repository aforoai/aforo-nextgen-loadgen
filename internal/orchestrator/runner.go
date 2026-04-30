// DefaultStageRunner adapts the existing Session 1-6 packages onto the
// orchestrator's StageRunner interface. Every method is a thin shim — the
// hard work lives in seed/runner/lifecycle/validate; here we just pack
// arguments and translate per-stage error semantics.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/doctor"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/report"
)

// DefaultStageRunner is the production implementation of StageRunner.
// Tests use the in-memory FakeStageRunner in runner_test.go.
type DefaultStageRunner struct{}

// Doctor delegates to internal/doctor with a 5s per-probe timeout.
func (DefaultStageRunner) Doctor(ctx context.Context, target aforo.Target, token string) (*doctor.Report, error) {
	d, err := doctor.New(doctor.Config{
		Target:          target,
		BearerToken:     token,
		PerCheckTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return d.Run(ctx), nil
}

// Seed delegates to internal/seed.NewSeeder.Run, persisting the manifest
// at the orchestrator's chosen path.
func (DefaultStageRunner) Seed(ctx context.Context, in SeedInput) (*seed.RunResult, error) {
	if in.BearerToken == "" {
		return nil, errors.New("seed: bearer token is required")
	}
	c, err := seed.NewClient(seed.ClientConfig{
		Target:      in.Target,
		BearerToken: in.BearerToken,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	s, err := seed.NewSeeder(seed.SeederConfig{
		Client:       c,
		Scenario:     in.Scenario,
		ManifestPath: in.ManifestPath,
		RunID:        in.RunID,
	})
	if err != nil {
		return nil, err
	}
	res, err := s.Run(ctx)
	if err != nil {
		return res, err
	}
	if res != nil && len(res.Errors) > 0 {
		// Surface seed errors as a stage-level error so the orchestrator
		// can decide whether to abort. Manifest is still preserved (and
		// non-nil) so the clean stage can tear down what landed.
		return res, fmt.Errorf("seed completed with %d error(s)", len(res.Errors))
	}
	return res, nil
}

// RunLoad delegates to internal/runner.New + Run.
func (DefaultStageRunner) RunLoad(ctx context.Context, in RunInput) (*runner.RunResult, error) {
	cfg := runner.Config{
		Scenario:    in.Scenario,
		Manifest:    in.Manifest,
		Target:      in.Target,
		OutputDir:   in.OutputDir,
		Workers:     in.Workers,
		BufferSize:  in.BufferSize,
		AdminToken:  in.BearerToken,
		MetricsAddr: "", // disabled inside e2e — operator is not watching /metrics
	}
	r, err := runner.New(cfg)
	if err != nil {
		return nil, err
	}
	return r.Run(ctx)
}

// Lifecycle delegates to internal/lifecycle. The agent owns its own
// transition log under <out>/transitions.jsonl which the validate stage
// later picks up via lifecycle.LoadTransitionLog.
func (DefaultStageRunner) Lifecycle(ctx context.Context, in LifecycleInput) (lifecycle.Snapshot, error) {
	tlog, err := lifecycle.NewTransitionLog(in.OutputDir)
	if err != nil {
		return lifecycle.Snapshot{}, err
	}
	defer func() { _ = tlog.Close() }()

	client, err := lifecycle.NewClient(lifecycle.ClientConfig{
		Target: in.Target,
		Token:  in.BearerToken,
	})
	if err != nil {
		return lifecycle.Snapshot{}, fmt.Errorf("lifecycle client: %w", err)
	}

	picker := lifecycle.NewPicker(in.Manifest, in.Scenario.Seed)
	agent, err := lifecycle.NewAgent(lifecycle.AgentConfig{
		Scenario:         in.Scenario,
		Manifest:         in.Manifest,
		Log:              tlog,
		Client:           client,
		Picker:           picker,
		PauseResumeDelay: in.PauseResumeDelay,
	})
	if err != nil {
		return lifecycle.Snapshot{}, err
	}

	// The agent runs until ctx cancels. The orchestrator's RunLoad
	// goroutine owns the timer; when it returns it cancels the shared
	// ctx and the agent drains. We wait for that drain to complete
	// before snapshotting — reading the log mid-drain races with the
	// agent's own writers.
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx)
	}()
	runErr := <-done

	snap := agent.LogSnapshot()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return snap, runErr
	}
	return snap, nil
}

// Validate delegates to internal/validate.New + Run, loading the
// transition log from disk if present.
func (DefaultStageRunner) Validate(ctx context.Context, in ValidateInput) (*validate.ValidationReport, error) {
	transitions, err := lifecycle.LoadTransitionLog(in.RunOutputDir)
	if err != nil {
		return nil, fmt.Errorf("load transitions: %w", err)
	}
	v, err := validate.New(&validate.Inputs{
		RunOutputDir:   in.RunOutputDir,
		Manifest:       in.Manifest,
		Run:            in.Run,
		Scenario:       in.Scenario,
		Backend:        in.Backend,
		IncludeBilling: in.IncludeBilling,
		Transitions:    transitions,
	})
	if err != nil {
		return nil, err
	}
	return v.Run(ctx)
}

// Report delegates to internal/validate/report.Render.
func (DefaultStageRunner) Report(_ context.Context, runOut string, run *runner.RunResult, val *validate.ValidationReport) (string, error) {
	htmlPath, err := report.Render(runOut, run, val)
	if err != nil {
		return "", err
	}
	return filepath.Clean(htmlPath), nil
}

// Clean delegates to internal/seed.Clean. Non-empty error list rolls up
// into a stage-level error with a friendly message.
func (DefaultStageRunner) Clean(ctx context.Context, in CleanInput) error {
	if in.BearerToken == "" {
		return errors.New("clean: bearer token is required")
	}
	c, err := seed.NewClient(seed.ClientConfig{
		Target:      in.Target,
		BearerToken: in.BearerToken,
	})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	m, err := seed.LoadManifest(in.ManifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	res := seed.Clean(ctx, c, m)
	if len(res.Errors) > 0 {
		// Per-entity failures are non-fatal at the e2e level — the
		// platform's archive endpoints are not always idempotent on
		// already-archived rows. Log the count and continue.
		return fmt.Errorf("clean: %d entity(s) could not be archived (manifest preserved at %s)",
			len(res.Errors), in.ManifestPath)
	}
	return nil
}
