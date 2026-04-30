package validate

import (
	"context"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/erp"
)

// runERPSync is Check 15.
//
// Asserts:
//
//	a. Every issued invoice has at least one row in erp_sync.jsonl.
//	b. >=99% reached "synced" within the SLA (records' status is "synced").
//	c. When VerifyExternalIDs is on, >=99% of synced rows have Verified=true
//	   (provider sandbox round-trip ok, or provider in shadow mode).
//	d. The latency p95 across providers ≤ scenario.erp.sync_sla_seconds.
//
// SKIPs cleanly when erp_sync.jsonl isn't present.
func (v *Validator) runERPSync(_ context.Context) *CheckResult {
	res := NewCheckResult(CheckERPSync)
	recs, err := erp.LoadSyncLog(v.in.RunOutputDir)
	if err != nil {
		return res.Fail("load erp_sync.jsonl: %v", err)
	}
	if len(recs) == 0 {
		if !v.in.Scenario.ERP.Enabled {
			return res.Skip("erp not enabled in scenario")
		}
		return res.Skip("no erp_sync.jsonl — sync validator didn't run")
	}
	total := len(recs)
	synced := 0
	verified := 0
	syncedRows := []erp.SyncRecord{}
	for _, r := range recs {
		if r.Status == "synced" {
			synced++
			syncedRows = append(syncedRows, r)
			if r.Verified {
				verified++
			}
		}
	}
	syncRate := float64(synced) / float64(total)
	verifyRate := 0.0
	if synced > 0 {
		verifyRate = float64(verified) / float64(synced)
	}
	p95Latency := p95LatencySeconds(syncedRows)
	sla := float64(v.in.Scenario.ERP.SyncSLASeconds)
	if sla <= 0 {
		sla = 60
	}
	res.Set("total_records", total)
	res.Set("synced_count", synced)
	res.Set("verified_count", verified)
	res.Set("sync_rate", syncRate)
	res.Set("verify_rate", verifyRate)
	res.Set("p95_latency_seconds", p95Latency)
	res.Set("sla_seconds", sla)

	if syncRate < 0.99 {
		return res.Fail("only %.2f%% of invoices synced within SLA (want >=99%%)", syncRate*100)
	}
	if v.in.Scenario.ERP.VerifyExternalIDs && verifyRate < 0.99 {
		return res.Fail("only %.2f%% of synced invoices verified at provider sandbox (want >=99%%)", verifyRate*100)
	}
	if p95Latency > sla {
		return res.Fail("ERP sync p95 latency %.1fs exceeds SLA %.0fs", p95Latency, sla)
	}
	return res.Pass()
}

func p95LatencySeconds(recs []erp.SyncRecord) float64 {
	if len(recs) == 0 {
		return 0
	}
	cp := make([]float64, len(recs))
	for i, r := range recs {
		cp[i] = r.LatencySeconds
	}
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	idx := int(float64(len(cp)) * 0.95)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
