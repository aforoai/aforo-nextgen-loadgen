package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/server"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/version"
)

type serverFlags struct {
	listen  string
	workDir string

	supabaseURL            string
	supabaseAnonKey        string
	supabaseServiceRoleKey string

	s3Bucket     string
	s3Prefix     string
	manifestsDir string
	manifestPath string

	grafanaBaseURL string

	allowAnonymous bool
	staticUserID   string
	staticEmail    string
	staticRole     string
}

// newServerCommand wires `aforo-loadgen server` — the operator-facing
// HTTP control plane that Control Tower's /admin/loadgen pages talk
// to. The CLI is still the workhorse for 95% of usage; this server is
// the thin REST adapter so non-CLI users can trigger and inspect runs
// from a browser.
func newServerCommand(_ *GlobalFlags) *cobra.Command {
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the loadgen control-plane HTTP server (operator API for Control Tower)",
		Long: `server hosts the operator-facing REST API the Control Tower /admin/loadgen
pages call. Endpoints:

  POST /api/v1/runs              — trigger run (returns run_id, async)
  GET  /api/v1/runs              — list (paginated, filterable)
  GET  /api/v1/runs/{id}         — detail
  GET  /api/v1/runs/{id}/manifest — download run.json
  POST /api/v1/runs/{id}/cancel  — graceful cancel
  GET  /api/v1/scenarios         — list built-in scenarios
  GET  /api/v1/health            — liveness

Auth: every endpoint except /health requires a Supabase JWT in the
Authorization header. Read endpoints accept any Control Tower role;
trigger and cancel require the platform_admin role.

Storage:
  - Manifests: --manifests-dir local FS by default; pass --s3-bucket to
    push under s3://bucket/<prefix>/<run-id>.json (requires aws CLI).
  - Index: Supabase loadgen_runs table when --supabase-url is set,
    in-memory otherwise (local dev only).

Example:
  aforo-loadgen server \
    --listen :8095 \
    --supabase-url https://xxx.supabase.co \
    --supabase-anon-key $SUPABASE_ANON \
    --supabase-service-role-key $SUPABASE_SERVICE \
    --s3-bucket aforo-loadgen-runs \
    --grafana-base-url https://grafana.aforo.space
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.listen, "listen", ":8095", "host:port to bind the server")
	cmd.Flags().StringVar(&f.workDir, "work-dir", "/var/lib/aforo-loadgen/server", "directory for spawned worker outputs (one subdir per run)")
	cmd.Flags().StringVar(&f.supabaseURL, "supabase-url", "", "Supabase project URL — when empty, run with in-memory index (local dev only)")
	cmd.Flags().StringVar(&f.supabaseAnonKey, "supabase-anon-key", "", "Supabase anon key (for /auth/v1/user round-trip)")
	cmd.Flags().StringVar(&f.supabaseServiceRoleKey, "supabase-service-role-key", "", "Supabase service role key (for loadgen_runs reads/writes)")
	cmd.Flags().StringVar(&f.s3Bucket, "s3-bucket", "", "S3 bucket for manifest persistence; falls back to --manifests-dir when empty")
	cmd.Flags().StringVar(&f.s3Prefix, "s3-prefix", "loadgen-runs/", "S3 key prefix under the bucket")
	cmd.Flags().StringVar(&f.manifestsDir, "manifests-dir", "/var/lib/aforo-loadgen/manifests", "local manifest store directory (used when --s3-bucket is empty)")
	cmd.Flags().StringVar(&f.manifestPath, "manifest-path", "manifest.json", "path the worker reads to materialise seed bundles (server-side only — clients cannot override)")
	cmd.Flags().StringVar(&f.grafanaBaseURL, "grafana-base-url", "", "base URL of Grafana; if set, runs include a dashboard deep-link")
	cmd.Flags().BoolVar(&f.allowAnonymous, "allow-anonymous", false, "DEV ONLY — bypass JWT validation and treat every request as the configured static identity")
	cmd.Flags().StringVar(&f.staticUserID, "static-user-id", "00000000-0000-0000-0000-000000000000", "user id for --allow-anonymous")
	cmd.Flags().StringVar(&f.staticEmail, "static-email", "dev@aforo.local", "email for --allow-anonymous")
	cmd.Flags().StringVar(&f.staticRole, "static-role", "platform_admin", "role for --allow-anonymous (platform_admin | support_agent | finance_viewer | content_moderator)")
	return cmd
}

func runServer(parent context.Context, stdout, _ io.Writer, f *serverFlags) error {
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	auth, err := buildAuth(f, logger)
	if err != nil {
		return err
	}
	index, err := buildIndex(f, logger)
	if err != nil {
		return err
	}
	storage, err := buildStorage(f, logger)
	if err != nil {
		return err
	}

	catalog := server.NewEmbeddedCatalog()
	runner, err := server.NewLocalRunner(f.workDir, "", catalog.Names())
	if err != nil {
		return fmt.Errorf("init runner: %w", err)
	}

	cfg := server.Config{
		ListenAddr:   f.listen,
		Auth:         auth,
		Index:        index,
		Storage:      storage,
		Runner:       runner,
		ScenarioCat:  catalog,
		GrafanaURLFn: buildGrafanaURLFn(f.grafanaBaseURL),
		ManifestPath: f.manifestPath,
		Version:      version.Version,
		Logger:       logger,
	}

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting loadgen server",
		"listen", f.listen,
		"index_kind", indexKindOf(index),
		"storage_kind", storage.Kind(),
		"work_dir", f.workDir,
	)
	return srv.Start(ctx)
}

func buildAuth(f *serverFlags, logger *slog.Logger) (server.Authenticator, error) {
	if f.allowAnonymous {
		logger.Warn("--allow-anonymous is set; every request will be treated as the static identity. NEVER use in production.",
			"user_id", f.staticUserID, "role", f.staticRole)
		return &server.StaticAuthenticator{Identity: &server.Identity{UserID: f.staticUserID, Email: f.staticEmail, Role: f.staticRole}}, nil
	}
	if f.supabaseURL == "" {
		return nil, errors.New("--supabase-url is required (or pass --allow-anonymous for local dev)")
	}
	return server.NewSupabaseAuthenticator(f.supabaseURL, f.supabaseAnonKey, f.supabaseServiceRoleKey)
}

func buildIndex(f *serverFlags, logger *slog.Logger) (server.RunsIndex, error) {
	if f.supabaseURL == "" {
		logger.Warn("no --supabase-url; using in-memory runs index (local dev only — runs are lost on restart)")
		return server.NewMemoryIndex(), nil
	}
	if f.supabaseServiceRoleKey == "" {
		return nil, errors.New("--supabase-service-role-key is required when --supabase-url is set")
	}
	return server.NewSupabaseIndex(f.supabaseURL, f.supabaseServiceRoleKey)
}

func buildStorage(f *serverFlags, logger *slog.Logger) (server.ManifestStore, error) {
	if f.s3Bucket != "" {
		s, err := server.NewS3Store(f.s3Bucket, f.s3Prefix)
		if err != nil {
			return nil, fmt.Errorf("init s3 store: %w", err)
		}
		logger.Info("using S3 manifest store", "bucket", f.s3Bucket, "prefix", f.s3Prefix)
		return s, nil
	}
	logger.Info("using local manifest store", "dir", f.manifestsDir)
	return server.NewLocalStore(f.manifestsDir)
}

// buildGrafanaURLFn returns a function that, given a runID, computes
// the Grafana dashboard deep-link. Empty base URL → nil function →
// Run rows omit the grafana_url field.
func buildGrafanaURLFn(base string) func(string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil
	}
	return func(runID string) string {
		// var-runId is the templated variable in dashboards/loadgen-run.json.
		v := url.Values{"var-runId": {runID}}
		return base + "/d/loadgen-run/loadgen-run?" + v.Encode()
	}
}

func indexKindOf(idx server.RunsIndex) string {
	if _, ok := idx.(*server.SupabaseIndex); ok {
		return "supabase"
	}
	return "memory"
}
