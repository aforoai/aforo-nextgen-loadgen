package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

type replayFlags struct {
	runOutput string
	target    string
	manifest  string
	out       string
	tokenEnv  string
}

// newReplayCommand wires `aforo-loadgen replay`. Replay re-runs a recorded
// run against a (possibly different) target — same scenario, same seed,
// same manifest → identical event sequence.
func newReplayCommand(_ *GlobalFlags) *cobra.Command {
	var f replayFlags
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay a recorded run-output against a target",
		Long: `Replay re-executes the scenario captured in a previous run-output
directory against the same (or a different) target. Reproducibility comes
from the scenario seed plus the manifest — replay reads scenario.yaml from
<run-output>/scenario.yaml and the same manifest the original run used.

Examples:
  aforo-loadgen replay --run-output runs/ci-smoke-1714400000 --target local
  aforo-loadgen replay --run-output runs/ci-smoke-1714400000 --target https://staging.aforo.io --manifest manifest.json
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReplay(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.runOutput, "run-output", "", "directory of a prior run (must contain scenario.yaml)")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, ci, or full URL")
	cmd.Flags().StringVar(&f.manifest, "manifest", "manifest.json", "path to manifest from `aforo-loadgen seed`")
	cmd.Flags().StringVar(&f.out, "out", "", "replay output directory (default: <run-output>/replay-<unix>)")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding an admin bearer token")
	return cmd
}

func runReplay(ctx context.Context, out, errOut io.Writer, f *replayFlags) error {
	if f.runOutput == "" {
		return errors.New("--run-output is required")
	}
	scnPath := filepath.Join(f.runOutput, "scenario.yaml")
	scnBytes, err := os.ReadFile(scnPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", scnPath, err)
	}
	doc, err := scenario.LoadFromBytes(scnBytes)
	if err != nil {
		return fmt.Errorf("parse scenario.yaml: %w", err)
	}
	if errs := scenario.Validate(doc); errs.HasErrors() {
		for _, e := range errs {
			fmt.Fprintln(errOut, e.Error())
		}
		return fmt.Errorf("recorded scenario failed validation; cannot replay")
	}

	manifest, err := seed.LoadManifest(f.manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}

	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join(f.runOutput, fmt.Sprintf("replay-%d", time.Now().Unix()))
	}

	cfg := runner.Config{
		Scenario:    doc.Scenario,
		Manifest:    manifest,
		Target:      target,
		OutputDir:   outDir,
		Workers:     32,
		BufferSize:  4096,
		AdminToken:  os.Getenv(f.tokenEnv),
		MetricsAddr: ":9095",
	}
	r, err := runner.New(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(out, "replay: scenario=%s target=%s out=%s\n", doc.Scenario.Name, target.Name, outDir)
	res, err := r.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(errOut, "replay error: %v\n", err)
	}
	if res != nil {
		fmt.Fprintf(out, "replay complete: %d events generated, %d submitted, %d succeeded\n",
			res.EventsGenerated, res.EventsSubmitted, res.EventsSucceeded)
		fmt.Fprintf(out, "artifacts written to: %s/\n", outDir)
	}
	return nil
}
