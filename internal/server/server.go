package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config bundles everything Server needs to run. Build via the CLI
// flags in cmd/aforo-loadgen/server.go; the zero value is unsafe.
type Config struct {
	ListenAddr string

	Auth         Authenticator
	Index        RunsIndex
	Storage      ManifestStore
	Runner       *LocalRunner
	ScenarioCat  ScenarioCatalog
	GrafanaURLFn func(runID string) string

	// ManifestPath is the absolute or relative path the worker reads
	// when materialising the seed bundle. The HTTP API does not
	// accept a per-request override — see the comment on
	// TriggerRequest for the security rationale.
	ManifestPath string

	// Version reported by /health.
	Version string

	// Logger may be nil; falls back to slog.Default().
	Logger *slog.Logger
}

// ScenarioCatalog abstracts the built-in scenario catalog so tests can
// inject fixtures without dragging in the embedded YAML files.
type ScenarioCatalog interface {
	List() []ScenarioInfo
	Has(name string) bool
}

// Server is the long-running HTTP server. Construct via New, then
// call Start. SIGINT cancellation is the caller's responsibility —
// Start respects ctx.Done().
type Server struct {
	cfg Config
	mux *http.ServeMux

	mu        sync.RWMutex
	running   map[string]*runState
	observeWG sync.WaitGroup // gates shutdown on observe goroutines
	logger    *slog.Logger
}

// runState tracks an in-flight worker subprocess. Persisted state
// lives in Index; this struct is the in-memory handle we use to
// cancel and forward exit status.
//
// `cancelled` is set atomically by the cancel handler / shutdown
// path and read by observeRun to decide between status=completed
// (worker exited on its own) and status=cancelled (we asked it to).
// A bool guarded by the server's mutex would do, but using sync/atomic
// keeps the cancel path lock-free.
type runState struct {
	runID      string
	handle     *ProcHandle
	cancelOnce sync.Once
	cancelled  atomic.Bool
}

// New constructs a Server with validated config. Returns an error if
// any required dependency is missing.
func New(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("listen addr required")
	}
	if cfg.Auth == nil {
		return nil, errors.New("authenticator required")
	}
	if cfg.Index == nil {
		return nil, errors.New("runs index required")
	}
	if cfg.Storage == nil {
		return nil, errors.New("manifest storage required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("local runner required")
	}
	if cfg.ScenarioCat == nil {
		return nil, errors.New("scenario catalog required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), running: make(map[string]*runState), logger: cfg.Logger}
	s.routes()
	return s, nil
}

// Handler returns the configured http.Handler — exposed so tests can
// drive endpoints via httptest.NewServer without binding a port.
func (s *Server) Handler() http.Handler { return s.mux }

// Start binds the listen addr and serves until ctx is cancelled.
// Active worker processes are SIGINT'd on shutdown; the call blocks
// until they exit so the run.json artifact is durable on disk before
// the process returns.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("loadgen server listening", "addr", s.cfg.ListenAddr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signalled, draining active runs")
		s.cancelAllRuns()
		// Wait for observe goroutines to flush final status before
		// closing the HTTP server. Without this, a row can stay stuck
		// in "running" if SIGTERM races with the observer's last
		// Update call.
		s.observeWG.Wait()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/scenarios", s.requireRead(s.handleListScenarios))
	s.mux.HandleFunc("GET /api/v1/runs", s.requireRead(s.handleListRuns))
	s.mux.HandleFunc("POST /api/v1/runs", s.requireAdmin(s.handleTriggerRun))
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.requireRead(s.handleGetRun))
	s.mux.HandleFunc("GET /api/v1/runs/{id}/manifest", s.requireRead(s.handleGetManifest))
	s.mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.requireAdmin(s.handleCancelRun))
}

// ────────── Middleware ──────────

type ctxKey int

const ctxIdentityKey ctxKey = 1

func identityFrom(ctx context.Context) *Identity {
	v, _ := ctx.Value(ctxIdentityKey).(*Identity)
	return v
}

// requireRead lets any internal role through; the read-side endpoints
// are safe to expose to support_agent and finance_viewer.
func (s *Server) requireRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.cfg.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !id.IsInternal() {
			writeErr(w, http.StatusForbidden, "no Control Tower role assigned")
			return
		}
		ctx := context.WithValue(r.Context(), ctxIdentityKey, id)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin gates write endpoints to platform_admin only.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.cfg.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !id.IsPlatformAdmin() {
			writeErr(w, http.StatusForbidden, "platform_admin role required")
			return
		}
		ctx := context.WithValue(r.Context(), ctxIdentityKey, id)
		next(w, r.WithContext(ctx))
	}
}

// ────────── Handlers ──────────

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	active := len(s.running)
	s.mu.RUnlock()

	indexKind := "memory"
	if _, ok := s.cfg.Index.(*SupabaseIndex); ok {
		indexKind = "supabase"
	}
	resp := HealthResponse{
		Status:     "ok",
		Version:    s.cfg.Version,
		ActiveRuns: active,
		Storage:    s.cfg.Storage.Kind(),
		Index:      indexKind,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListScenarios(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"scenarios": s.cfg.ScenarioCat.List()})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	q := ListQuery{
		Status:   r.URL.Query().Get("status"),
		Scenario: r.URL.Query().Get("scenario"),
		Page:     page,
		PerPage:  perPage,
	}
	resp, err := s.cfg.Index.List(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.cfg.Index.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "run not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.cfg.Index.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "run not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run.ManifestS3Path == "" {
		writeErr(w, http.StatusNotFound, "manifest not yet written (run still in progress?)")
		return
	}
	rc, err := s.cfg.Storage.Get(r.Context(), run.ManifestS3Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read manifest: "+err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+id+".json\"")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	var req TriggerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	req.Scenario = strings.TrimSpace(req.Scenario)
	req.Target = strings.TrimSpace(req.Target)
	if req.Scenario == "" {
		writeErr(w, http.StatusBadRequest, "scenario is required")
		return
	}
	if req.Target == "" {
		writeErr(w, http.StatusBadRequest, "target is required")
		return
	}
	if !s.cfg.ScenarioCat.Has(req.Scenario) {
		writeErr(w, http.StatusBadRequest, "unknown scenario: "+req.Scenario)
		return
	}
	// High-TPS guardrail. The CLI has --i-know-what-im-doing for the
	// same purpose; the server requires acknowledge_high_tps=true so
	// an accidental click in the UI doesn't spin up a 10K-TPS run.
	if info, ok := s.scenarioInfo(req.Scenario); ok && info.HighTPS && !req.Acknowledge {
		writeErr(w, http.StatusPreconditionFailed,
			fmt.Sprintf("scenario %q is high-TPS (%d); set acknowledge_high_tps=true to confirm", req.Scenario, info.TargetTPS))
		return
	}

	id := identityFrom(r.Context())
	runID := genRunID(req.Scenario)
	now := time.Now().UTC()

	// Manifest path is server-configured only — see TriggerRequest's
	// godoc for the security rationale. Falls back to the canonical
	// default when the operator didn't pass --manifest-path on the
	// CLI.
	manifestPath := s.cfg.ManifestPath
	if manifestPath == "" {
		manifestPath = "manifest.json"
	}

	row := Run{
		RunID:       runID,
		Scenario:    req.Scenario,
		Target:      req.Target,
		Status:      RunStatusQueued,
		TriggeredBy: id.UserID,
		StartedAt:   now,
	}
	if s.cfg.GrafanaURLFn != nil {
		row.GrafanaURL = s.cfg.GrafanaURLFn(runID)
	}
	if err := s.cfg.Index.Insert(r.Context(), row); err != nil {
		writeErr(w, http.StatusInternalServerError, "index insert: "+err.Error())
		return
	}

	handle, err := s.cfg.Runner.Spawn(r.Context(), runID, req, manifestPath)
	if err != nil {
		row.Status = RunStatusFailed
		end := time.Now().UTC()
		row.EndedAt = &end
		_ = s.cfg.Index.Update(r.Context(), row)
		writeErr(w, http.StatusInternalServerError, "spawn worker: "+err.Error())
		return
	}

	st := &runState{runID: runID, handle: handle}
	s.mu.Lock()
	s.running[runID] = st
	s.mu.Unlock()

	s.observeWG.Add(1)
	go s.observeRun(st, row)

	writeJSON(w, http.StatusAccepted, TriggerResponse{RunID: runID, Status: RunStatusQueued})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	st, ok := s.running[id]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no active run with id "+id)
		return
	}
	st.cancelled.Store(true)
	st.cancelOnce.Do(func() {
		_ = st.handle.Cancel()
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id, "status": "cancel-signalled"})
}

// ────────── Run Lifecycle ──────────

// observeRun is launched as a goroutine per spawned worker. It flips
// the index row to running, waits for completion, harvests the
// run.json artifact, pushes it into the manifest store, and writes
// the final status.
//
// observeWG.Done is deferred so Server.Start's shutdown path can
// gate on every observer finishing — without this, a SIGTERM would
// shut the HTTP server down before we'd written the final status,
// leaving rows stuck in "running".
func (s *Server) observeRun(st *runState, row Run) {
	defer s.observeWG.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runID := st.runID
	h := st.handle

	row.Status = RunStatusRunning
	if err := s.cfg.Index.Update(ctx, row); err != nil {
		s.logger.Warn("index update running", "run_id", runID, "err", err)
	}

	exitErr := h.Wait()

	// Worker is done — remove from active set first so /cancel
	// returns 404 cleanly instead of racing with our final write.
	s.mu.Lock()
	delete(s.running, runID)
	s.mu.Unlock()

	end := time.Now().UTC()
	row.EndedAt = &end

	// Cancellation status is decided by whether the operator (or
	// shutdown path) asked us to stop, NOT by the worker exit code.
	// The run engine traps SIGINT and exits 0 cleanly — we'd otherwise
	// classify a graceful cancel as a successful completion.
	switch {
	case st.cancelled.Load():
		row.Status = RunStatusCancelled
	case exitErr != nil:
		row.Status = RunStatusFailed
	default:
		row.Status = RunStatusCompleted
	}

	// Even on failure / cancellation we try to harvest run.json —
	// the run engine writes a partial on SIGINT.
	if art, err := h.ReadRunJSON(); err == nil {
		row = applyArtifact(row, art)
	} else {
		s.logger.Info("no run.json artifact", "run_id", runID, "err", err)
	}

	// Push manifest into the storage layer regardless of outcome —
	// even partial manifests are valuable for postmortem.
	if pr, pw := io.Pipe(); pr != nil {
		go func() {
			defer pw.Close()
			_ = h.CopyRunJSON(pw)
		}()
		if uri, err := s.cfg.Storage.Put(ctx, runID, pr); err == nil {
			row.ManifestS3Path = uri
		} else {
			s.logger.Warn("manifest store put", "run_id", runID, "err", err)
		}
	}

	if err := s.cfg.Index.Update(ctx, row); err != nil {
		s.logger.Error("final index update", "run_id", runID, "err", err)
	}
}

// applyArtifact merges fields harvested from run.json into the index
// row. Only fields populated by the artifact are touched; queued-time
// metadata stays intact.
func applyArtifact(row Run, art RunArtifact) Run {
	row.EventsSent = art.EventsSubmitted
	row.EventsSucceeded = art.EventsSucceeded
	row.P99Ms = int(art.LatencyP99ms)
	if art.Outcome != "" {
		row.OverallOutcome = RunOutcome(art.Outcome)
	}
	if art.PerArchetype != nil {
		row.PerArchetypeSummary = art.PerArchetype
	}
	if len(art.NegativePathCounts) > 0 {
		out := make(map[string]any, len(art.NegativePathCounts))
		for k, v := range art.NegativePathCounts {
			out[k] = v
		}
		row.PerNegativePathStats = out
	}
	if len(art.Assertions) > 0 {
		row.Assertions = make([]Assertion, 0, len(art.Assertions))
		for _, a := range art.Assertions {
			row.Assertions = append(row.Assertions, Assertion{Name: a.Name, Outcome: a.Outcome, Message: a.Message})
		}
	}
	return row
}

func (s *Server) scenarioInfo(name string) (ScenarioInfo, bool) {
	for _, sc := range s.cfg.ScenarioCat.List() {
		if sc.Name == name {
			return sc, true
		}
	}
	return ScenarioInfo{}, false
}

// cancelAllRuns is invoked on shutdown. It sends SIGINT to every
// active worker and waits up to 30s per run for graceful exit.
func (s *Server) cancelAllRuns() {
	s.mu.Lock()
	handles := make([]*runState, 0, len(s.running))
	for _, st := range s.running {
		handles = append(handles, st)
	}
	s.mu.Unlock()

	// Mark every active run as cancelled so observeRun classifies
	// the worker exit correctly when its blocking h.Wait() returns.
	for _, st := range handles {
		st.cancelled.Store(true)
		st.cancelOnce.Do(func() { _ = st.handle.Cancel() })
	}

	// Force-kill any worker that doesn't acknowledge SIGINT within
	// 30 seconds. We sleep 30s then call Kill — Kill is idempotent
	// via doneOnce so the happy-path workers that have already
	// exited are unaffected.
	go func() {
		time.Sleep(30 * time.Second)
		for _, st := range handles {
			s.mu.RLock()
			_, stillActive := s.running[st.runID]
			s.mu.RUnlock()
			if stillActive {
				s.logger.Warn("worker did not exit in 30s, killing", "run_id", st.runID)
				st.handle.Kill()
			}
		}
	}()
}

// ────────── Helpers ──────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
