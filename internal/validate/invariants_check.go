package validate

import (
	"context"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/invariants"
)

// runInvariants is Check 7 — runs the deterministic property-based fuzzer
// and surfaces every violation with the offending sample. The fuzz seed
// is scenario.seed so the same inputs reproduce the same trials.
//
// In addition to the seven property invariants, the check folds in the
// stale-key post-revoke probe result (Check 7.g): if the validator
// observed any successful ingestion on a revoked api_key, the invariants
// pass MUST FAIL. This is the same signal as Check 6.e.3 — surfaced in
// both places so per-check filters (--checks property_based_invariants)
// still catch the regression.
func (v *Validator) runInvariants(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckInvariants)

	cfg := invariants.FuzzConfig{
		Seed:   v.in.Scenario.Seed,
		Trials: 200,
	}
	// Wire stale-key probe result into the property check. Best-effort: an
	// offline backend returns ErrUnsupported and we skip the wiring (the
	// negative_paths check already SKIPped its FP probe with a clear note).
	if v.in.Backend.Capabilities().EventQueries {
		// EventsByAPIKey is what the FP probe needs. Capabilities is a
		// hint — actual call may still return ErrUnsupported on offline.
		if fp, err := v.staleKeyFalsePositives(ctx); err == nil {
			cfg.StaleKeyPostRevokeIngestions = fp
		}
	}

	out := invariants.Run(cfg)

	res.
		Set("trials", out.Trials).
		Set("violation_count", len(out.Violations)).
		Set("violations", out.Violations)

	if len(out.Violations) > 0 {
		return res.Fail("%d invariant violation(s) — see violations[]", len(out.Violations))
	}
	return res.Pass()
}
