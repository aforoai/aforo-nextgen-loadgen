package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"
)

// CoordinatorConfig is the construction-time bag for the coordinator
// orchestrator.
type CoordinatorConfig struct {
	// WorkerAddrs is the list of worker host:port strings. Required.
	// Order is preserved for deterministic assignment.
	WorkerAddrs []string

	// MTLS is the client-side TLS material used to connect to every
	// worker.
	MTLS MTLSConfig

	// HeartbeatInterval is the poll cadence. 0 → DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration

	// DropoutTimeout is the time without a successful heartbeat after
	// which a worker is declared dropped. 0 → DefaultDropoutTimeout.
	DropoutTimeout time.Duration

	// Now is the time source. nil → time.Now.
	Now func() time.Time

	// Logger receives one line per dispatch / heartbeat / dropout.
	// nil → discard.
	Logger func(format string, args ...any)
}

// Coordinator orchestrates the multi-worker run. Lifecycle:
//
//  1. NewCoordinator validates config + dials every worker.
//  2. Dispatch sends one Assignment per worker.
//  3. PollUntilDone heartbeats every HeartbeatInterval until every
//     worker reports state="done" or drops out.
//  4. AggregateReports collects final reports and merges them.
//
// A worker dropout is logged but does not abort the run. The
// coordinator marks the worker's tenant range as "incomplete" in the
// merged report so post-run analysis sees the gap.
type Coordinator struct {
	cfg     CoordinatorConfig
	now     func() time.Time
	logger  func(format string, args ...any)

	clientsMu sync.Mutex
	clients   map[string]*WorkerClient // addr → client

	stateMu  sync.Mutex
	workers  []*workerState
	runID    string
}

type workerState struct {
	addr           string
	id             string
	assignedTenants []string
	lastHeartbeat  time.Time
	state          string // mirrors Heartbeat.State
	dropoutLogged  bool
	report         *Report
}

// NewCoordinator constructs a Coordinator and dials every worker. Returns
// an error if any worker is unreachable — fail-closed before assigning.
func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	if len(cfg.WorkerAddrs) == 0 {
		return nil, errors.New("coordinator: at least one worker address is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.DropoutTimeout <= 0 {
		cfg.DropoutTimeout = DefaultDropoutTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}

	// Dial every worker up-front. A bad cert on one worker should fail
	// the run before any work is assigned.
	for _, addr := range cfg.WorkerAddrs {
		if err := pingTLS(addr, cfg.MTLS); err != nil {
			return nil, fmt.Errorf("coordinator: ping %s: %w", addr, err)
		}
	}

	c := &Coordinator{
		cfg:     cfg,
		now:     cfg.Now,
		logger:  cfg.Logger,
		clients: map[string]*WorkerClient{},
		runID:   randomRunID("run"),
	}
	for i, addr := range cfg.WorkerAddrs {
		client, err := NewWorkerClient(addr, cfg.MTLS, 0)
		if err != nil {
			return nil, fmt.Errorf("coordinator: client[%s]: %w", addr, err)
		}
		c.clients[addr] = client
		c.workers = append(c.workers, &workerState{
			addr: addr,
			id:   fmt.Sprintf("worker-%d", i),
		})
	}
	return c, nil
}

// RunID returns the coordinator-generated run id (also embedded in
// every Assignment).
func (c *Coordinator) RunID() string { return c.runID }

// Workers returns a defensive copy of the worker addresses + ids in
// dispatch order.
func (c *Coordinator) Workers() []string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	out := make([]string, len(c.workers))
	for i, w := range c.workers {
		out[i] = w.addr
	}
	return out
}

// PartitionConfig is the bag for Dispatch — what to partition and how.
type PartitionConfig struct {
	// TenantIDs is the full set of tenant ids that need traffic. The
	// coordinator partitions these across the worker fleet via stable
	// fnv32a hash so re-runs produce identical assignments (modulo
	// dropout reassignment).
	TenantIDs []string

	// ScenarioYAML is the full scenario document, serialized.
	ScenarioYAML string

	// ManifestJSON is the full manifest, serialized.
	ManifestJSON string

	// TargetName is the Aforo platform target.
	TargetName string

	// TotalTargetTPS is the scenario's headline TPS, split across
	// workers per partition.
	TotalTargetTPS int

	// DurationOverride lets the coordinator shorten the run uniformly.
	// Workers honor this on their local runner.Config.
	DurationOverride time.Duration
}

// Dispatch partitions tenants across workers and POSTs one Assignment
// per worker. Returns the set of accepted Acceptances; rejected workers
// surface through the returned error. A partial-success rollout is OK
// — the coordinator can run with fewer workers, but it surfaces the
// rejections so operators see them.
func (c *Coordinator) Dispatch(ctx context.Context, pc PartitionConfig) (map[string]Acceptance, error) {
	chunks, err := partitionTenants(pc.TenantIDs, len(c.workers))
	if err != nil {
		return nil, err
	}
	tps := splitTPS(pc.TotalTargetTPS, len(c.workers))

	accepted := map[string]Acceptance{}
	var rejections []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, w := range c.workers {
		wg.Add(1)
		go func(idx int, w *workerState) {
			defer wg.Done()
			a := &Assignment{
				RunID:              c.runID,
				WorkerID:           w.id,
				ScenarioYAML:       pc.ScenarioYAML,
				ManifestJSON:       pc.ManifestJSON,
				TenantIDs:          chunks[idx],
				TargetName:         pc.TargetName,
				PerWorkerTargetTPS: tps[idx],
				DurationOverride:   pc.DurationOverride,
				Now:                c.now(),
			}
			rsp, err := c.clients[w.addr].Assign(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rejections = append(rejections, fmt.Errorf("worker %s: %w", w.addr, err))
				return
			}
			accepted[w.addr] = rsp
			c.stateMu.Lock()
			w.assignedTenants = chunks[idx]
			w.state = "running"
			w.lastHeartbeat = c.now()
			c.stateMu.Unlock()
			c.logger("coordinator: dispatched %d tenants @ %d tps to %s", len(chunks[idx]), tps[idx], w.addr)
		}(i, w)
	}
	wg.Wait()

	if len(accepted) == 0 {
		return accepted, fmt.Errorf("coordinator: no worker accepted; rejections: %v", rejections)
	}
	if len(rejections) > 0 {
		return accepted, fmt.Errorf("coordinator: %d/%d worker(s) rejected: %v",
			len(rejections), len(c.workers), rejections)
	}
	return accepted, nil
}

// PollUntilDone heartbeats every HeartbeatInterval until every worker
// reaches a terminal state or drops out (lastHeartbeat older than
// DropoutTimeout). Returns when ctx is cancelled OR all workers are
// terminal.
//
// Dropout handling: when a worker misses DropoutTimeout, the
// coordinator marks it dropped and continues. A future enhancement is
// to redistribute the dropped tenants to surviving workers; v1 marks
// the gap and continues so the headline TPS does not crater.
func (c *Coordinator) PollUntilDone(ctx context.Context) error {
	t := time.NewTicker(c.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if c.pollOnce(ctx) {
				return nil
			}
		}
	}
}

// pollOnce fires one heartbeat round. Returns true if every worker is
// terminal (done | aborted | failed | dropped).
func (c *Coordinator) pollOnce(ctx context.Context) bool {
	var wg sync.WaitGroup
	for _, w := range c.workers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.heartbeatOne(ctx, w)
		}()
	}
	wg.Wait()
	return c.allTerminal()
}

func (c *Coordinator) heartbeatOne(ctx context.Context, w *workerState) {
	c.stateMu.Lock()
	st := w.state
	c.stateMu.Unlock()
	if isTerminalState(st) {
		return
	}

	hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hb, err := c.clients[w.addr].Heartbeat(hbCtx)
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err != nil {
		// One missed heartbeat is not fatal. Check the dropout window.
		if !w.lastHeartbeat.IsZero() && c.now().Sub(w.lastHeartbeat) > c.cfg.DropoutTimeout {
			if !w.dropoutLogged {
				c.logger("coordinator: WORKER DROPOUT %s (last hb %s ago): %v",
					w.addr, c.now().Sub(w.lastHeartbeat), err)
				w.dropoutLogged = true
			}
			w.state = "dropped"
		}
		return
	}
	w.lastHeartbeat = c.now()
	w.state = hb.State
	c.logger("coordinator: hb %s state=%s tps=%.0f p99=%.1fms events=%d",
		w.addr, hb.State, hb.CurrentTPS, hb.LatencyP99Ms, hb.EventsSent)
}

// AggregateReports fetches the final Report from each non-dropped worker
// and merges them into a single AggregateResult.
func (c *Coordinator) AggregateReports(ctx context.Context) AggregateResult {
	c.stateMu.Lock()
	workers := make([]*workerState, len(c.workers))
	copy(workers, c.workers)
	c.stateMu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	reports := make([]*Report, 0, len(workers))
	dropouts := []string{}
	for _, w := range workers {
		w := w
		c.stateMu.Lock()
		dropped := w.state == "dropped"
		c.stateMu.Unlock()
		if dropped {
			dropouts = append(dropouts, w.addr)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			rep, ok, err := c.clients[w.addr].FetchReport(fctx)
			if err != nil {
				c.logger("coordinator: fetch report from %s: %v", w.addr, err)
				return
			}
			if !ok {
				c.logger("coordinator: %s did not return a final report", w.addr)
				return
			}
			mu.Lock()
			reports = append(reports, rep)
			c.stateMu.Lock()
			w.report = rep
			c.stateMu.Unlock()
			mu.Unlock()
		}()
	}
	wg.Wait()
	return mergeReports(c.runID, reports, dropouts)
}

// AbortAll fan-out aborts every worker. Used on coordinator shutdown
// to release the cluster.
func (c *Coordinator) AbortAll(ctx context.Context, reason string) {
	var wg sync.WaitGroup
	for _, w := range c.workers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			abCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := c.clients[w.addr].Abort(abCtx, c.runID, reason); err != nil {
				c.logger("coordinator: abort %s: %v", w.addr, err)
			}
		}()
	}
	wg.Wait()
}

// Close releases all underlying HTTP transports.
func (c *Coordinator) Close() {
	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()
	for _, cl := range c.clients {
		cl.Close()
	}
}

// allTerminal reports whether every worker is in a terminal state.
func (c *Coordinator) allTerminal() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for _, w := range c.workers {
		if !isTerminalState(w.state) {
			return false
		}
	}
	return true
}

// AggregateResult is the merged view returned to the CLI. Mirrors the
// shape of runner.RunResult but with the multi-worker accounting that
// only makes sense in the distributed case.
type AggregateResult struct {
	RunID            string             `json:"run_id"`
	StartedAt        time.Time          `json:"started_at"`
	StoppedAt        time.Time          `json:"stopped_at"`
	Workers          int                `json:"workers"`
	WorkersReported  int                `json:"workers_reported"`
	WorkersDropped   []string           `json:"workers_dropped,omitempty"`
	EventsGenerated  int64              `json:"events_generated"`
	EventsSubmitted  int64              `json:"events_submitted"`
	EventsSucceeded  int64              `json:"events_succeeded"`
	EventsFailed     int64              `json:"events_failed"`
	ClientErrors     int64              `json:"client_errors"`
	ServerErrors     int64              `json:"server_errors"`
	TransportErrors  int64              `json:"transport_errors"`
	LatencyP50Ms     float64            `json:"latency_p50_ms"`
	LatencyP90Ms     float64            `json:"latency_p90_ms"`
	LatencyP99Ms     float64            `json:"latency_p99_ms"`
	LatencyMaxMs     float64            `json:"latency_max_ms"`
	PerArchetype     map[string]int64   `json:"per_archetype,omitempty"`
	PerProductType   map[string]int64   `json:"per_product_type,omitempty"`
	PerIngestionPath map[string]int64   `json:"per_ingestion_path,omitempty"`
	PerWorker        map[string]*Report `json:"per_worker,omitempty"`
}

// mergeReports aggregates per-worker reports into one cluster view.
func mergeReports(runID string, reports []*Report, dropouts []string) AggregateResult {
	out := AggregateResult{
		RunID:           runID,
		Workers:         len(reports) + len(dropouts),
		WorkersReported: len(reports),
		WorkersDropped:  dropouts,
		PerArchetype:    map[string]int64{},
		PerProductType:  map[string]int64{},
		PerIngestionPath: map[string]int64{},
		PerWorker:       map[string]*Report{},
	}
	if len(reports) == 0 {
		return out
	}
	out.StartedAt = reports[0].StartedAt
	out.StoppedAt = reports[0].StoppedAt
	for _, r := range reports {
		if r.StartedAt.Before(out.StartedAt) {
			out.StartedAt = r.StartedAt
		}
		if r.StoppedAt.After(out.StoppedAt) {
			out.StoppedAt = r.StoppedAt
		}
		out.EventsGenerated += r.EventsGenerated
		out.EventsSubmitted += r.EventsSubmitted
		out.EventsSucceeded += r.EventsSucceeded
		out.EventsFailed += r.EventsFailed
		out.ClientErrors += r.ClientErrors
		out.ServerErrors += r.ServerErrors
		out.TransportErrors += r.TransportErrors
		// Latency: take the worst observed per-worker as a conservative
		// upper bound. Same convention as the in-process distributed
		// merge in internal/runner/distributed.go.
		if r.LatencyP50Ms > out.LatencyP50Ms {
			out.LatencyP50Ms = r.LatencyP50Ms
		}
		if r.LatencyP90Ms > out.LatencyP90Ms {
			out.LatencyP90Ms = r.LatencyP90Ms
		}
		if r.LatencyP99Ms > out.LatencyP99Ms {
			out.LatencyP99Ms = r.LatencyP99Ms
		}
		if r.LatencyMaxMs > out.LatencyMaxMs {
			out.LatencyMaxMs = r.LatencyMaxMs
		}
		for k, v := range r.PerArchetype {
			out.PerArchetype[k] += v
		}
		for k, v := range r.PerProductType {
			out.PerProductType[k] += v
		}
		for k, v := range r.PerIngestionPath {
			out.PerIngestionPath[k] += v
		}
		out.PerWorker[r.WorkerID] = r
	}
	return out
}

// partitionTenants splits the tenant list into n approximately-equal
// chunks via fnv32a(tenantID) % n. Same scenario+manifest always
// partitions the same way, so re-runs reproduce.
//
// When n exceeds the tenant count, returns one chunk per tenant + the
// remaining n-tenants empty chunks. This is rarely useful in practice
// — the coordinator's CLI rejects --workers > tenants up-front.
func partitionTenants(ids []string, n int) ([][]string, error) {
	if n <= 0 {
		return nil, errors.New("partition: n must be > 0")
	}
	if len(ids) == 0 {
		return nil, errors.New("partition: tenant list is empty")
	}
	chunks := make([][]string, n)
	for _, id := range ids {
		chunks[stableHash(id)%uint32(n)] = append(chunks[stableHash(id)%uint32(n)], id)
	}
	// Ensure deterministic ordering inside chunks.
	for i := range chunks {
		sort.Strings(chunks[i])
	}
	return chunks, nil
}

// splitTPS divides total into n shares, distributing the remainder so
// no partition gets zero. Same algorithm as
// internal/runner/distributed.go's splitTPS — replicated here to keep
// the coord package independent of the runner package.
func splitTPS(total, n int) []int {
	out := make([]int, n)
	if n <= 0 {
		return out
	}
	if total <= 0 {
		return out
	}
	base := total / n
	rem := total % n
	for i := 0; i < n; i++ {
		out[i] = base
		if i < rem {
			out[i]++
		}
		if out[i] == 0 {
			out[i] = 1
		}
	}
	return out
}

// stableHash is fnv32a — fast and stable across Go versions.
func stableHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// isTerminalState reports whether s represents a state from which no
// further heartbeats matter.
func isTerminalState(s string) bool {
	switch s {
	case "done", "aborted", "failed", "dropped":
		return true
	}
	return false
}

// randomRunID returns "<prefix>-<8-byte hex>".
func randomRunID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}
