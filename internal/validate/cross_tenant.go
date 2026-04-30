package validate

import (
	"context"
	"errors"
	"sort"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// runCrossTenantLeakage is Check 2 — for up to 10 tenants in the manifest,
// ask the backend "give me events tagged with this tenant id, but using a
// different tenant's auth header." A correctly-isolated platform returns
// zero rows for every probe.
//
// The probe set is intentionally small (10) because each probe is a real
// authenticated query against the backend; expanding linearly scales test
// time. 10 is enough — any regression shows up everywhere, not just at
// tenant 11+.
func (v *Validator) runCrossTenantLeakage(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckCrossTenant)

	if !v.in.Backend.Capabilities().CrossTenantProbe {
		return res.Skip("backend cannot run cross-tenant probes (offline mode or capability missing)")
	}

	tenants := v.in.Manifest.Tenants
	if len(tenants) < 2 {
		return res.Skip("manifest has %d tenant(s); need ≥2 to construct a probe", len(tenants))
	}

	probes := buildCrossTenantProbes(tenants, 10)

	results, err := v.in.Backend.CrossTenantQuery(ctx, v.runWindow(), probes)
	if err != nil {
		var unsup ErrUnsupported
		if errors.As(err, &unsup) {
			return res.Skip("backend unsupported: %s", unsup.Op)
		}
		return res.Fail("backend CrossTenantQuery: %v", err)
	}

	type leak struct {
		ProbeID       string `json:"probe_id"`
		WrongTenantID string `json:"wrong_tenant_id"`
		RealTenantID  string `json:"real_tenant_id"`
		LeakedRows    int64  `json:"leaked_rows"`
	}
	leaks := make([]leak, 0, len(probes))
	totalLeaked := int64(0)
	for _, p := range probes {
		n := results[p.ProbeID]
		if n > 0 {
			totalLeaked += n
		}
		leaks = append(leaks, leak{
			ProbeID:       p.ProbeID,
			WrongTenantID: p.WrongTenantID,
			RealTenantID:  p.RealTenantID,
			LeakedRows:    n,
		})
	}
	sort.SliceStable(leaks, func(i, j int) bool {
		return leaks[i].ProbeID < leaks[j].ProbeID
	})

	tolerance := int64(v.in.Scenario.Assertions.CrossTenantLeakageMax)
	res.
		Set("probes_run", len(probes)).
		Set("total_leaked_rows", totalLeaked).
		Set("tolerance", tolerance).
		Set("by_probe", leaks)

	if totalLeaked > tolerance {
		return res.Fail("cross-tenant leakage detected: %d row(s) (tolerance %d)",
			totalLeaked, tolerance)
	}
	return res.Pass()
}

// buildCrossTenantProbes pairs the first up-to-N tenants with their next
// neighbor — deterministic for reproducibility. A tenant is never paired
// with itself.
func buildCrossTenantProbes(tenants []seed.ManifestTenant, max int) []CrossTenantProbe {
	if len(tenants) < 2 {
		return nil
	}
	if max <= 0 || max > len(tenants) {
		max = len(tenants)
	}
	out := make([]CrossTenantProbe, 0, max)
	for i := 0; i < max; i++ {
		real := tenants[i]
		wrong := tenants[(i+1)%len(tenants)]
		out = append(out, CrossTenantProbe{
			ProbeID:       real.TenantID + "/" + wrong.TenantID,
			WrongTenantID: wrong.TenantID,
			RealTenantID:  real.TenantID,
		})
	}
	return out
}
