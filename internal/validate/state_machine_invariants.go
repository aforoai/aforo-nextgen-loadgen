package validate

import (
	"context"
	"fmt"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// runStateMachineInvariants is Check 10 — over the run, no transition
// violated the platform's V3 SubscriptionStateMachine. Specifically:
//
//	a. No CANCELLED → ACTIVE (terminal violation)
//	b. No EXPIRED   → anything (terminal violation)
//	c. No GA → BETA (maturity regression — caught when from_state is set
//	   on the audit row to a maturity value, e.g. dunning_walker rows)
//	d. Every successful transition has a corresponding subscription_phase
//	   audit row in the platform DB (best-effort: requires backend support).
//
// The check is structural — it walks transitions.jsonl and validates each
// row's from_state vs the transition kind's legality. It does NOT require
// backend access for the legality assertions, only for the audit-row check
// (d), which SKIPs when the backend has no subscription read capability.
//
// State-machine violations always produce overall FAIL. Audit-row absence
// is reported but not failure-grade unless every checked row is missing.
func (v *Validator) runStateMachineInvariants(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckStateMachineInvariants)

	if len(v.in.Transitions) == 0 {
		return res.Skip("no transitions.jsonl — run with `aforo-loadgen lifecycle` to populate")
	}

	type violation struct {
		Index          int    `json:"index"`
		SubscriptionID string `json:"subscription_id"`
		Transition     string `json:"transition"`
		FromState      string `json:"from_state"`
		ExpectedTo     string `json:"expected_to"`
		Reason         string `json:"reason"`
	}

	violations := []violation{}

	// (a) + (b) + (c) — walk the log, check kind-vs-from-state legality.
	for i, t := range v.in.Transitions {
		// Skip SKIPPED + PENDING rows — they are intent rows / intentional
		// no-ops, not violations. Validate only OUTCOME rows so the same
		// transition isn't double-flagged.
		if t.TransitionStatus == lifecycle.StatusSkipped ||
			t.TransitionStatus == lifecycle.StatusPending {
			continue
		}
		// FAILed rows record the platform rejecting an attempt — that's
		// expected behavior. We only enforce legality on what the agent
		// actually sent (the FromState was the agent's view at intent time).
		if t.FromState == "" {
			continue
		}
		from := scenarioState(t.FromState)
		if !lifecycle.CanFireFrom(t.Transition, from) {
			violations = append(violations, violation{
				Index:          i,
				SubscriptionID: t.SubscriptionID,
				Transition:     string(t.Transition),
				FromState:      t.FromState,
				ExpectedTo:     t.ExpectedPostState,
				Reason:         fmt.Sprintf("transition %s illegal from state %s", t.Transition, t.FromState),
			})
			continue
		}
		// Strict from→to legality check on the platform's state machine.
		if t.ExpectedPostState != "" && t.TransitionStatus == lifecycle.StatusOK {
			to := scenarioState(t.ExpectedPostState)
			if !lifecycle.IsLegalTransition(from, to) {
				violations = append(violations, violation{
					Index:          i,
					SubscriptionID: t.SubscriptionID,
					Transition:     string(t.Transition),
					FromState:      t.FromState,
					ExpectedTo:     t.ExpectedPostState,
					Reason:         fmt.Sprintf("V3 state machine forbids %s → %s", t.FromState, t.ExpectedPostState),
				})
			}
		}
	}

	// (d) Audit-row presence — SKIP when backend lacks Subscriptions
	// capability. Only sample the most recent OK row per sub to keep the
	// probe count bounded.
	auditRowsChecked, auditRowsMissing := 0, 0
	if v.in.Backend != nil && v.in.Backend.Capabilities().Subscriptions {
		seen := map[string]bool{}
		for _, t := range v.in.Transitions {
			if t.TransitionStatus != lifecycle.StatusOK || seen[t.SubscriptionID] {
				continue
			}
			seen[t.SubscriptionID] = true
			snap, err := v.in.Backend.GetSubscriptionState(ctx, t.TenantID, t.SubscriptionID)
			if err != nil {
				continue
			}
			auditRowsChecked++
			if !snap.LastPhaseRecorded {
				auditRowsMissing++
				violations = append(violations, violation{
					SubscriptionID: t.SubscriptionID,
					Transition:     string(t.Transition),
					Reason:         "subscription_phase audit row missing for last transition",
				})
			}
			if auditRowsChecked >= 50 {
				break // bound the probe
			}
		}
	}

	res.
		Set("violations_total", len(violations)).
		Set("violations", violations).
		Set("audit_rows_checked", auditRowsChecked).
		Set("audit_rows_missing", auditRowsMissing)

	if len(violations) > 0 {
		return res.Fail("%d state-machine violation(s)", len(violations))
	}
	return res.Pass()
}
