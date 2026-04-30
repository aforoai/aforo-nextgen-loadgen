package coord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/driver"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// RunnerWorkerHandler is the production WorkerHandler. Each Accept call
// constructs a runner.Config from the Assignment and runs the engine in
// a background goroutine. Heartbeats reflect the live runner's progress.
//
// One handler per worker process. Subsequent Accepts after the first
// fail with "worker is busy" — the design assumes one assignment per
// worker per coordinator run.
type RunnerWorkerHandler struct {
	mu      sync.Mutex
	current *runState

	// AdminToken is the Aforo admin bearer token, used as the fallback
	// when a generated event has no per-key token. Loaded from the
	// worker's env at startup.
	AdminToken string

	// OutputDir is the per-run scratch dir. Each Assignment creates a
	// subdirectory under this.
	OutputDir string

	// WorkerID is the stable identifier returned in every response.
	WorkerID string
}

type runState struct {
	runID       string
	assignedTen []string
	cancel      context.CancelFunc
	ctx         context.Context
	doneCh      chan struct{}
	finalReport *Report
	state       string // mirrors Heartbeat.State
	startedAt   time.Time
	stoppedAt   time.Time

	// liveStatsMu protects the snapshot-able fields below — heartbeats
	// read from the running engine via these scalars rather than dipping
	// into runner internals.
	liveStatsMu sync.Mutex
	eventsSent  int64
	eventsFail  int64
	tps         float64
	p99Ms       float64
}

// Accept wires the assignment into a runner.Config and starts the
// engine. Returns Accepted=false on any validation failure.
func (h *RunnerWorkerHandler) Accept(ctx context.Context, a *Assignment) Acceptance {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current != nil && !h.current.isTerminal() {
		return Acceptance{
			Accepted: false,
			WorkerID: h.WorkerID,
			Reason:   fmt.Sprintf("worker busy with run %s state=%s", h.current.runID, h.current.state),
		}
	}

	doc, err := scenario.LoadFromBytes([]byte(a.ScenarioYAML))
	if err != nil {
		return Acceptance{Accepted: false, WorkerID: h.WorkerID, Reason: "scenario decode: " + err.Error()}
	}
	if errs := scenario.Validate(doc); errs.HasErrors() {
		return Acceptance{Accepted: false, WorkerID: h.WorkerID, Reason: "scenario invalid: " + errs.Error()}
	}

	manifest, err := seed.LoadManifestFromBytes([]byte(a.ManifestJSON))
	if err != nil {
		return Acceptance{Accepted: false, WorkerID: h.WorkerID, Reason: "manifest decode: " + err.Error()}
	}
	subManifest := filterManifestByTenants(manifest, a.TenantIDs)
	if len(subManifest.Tenants) == 0 {
		return Acceptance{
			Accepted: false,
			WorkerID: h.WorkerID,
			Reason:   "tenant intersection produced empty manifest — coordinator and worker have mismatched manifest data",
		}
	}

	target, err := aforo.ResolveTarget(a.TargetName)
	if err != nil {
		return Acceptance{Accepted: false, WorkerID: h.WorkerID, Reason: "target resolve: " + err.Error()}
	}

	// Skew check — informational. Coordinator passes its Now; we
	// compare to ours. >5min is a config bug; we accept anyway and
	// surface the skew so the operator can see it in the run.json.
	skew := time.Since(a.Now)
	if skew < -5*time.Minute || skew > 5*time.Minute {
		// Continue, but include the skew in the response.
	}

	// Per-worker TPS clone of the scenario.
	scn := *doc.Scenario
	scn.TargetTPS = a.PerWorkerTargetTPS
	scn.Name = scn.Name + "-" + a.WorkerID

	cfgCtx, cancel := context.WithCancel(context.Background())
	rs := &runState{
		runID:       a.RunID,
		assignedTen: a.TenantIDs,
		cancel:      cancel,
		ctx:         cfgCtx,
		doneCh:      make(chan struct{}),
		state:       "ramp",
		startedAt:   time.Now(),
	}
	h.current = rs

	cfg := runner.Config{
		Scenario:         &scn,
		Manifest:         subManifest,
		Target:           target,
		OutputDir:        h.OutputDir + "/" + a.WorkerID,
		Workers:          0,
		DurationOverride: a.DurationOverride,
		AdminToken:       h.AdminToken,
		// Distributed mode prefers the parent runner's drivers — we
		// don't enable webhook receivers here; that's a single-host
		// concern (the manifest's webhook_sources sidecar is loaded
		// from disk in single-host mode).
		WebhookSources: map[string]driver.WebhookSource{},
	}

	r, err := runner.New(cfg)
	if err != nil {
		cancel()
		h.current = nil
		return Acceptance{Accepted: false, WorkerID: h.WorkerID, Reason: "runner init: " + err.Error()}
	}

	go h.runLoop(rs, r)

	return Acceptance{
		Accepted:   true,
		WorkerID:   h.WorkerID,
		WorkerSkew: skew,
	}
}

func (h *RunnerWorkerHandler) runLoop(rs *runState, r *runner.Runner) {
	defer close(rs.doneCh)
	rs.liveStatsMu.Lock()
	rs.state = "running"
	rs.liveStatsMu.Unlock()

	res, err := r.Run(rs.ctx)
	rs.liveStatsMu.Lock()
	rs.stoppedAt = time.Now()
	if err != nil && !errors.Is(err, context.Canceled) {
		rs.state = "failed"
	} else if errors.Is(err, context.Canceled) {
		rs.state = "aborted"
	} else {
		rs.state = "done"
	}
	if res != nil {
		// Project the runner result into the wire-shape Report.
		rep := &Report{
			WorkerID:         h.WorkerID,
			RunID:            rs.runID,
			StartedAt:        rs.startedAt,
			StoppedAt:        rs.stoppedAt,
			EventsGenerated:  res.EventsGenerated,
			EventsSubmitted:  res.EventsSubmitted,
			EventsSucceeded:  res.EventsSucceeded,
			EventsFailed:     res.EventsFailed,
			ClientErrors:     res.ClientErrors,
			ServerErrors:     res.ServerErrors,
			TransportErrors:  res.TransportFailures,
			LatencyP50Ms:     res.LatencyP50ms,
			LatencyP90Ms:     res.LatencyP90ms,
			LatencyP99Ms:     res.LatencyP99ms,
			LatencyMaxMs:     res.LatencyMaxMs,
			PerArchetype:     res.PerArchetype,
			PerProductType:   res.PerProductType,
			PerIngestionPath: res.PerIngestionPath,
			Errors:           res.Errors,
		}
		rs.finalReport = rep
		// Update live stats one last time so a heartbeat right at the
		// end shows the final numbers.
		rs.eventsSent = res.EventsSucceeded
		rs.eventsFail = res.EventsFailed
		rs.p99Ms = res.LatencyP99ms
	}
	rs.liveStatsMu.Unlock()
}

// Heartbeat returns the current runState as a Heartbeat. O(1).
func (h *RunnerWorkerHandler) Heartbeat() Heartbeat {
	h.mu.Lock()
	rs := h.current
	h.mu.Unlock()
	if rs == nil {
		return Heartbeat{WorkerID: h.WorkerID, State: "idle"}
	}
	rs.liveStatsMu.Lock()
	defer rs.liveStatsMu.Unlock()
	return Heartbeat{
		WorkerID:     h.WorkerID,
		RunID:        rs.runID,
		State:        rs.state,
		UptimeSec:    int64(time.Since(rs.startedAt).Seconds()),
		EventsSent:   rs.eventsSent,
		EventsFail:   rs.eventsFail,
		CurrentTPS:   rs.tps,
		LatencyP99Ms: rs.p99Ms,
	}
}

// Abort cancels the in-progress run. Idempotent.
func (h *RunnerWorkerHandler) Abort(ctx context.Context, reason string) AbortResponse {
	h.mu.Lock()
	rs := h.current
	h.mu.Unlock()
	if rs == nil || rs.cancel == nil {
		return AbortResponse{Accepted: true, WorkerID: h.WorkerID}
	}
	rs.cancel()
	// Wait briefly for the run loop to acknowledge — the coordinator's
	// abort handler should not block forever.
	select {
	case <-rs.doneCh:
	case <-time.After(10 * time.Second):
	}
	return AbortResponse{Accepted: true, WorkerID: h.WorkerID}
}

// LastReport returns the final report once available.
func (h *RunnerWorkerHandler) LastReport() *Report {
	h.mu.Lock()
	rs := h.current
	h.mu.Unlock()
	if rs == nil {
		return nil
	}
	rs.liveStatsMu.Lock()
	defer rs.liveStatsMu.Unlock()
	if rs.finalReport == nil {
		return nil
	}
	// Defensive copy.
	cp := *rs.finalReport
	return &cp
}

// isTerminal reports whether the runState has reached a non-running state.
// Caller does not need to hold the lock; reads are racy but correct for
// the "is the worker free?" question — heartbeat and Accept both
// re-check under the parent mutex.
func (rs *runState) isTerminal() bool {
	rs.liveStatsMu.Lock()
	defer rs.liveStatsMu.Unlock()
	return isTerminalState(rs.state)
}

// filterManifestByTenants builds a sub-manifest containing only the
// tenants whose IDs appear in keepIDs. The returned manifest's
// summary is recomputed.
func filterManifestByTenants(m *seed.Manifest, keepIDs []string) *seed.Manifest {
	keep := map[string]struct{}{}
	for _, id := range keepIDs {
		keep[id] = struct{}{}
	}
	sub := seed.NewManifest(m.RunID, m.Target, m.Scenario, m.CreatedAt)
	for _, t := range m.Tenants {
		if _, ok := keep[t.TenantID]; ok {
			sub.AppendTenant(t)
		}
	}
	sub.Finalize()
	return sub
}
