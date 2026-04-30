package validate

import (
	"context"
	"errors"
	"fmt"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// runSingleERPInvariant is Check 18.
//
// Asserts (when scenario.erp.multi_erp_enabled = false): connecting a
// SECOND ERP to a tenant returns 409 Conflict — i.e. the platform's
// single-ERP-invariant guard fires.
//
// The probe runs a controlled connect/disconnect:
//
//	1. POST /api/v1/erp-integrations/connect — provider A   → expect 2xx
//	2. POST /api/v1/erp-integrations/connect — provider B   → expect 409
//	3. POST /api/v1/erp-integrations/disconnect — provider A → cleanup
//
// SKIPs when:
//   - scenario.erp is disabled
//   - scenario.erp.multi_erp_enabled is true (the invariant is OFF for
//     this run by design)
//   - <2 providers configured (no second to attempt)
//   - the validator can't construct a backend client (offline mode)
//
// The probe targets the platform's billing service. We pick the FIRST
// tenant from the manifest as the probe target. The disconnect cleanup
// runs even on failure so the population is left clean.
func (v *Validator) runSingleERPInvariant(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckSingleERPInvariant)
	if !v.in.Scenario.ERP.Enabled {
		return res.Skip("erp not enabled in scenario")
	}
	if v.in.Scenario.ERP.MultiERPEnabled {
		return res.Skip("scenario.erp.multi_erp_enabled is true — single-ERP invariant is OFF for this run")
	}
	if len(v.in.Scenario.ERP.ProvidersPerTenantMix) < 2 {
		return res.Skip("need at least 2 providers in scenario.erp.providers_per_tenant_mix to probe second-connect")
	}
	if len(v.in.Manifest.Tenants) == 0 {
		return res.Skip("no tenants in manifest — nothing to probe")
	}

	// Backend offline → no live HTTP. The check needs the platform.
	if !v.in.Backend.Capabilities().BillRuns {
		return res.Skip("backend offline — single-ERP probe needs live billing service")
	}

	tenantID := v.in.Manifest.Tenants[0].TenantID
	providers := orderedProviders(v.in.Scenario.ERP.ProvidersPerTenantMix)
	first, second := providers[0], providers[1]

	target := v.in.Run.Target
	t, err := aforo.ResolveTarget(target)
	if err != nil {
		return res.Fail("resolve target: %v", err)
	}
	client, err := lifecycle.NewClient(lifecycle.ClientConfig{Target: t})
	if err != nil {
		return res.Fail("client: %v", err)
	}

	// Try to connect provider A. Treat 409 here as "already connected" — we
	// disconnect at the end, so prior runs leaving residue don't fail us.
	body1 := map[string]any{
		"provider":     first,
		"display_name": "loadgen-probe-a",
	}
	statusA, errA := client.PostJSON(ctx, aforo.ServiceBilling,
		"/api/v1/erp-integrations/connect", tenantID,
		fmt.Sprintf("loadgen-probe-erp-a-%s", tenantID),
		body1, nil,
	)
	res.Set("first_connect_status", statusA)
	if errA != nil && statusA != 409 {
		return res.Fail("first connect (%s) failed: %v", first, errA)
	}

	// Now attempt to connect provider B — expect 409.
	body2 := map[string]any{
		"provider":     second,
		"display_name": "loadgen-probe-b",
	}
	statusB, errB := client.PostJSON(ctx, aforo.ServiceBilling,
		"/api/v1/erp-integrations/connect", tenantID,
		fmt.Sprintf("loadgen-probe-erp-b-%s", tenantID),
		body2, nil,
	)
	res.Set("second_connect_status", statusB)
	res.Set("first_provider", first)
	res.Set("second_provider", second)

	defer func() {
		// Cleanup attempt — best-effort; ignore errors.
		_, _ = client.PostJSON(ctx, aforo.ServiceBilling,
			fmt.Sprintf("/api/v1/erp-integrations/disconnect?provider=%s", first),
			tenantID, "loadgen-probe-erp-cleanup-"+tenantID,
			struct{}{}, nil,
		)
	}()

	if statusB == 409 {
		return res.Pass()
	}
	if errB != nil && lifecycle.HTTPStatus(errB) == 409 {
		return res.Pass()
	}
	if errors.Is(errB, context.Canceled) {
		return res.Skip("ctx canceled mid-probe")
	}
	return res.Fail("second connect (%s) returned status=%d (want 409 for single-ERP invariant)", second, statusB)
}

func orderedProviders(mix map[string]float64) []string {
	out := make([]string, 0, len(mix))
	for k := range mix {
		out = append(out, k)
	}
	// Stable lexicographic order — picks "quickbooks" before "xero" deterministically.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
