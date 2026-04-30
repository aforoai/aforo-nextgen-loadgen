package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/coord"
)

type workerFlags struct {
	listen     string
	out        string
	tokenEnv   string
	workerID   string

	tlsCert string
	tlsKey  string
	tlsCA   string
}

// newWorkerCommand wires `aforo-loadgen worker` — the per-host server
// that accepts an Assignment from a coordinator.
func newWorkerCommand(_ *GlobalFlags) *cobra.Command {
	var f workerFlags
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Listen for coordinator assignments (multi-machine distributed mode)",
		Long: `worker hosts the per-node server that receives an Assignment from the
coordinator, runs the partitioned scenario locally, and reports back.

The worker accepts exactly one assignment per coordinator run. Subsequent
assignments while a run is in progress are rejected with HTTP 409.

Example:
  aforo-loadgen worker \
    --listen :7070 \
    --out /var/lib/aforo-loadgen \
    --tls-cert tls/worker.pem --tls-key tls/worker.key --tls-ca tls/ca.pem

The worker process is meant to run as a long-lived service on each
perf-cluster node. SIGINT/SIGTERM aborts any in-progress run cleanly
before the process exits.
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.listen, "listen", ":7070", "host:port to bind the worker server (mTLS)")
	cmd.Flags().StringVar(&f.out, "out", "/var/lib/aforo-loadgen", "output dir for per-run artifacts")
	cmd.Flags().StringVar(&f.workerID, "worker-id", "", "stable worker id (default: hostname)")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding admin bearer token (fallback per-event auth)")
	cmd.Flags().StringVar(&f.tlsCert, "tls-cert", "", "worker server cert PEM")
	cmd.Flags().StringVar(&f.tlsKey, "tls-key", "", "worker server key PEM")
	cmd.Flags().StringVar(&f.tlsCA, "tls-ca", "", "CA bundle PEM (verifies coordinator's client cert)")
	return cmd
}

func runWorker(ctx context.Context, out, errOut io.Writer, f *workerFlags) error {
	if _, err := coord.ParseListenAddr(f.listen); err != nil {
		return err
	}
	wid := f.workerID
	if wid == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			return errors.New("--worker-id is empty and hostname is unavailable; set --worker-id explicitly")
		}
		wid = hostname
	}

	if err := os.MkdirAll(f.out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", f.out, err)
	}

	handler := &coord.RunnerWorkerHandler{
		WorkerID:   wid,
		OutputDir:  f.out,
		AdminToken: os.Getenv(f.tokenEnv),
	}
	server, err := coord.NewWorkerServer(coord.WorkerServerConfig{
		ListenAddr: f.listen,
		MTLS: coord.MTLSConfig{
			CertFile: f.tlsCert,
			KeyFile:  f.tlsKey,
			CAFile:   f.tlsCA,
		},
		Handler: handler,
		Logger:  func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) },
	})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(out, "aforo-loadgen worker %s listening on %s (mTLS)\n", wid, f.listen)
	if err := server.Start(ctx); err != nil {
		return fmt.Errorf("worker server: %w", err)
	}
	fmt.Fprintln(out, "worker shut down cleanly")
	return nil
}
