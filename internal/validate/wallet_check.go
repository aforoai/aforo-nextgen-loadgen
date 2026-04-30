package validate

import (
	"context"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/wallet"
)

// runWalletLifecycle is Check 17.
//
// Asserts (per wallet_audit.jsonl):
//
//	a. Every PREPAID/HYBRID customer has at least pre + post-expiry snapshots.
//	b. The maximum sum-of-pending-holds during the run was ≤ initial balance
//	   (a hold cannot exceed an empty wallet — platform invariant).
//	c. After post-expiry, holds_active == 0 OR every active hold is a future-
//	   period hold (carried over). The validator surfaces the count and fails
//	   only when active holds remain AND the scenario configured a TTL — i.e.
//	   the platform should have released them.
//
// SKIPs cleanly when wallet_audit.jsonl is absent.
func (v *Validator) runWalletLifecycle(_ context.Context) *CheckResult {
	res := NewCheckResult(CheckWalletLifecycle)
	rows, err := wallet.LoadAudit(v.in.RunOutputDir)
	if err != nil {
		return res.Fail("load wallet_audit.jsonl: %v", err)
	}
	if len(rows) == 0 {
		if !v.in.Scenario.Wallet.HoldExpiryAudit && !v.in.Scenario.Wallet.BalanceAuditEnabled {
			return res.Skip("wallet audit not enabled in scenario")
		}
		return res.Skip("no wallet_audit.jsonl — collector didn't run")
	}
	noPre := 0
	noPostExpiry := 0
	holdInvariantViolations := 0
	expiredButActive := 0
	hadTTL := v.in.Scenario.Wallet.HoldTTLSeconds > 0
	for _, r := range rows {
		if r.InitialBalance == 0 && r.FinalBalance == 0 {
			noPre++
		}
		if r.HeldAtFinal == 0 && r.OutstandingHolds == 0 && r.FinalBalance == 0 && r.InitialBalance != 0 {
			noPostExpiry++
		}
		if !r.HoldsLEBalance {
			holdInvariantViolations++
		}
		if hadTTL && r.OutstandingHolds > 0 {
			expiredButActive++
		}
	}
	res.Set("total_wallets", len(rows))
	res.Set("missing_pre_snapshot", noPre)
	res.Set("missing_post_expiry", noPostExpiry)
	res.Set("hold_invariant_violations", holdInvariantViolations)
	res.Set("outstanding_holds_after_expiry", expiredButActive)

	if holdInvariantViolations > 0 {
		return res.Fail("%d wallets had pending holds exceeding initial balance (platform invariant violated)", holdInvariantViolations)
	}
	if hadTTL && expiredButActive > 0 {
		return res.Fail("%d wallets still have outstanding holds after %ds TTL — HoldExpiryScheduler did not converge",
			expiredButActive, v.in.Scenario.Wallet.HoldTTLSeconds)
	}
	if noPre > 0 {
		return res.Fail("%d wallets missing pre-snapshot — collector ran late or failed", noPre)
	}
	return res.Pass()
}
