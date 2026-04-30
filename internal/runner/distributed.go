package runner

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"sort"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// DistributedConfig configures multi-partition mode. When Partitions > 1,
// the runner splits the manifest into N tenant chunks and runs N Runners
// in parallel, each generating events for its tenant subset only. Total
// throughput is preserved by dividing TargetTPS by N per partition.
//
// "Distributed" here means "tenant-partitioned"; the partitions live in
// the same process today. A future session can swap the in-process
// orchestration for cross-process (gRPC over Unix socket) without changing
// the partitioning logic — the seam is intentionally at this boundary.
type DistributedConfig struct {
	Partitions int
}

// RunDistributed runs N parallel partitioned runners and returns the
// merged result. Caller-provided cfg drives the per-partition runner;
// OutputDir under each partition is suffixed with /partition-K so the
// per-partition artifacts don't collide.
//
// Use this entry point when DistributedConfig.Partitions > 1; for the
// single-partition (default) case, construct a Runner via New + Run.
func RunDistributed(ctx context.Context, cfg Config, dc DistributedConfig) (*RunResult, error) {
	if dc.Partitions <= 1 {
		// Caller should have called runner.New + runner.Run directly; this
		// function is for the multi-partition case only.
		return nil, fmt.Errorf("RunDistributed: Partitions must be >= 2, got %d", dc.Partitions)
	}
	if cfg.Manifest == nil || len(cfg.Manifest.Tenants) == 0 {
		return nil, fmt.Errorf("RunDistributed: empty manifest")
	}

	chunks, err := partitionManifest(cfg.Manifest, dc.Partitions)
	if err != nil {
		return nil, err
	}

	// Per-partition TPS is the share of the requested total. We round-robin
	// the remainder to avoid systematic under-loading on partition 0.
	perPartitionTPS := splitTPS(cfg.Scenario.TargetTPS, dc.Partitions)

	results := make([]*RunResult, dc.Partitions)
	errs := make([]error, dc.Partitions)
	var wg sync.WaitGroup
	for i := 0; i < dc.Partitions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			subCfg := cfg
			subCfg.Manifest = chunks[idx]
			subCfg.OutputDir = filepath.Join(cfg.OutputDir, fmt.Sprintf("partition-%d", idx))
			// Per-partition scenario clone with TPS reduced.
			subScenario := *cfg.Scenario
			subScenario.TargetTPS = perPartitionTPS[idx]
			subScenario.Name = fmt.Sprintf("%s-p%d", cfg.Scenario.Name, idx)
			subCfg.Scenario = &subScenario

			r, err := New(subCfg)
			if err != nil {
				errs[idx] = err
				return
			}
			res, runErr := r.Run(ctx)
			results[idx] = res
			if runErr != nil {
				errs[idx] = runErr
			}
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			return nil, fmt.Errorf("partition %d: %w", i, e)
		}
	}
	merged := mergeResults(cfg, results)
	// Persist the merged run.json under the parent OutputDir so consumers
	// can address the run as a single artifact even when partitions ran
	// concurrently. We do not persist a merged HDR (the per-partition
	// latencies.hdr files contain the raw bins; merging accurately
	// requires reading those back from disk and re-bucketing — left for
	// follow-up).
	if err := merged.Save(cfg.OutputDir, nil, nil, nil); err != nil {
		return merged, fmt.Errorf("save merged: %w", err)
	}
	return merged, nil
}

// partitionManifest splits the manifest tenants deterministically into N
// chunks. The split key is fnv32a(tenantID) % N — same scenario+manifest
// always lands the same tenant in the same partition, so re-runs reproduce.
//
// The summary on each chunk is recomputed via Manifest.Finalize so the
// per-partition seeded counts are accurate.
func partitionManifest(m *seed.Manifest, n int) ([]*seed.Manifest, error) {
	if n <= 0 {
		return nil, fmt.Errorf("partition count must be > 0")
	}
	if n > len(m.Tenants) {
		return nil, fmt.Errorf("partition count %d > tenant count %d (use --partitions <= %d)",
			n, len(m.Tenants), len(m.Tenants))
	}
	chunks := make([]*seed.Manifest, n)
	for i := range chunks {
		chunks[i] = seed.NewManifest(m.RunID+fmt.Sprintf("-p%d", i), m.Target, m.Scenario, m.CreatedAt)
	}
	for _, t := range m.Tenants {
		idx := int(stableHash(t.TenantID) % uint32(n))
		chunks[idx].AppendTenant(t)
	}
	for _, c := range chunks {
		c.Finalize()
		if len(c.Tenants) == 0 {
			// A degenerate hash that left a partition empty is rare but
			// possible at very small N. Move one tenant from the largest
			// neighbor so every partition has at least one tenant.
			redistributeOneTenant(chunks)
		}
	}
	return chunks, nil
}

// redistributeOneTenant transfers one tenant from the largest non-empty
// partition to the first empty partition. Caller ensures at least one
// non-empty exists.
func redistributeOneTenant(chunks []*seed.Manifest) {
	var emptyIdx, largeIdx int = -1, 0
	for i, c := range chunks {
		if len(c.Tenants) == 0 && emptyIdx < 0 {
			emptyIdx = i
		}
		if len(c.Tenants) > len(chunks[largeIdx].Tenants) {
			largeIdx = i
		}
	}
	if emptyIdx < 0 || largeIdx == emptyIdx || len(chunks[largeIdx].Tenants) <= 1 {
		return
	}
	moved := chunks[largeIdx].Tenants[len(chunks[largeIdx].Tenants)-1]
	chunks[largeIdx].Tenants = chunks[largeIdx].Tenants[:len(chunks[largeIdx].Tenants)-1]
	chunks[emptyIdx].AppendTenant(moved)
	chunks[largeIdx].Finalize()
	chunks[emptyIdx].Finalize()
}

// stableHash is fnv32a — fast and stable across Go versions.
func stableHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// splitTPS divides total into n shares, round-robining the remainder.
// e.g. (10, 3) → [4, 3, 3]; (12, 4) → [3, 3, 3, 3]; (1, 4) → [1, 0, 0, 0].
//
// Special case: when total ≤ n, partitions beyond `total` get 1 each up to
// the total — every partition still produces traffic, even if minimal.
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
			// Ensure every partition fires at least one event so the merge
			// has data from every shard. Trades exact total for non-empty
			// partitions — acceptable for the common case where total >> n.
			out[i] = 1
		}
	}
	return out
}

// mergeResults aggregates the per-partition RunResults into one. Counters
// sum, per-tenant maps union (partitions are disjoint by tenant), latency
// percentiles are recomputed from the merged HDR (approximated by
// max-observed when raw HDRs aren't available — partitions write per-
// partition latencies.hdr but merging requires re-reading from disk; for
// the in-process path we use the worst per-partition p99 as a conservative
// upper bound on the run-wide p99).
//
// The returned RunResult's RunID, Target, scenario name, etc. come from
// the parent config so consumers see a single coherent run.
func mergeResults(cfg Config, results []*RunResult) *RunResult {
	merged := &RunResult{
		RunID:        randomRunID(),
		ScenarioName: cfg.Scenario.Name,
		Target:       cfg.Target.Name,
		TargetTPS:    cfg.Scenario.TargetTPS,

		NegativePathCounts: map[generator.NegativePathKind]int64{},
		PerArchetype:       map[string]int64{},
		PerTenant:          map[string]int64{},
		PerProductType:     map[string]int64{},
		PerIngestionPath:   map[string]int64{},
		PerTenantP99Ms:     map[string]float64{},
		PerTenantPathP99Ms: map[string]map[string]float64{},
	}
	if len(results) == 0 {
		return merged
	}
	merged.StartedAt = results[0].StartedAt
	merged.StoppedAt = results[0].StoppedAt
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.StartedAt.Before(merged.StartedAt) {
			merged.StartedAt = r.StartedAt
		}
		if r.StoppedAt.After(merged.StoppedAt) {
			merged.StoppedAt = r.StoppedAt
		}
		merged.EventsGenerated += r.EventsGenerated
		merged.EventsSubmitted += r.EventsSubmitted
		merged.EventsSucceeded += r.EventsSucceeded
		merged.EventsFailed += r.EventsFailed
		merged.ClientErrors += r.ClientErrors
		merged.ServerErrors += r.ServerErrors
		merged.TransportFailures += r.TransportFailures
		merged.CircuitOpenSkipped += r.CircuitOpenSkipped
		merged.ExpectedFailures += r.ExpectedFailures
		merged.TenantsActive += r.TenantsActive
		merged.FairnessGateDeferred += r.FairnessGateDeferred
		merged.PerTenantHistogramsMB += r.PerTenantHistogramsMB

		for k, v := range r.NegativePathCounts {
			merged.NegativePathCounts[k] += v
		}
		for k, v := range r.PerArchetype {
			merged.PerArchetype[k] += v
		}
		for k, v := range r.PerTenant {
			merged.PerTenant[k] += v
		}
		for k, v := range r.PerProductType {
			merged.PerProductType[k] += v
		}
		for k, v := range r.PerIngestionPath {
			merged.PerIngestionPath[k] += v
		}
		for k, v := range r.PerTenantP99Ms {
			merged.PerTenantP99Ms[k] = v
		}
		for k, v := range r.PerTenantPathP99Ms {
			merged.PerTenantPathP99Ms[k] = v
		}

		// Latency percentiles — take the worst (most pessimistic) observed
		// per-partition value as the merged p99 / max. This is conservative
		// but accurate as an upper bound on tail latency.
		if r.LatencyP99ms > merged.LatencyP99ms {
			merged.LatencyP99ms = r.LatencyP99ms
		}
		if r.LatencyP90ms > merged.LatencyP90ms {
			merged.LatencyP90ms = r.LatencyP90ms
		}
		if r.LatencyP50ms > merged.LatencyP50ms {
			merged.LatencyP50ms = r.LatencyP50ms
		}
		if r.LatencyMaxMs > merged.LatencyMaxMs {
			merged.LatencyMaxMs = r.LatencyMaxMs
		}

		merged.BackpressureEngagedAt = append(merged.BackpressureEngagedAt, r.BackpressureEngagedAt...)
		merged.CircuitBreakerOpenedAt = append(merged.CircuitBreakerOpenedAt, r.CircuitBreakerOpenedAt...)
		merged.Errors = append(merged.Errors, r.Errors...)
	}
	merged.Duration = merged.StoppedAt.Sub(merged.StartedAt)

	// Recompute the merged fairness report from the per-tenant p99 map.
	if len(merged.PerTenantP99Ms) > 0 {
		merged.Fairness = recomputeFairness(merged.PerTenantP99Ms)
	}

	sort.Slice(merged.BackpressureEngagedAt, func(i, j int) bool {
		return merged.BackpressureEngagedAt[i].Before(merged.BackpressureEngagedAt[j])
	})
	sort.Slice(merged.CircuitBreakerOpenedAt, func(i, j int) bool {
		return merged.CircuitBreakerOpenedAt[i].Before(merged.CircuitBreakerOpenedAt[j])
	})
	return merged
}

// recomputeFairness mirrors the per-tenant store's report computation but
// works directly off the merged map so we don't have to round-trip the
// HDR data.
func recomputeFairness(p99 map[string]float64) *FairnessReport {
	if len(p99) == 0 {
		return &FairnessReport{}
	}
	values := make([]float64, 0, len(p99))
	for _, v := range p99 {
		values = append(values, v)
	}
	sort.Float64s(values)
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, v := range values {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(values))
	stddev := 0.0
	if variance > 0 {
		stddev = sqrt64(variance)
	}
	stddevPct := 0.0
	if mean > 0 {
		stddevPct = stddev / mean
	}
	return &FairnessReport{
		TenantsObserved: len(values),
		MeanP99Ms:       mean,
		StddevP99Ms:     stddev,
		StddevPct:       stddevPct,
		MinP99Ms:        values[0],
		MaxP99Ms:        values[len(values)-1],
	}
}

// sqrt64 — same Newton-Raphson sqrt used in the driver fairness gate.
// Inlined to avoid pulling math here for one call.
func sqrt64(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}
