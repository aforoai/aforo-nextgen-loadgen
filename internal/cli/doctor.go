// Session 7: doctor subcommand. Diagnoses local environment + target
// reachability before any seed/run work begins. Used standalone and as
// the first stage of `aforo-loadgen e2e`.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/doctor"
)

type doctorFlags struct {
	target     string
	tokenEnv   string
	timeoutSec int
	jsonOut    string
	jsonOnly   bool
}

func newDoctorCommand(_ *GlobalFlags) *cobra.Command {
	var f doctorFlags
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local environment and target reachability",
		Long: `Doctor probes every Aforo microservice the e2e flow touches, verifies
your bearer token is valid, and confirms platform infra (PostgreSQL,
Kafka, Redis, ClickHouse) is reachable through the services that report
component health.

What it checks:
  service:<name>            actuator/health on each microservice
  auth:bearer-token         AFORO_ADMIN_TOKEN authenticates to org-service
  auth:tenant-bootstrap     at least one tenant exists or can be created
  infra:db / kafka / redis  components reported by service actuators

Exit codes:
  0 — all critical checks pass (warnings allowed)
  1 — any critical check failed; see the printed remedy for next step

Examples:
  aforo-loadgen doctor --target local
  aforo-loadgen doctor --target staging --token-env AFORO_STAGING_TOKEN
  aforo-loadgen doctor --target local --json doctor.json
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, or full URL")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the bearer token")
	cmd.Flags().IntVar(&f.timeoutSec, "timeout-sec", 5, "per-probe HTTP timeout in seconds")
	cmd.Flags().StringVar(&f.jsonOut, "json", "", "if set, also write the report as JSON to this path")
	cmd.Flags().BoolVar(&f.jsonOnly, "json-only", false, "suppress text output (useful for orchestration)")
	return cmd
}

func runDoctor(ctx context.Context, out, errOut io.Writer, f *doctorFlags) error {
	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}
	token := os.Getenv(f.tokenEnv)

	d, err := doctor.New(doctor.Config{
		Target:          target,
		BearerToken:     token,
		PerCheckTimeout: time.Duration(f.timeoutSec) * time.Second,
	})
	if err != nil {
		return err
	}

	report := d.Run(ctx)

	if f.jsonOut != "" {
		if err := writeJSON(f.jsonOut, report); err != nil {
			fmt.Fprintf(errOut, "warning: failed to write %s: %v\n", f.jsonOut, err)
		}
	}
	if !f.jsonOnly {
		printDoctorReport(out, report)
	}
	if report.HasCritical() {
		// Cobra surfaces the returned error and exits with code 1; we don't
		// call os.Exit directly so deferred cleanups in tests still run.
		return fmt.Errorf("doctor: %d critical check(s) failed", countCritical(report))
	}
	return nil
}

func printDoctorReport(out io.Writer, r *doctor.Report) {
	fmt.Fprintf(out, "doctor — target: %s\n", r.Target)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tSEVERITY\tCHECK\tDETAIL")
	for _, c := range r.Checks {
		detail := c.Detail
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Status, c.Severity, c.Name, truncate(detail, 80))
	}
	_ = tw.Flush()

	// Remedies, if any, surface below the table — operators copy/paste these.
	var remedies []string
	for _, c := range r.Checks {
		if c.Status == doctor.StatusFail && c.Remedy != "" {
			remedies = append(remedies, fmt.Sprintf("  • [%s] %s", c.Name, c.Remedy))
		}
	}
	if len(remedies) > 0 {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "remedies:")
		for _, r := range remedies {
			fmt.Fprintln(out, r)
		}
	}

	pass, fail, skip := tally(r.Checks)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "summary: %s — pass=%d fail=%d skip=%d  (%.0fms)\n",
		r.Overall, pass, fail, skip, float64(r.Elapsed.Milliseconds()))
}

func countCritical(r *doctor.Report) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == doctor.StatusFail && c.Severity == doctor.SeverityCritical {
			n++
		}
	}
	return n
}

func tally(rows []doctor.CheckResult) (pass, fail, skip int) {
	for _, c := range rows {
		switch c.Status {
		case doctor.StatusOK:
			pass++
		case doctor.StatusFail:
			fail++
		case doctor.StatusSkip:
			skip++
		}
	}
	return
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	return os.WriteFile(path, data, 0o644) // #nosec G306 — operator artifact
}
