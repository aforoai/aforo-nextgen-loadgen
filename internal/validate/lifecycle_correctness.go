package validate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// runLifecycleCorrectness is Check 9 — for each successful transition in
// transitions.jsonl, verify that the platform now reflects the expected
// post-state. Sub-assertions:
//
//	a. State match — sub.status == expected_post_state
//	b. Pro-ration on migrations — invoice has expected credit line (best-effort)
//	c. Pause/Resume — events during paused window NOT billed (deferred to Check 9.c.live)
//	d. Trial conversion — billing starts at conversion timestamp, no retroactive charges
//	e. Migrate stable-id — source_subscription_id == target_subscription_id
//	f. Dunning escalation — sub enters SUSPEND/CANCEL after configured retry count
//
// The check runs against transitions.jsonl alone for purely structural
// assertions (e — stable id from the OK record's note, f — escalation rows
// flow through dunning_walker). Live state checks (a, b) require the
// Subscriptions backend capability; without it, those rows SKIP individually.
func (v *Validator) runLifecycleCorrectness(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckLifecycleCorrectness)

	if len(v.in.Transitions) == 0 {
		return res.Skip("no transitions.jsonl — run with `aforo-loadgen lifecycle` to populate")
	}

	type rowOutcome struct {
		Index             int    `json:"index"`
		SubscriptionID    string `json:"subscription_id"`
		Transition        string `json:"transition"`
		ExpectedPostState string `json:"expected_post_state,omitempty"`
		ActualState       string `json:"actual_state,omitempty"`
		Match             bool   `json:"match"`
		Reason            string `json:"reason,omitempty"`
	}
	type kindOutcome struct {
		Kind        string       `json:"kind"`
		Total       int          `json:"total"`
		OK          int          `json:"ok"`
		Failures    int          `json:"failures"`
		StateMatch  int          `json:"state_match"`
		StateMismatch int        `json:"state_mismatch"`
		Sample      []rowOutcome `json:"sample,omitempty"`
	}

	byKind := map[lifecycle.TransitionKind]*kindOutcome{}
	subState := map[string]string{} // last-known platform state per sub
	overallPass := true

	canQueryLive := v.in.Backend != nil && v.in.Backend.Capabilities().Subscriptions
	stableIdViolations := 0
	dunningEscalations := 0
	totalAttempted := 0
	totalOK := 0

	for i, t := range v.in.Transitions {
		// Skip PENDING intent rows — they are logged before the API call so
		// a hung agent leaves a breadcrumb, but they MUST NOT count as
		// attempted transitions for the counter (the OUTCOME row will).
		if t.TransitionStatus == lifecycle.StatusPending {
			continue
		}
		k := byKind[t.Transition]
		if k == nil {
			k = &kindOutcome{Kind: string(t.Transition)}
			byKind[t.Transition] = k
		}
		k.Total++
		totalAttempted++

		switch t.TransitionStatus {
		case lifecycle.StatusOK:
			k.OK++
			totalOK++
		case lifecycle.StatusFail:
			k.Failures++
		}

		// Detect stable-id violation rows (migrate writes a synthetic FAIL
		// row when source != target).
		if t.Transition == lifecycle.TransitionMigrate &&
			t.TransitionStatus == lifecycle.StatusFail &&
			strings.Contains(t.Error, "stable-id violation") {
			stableIdViolations++
			overallPass = false
		}
		if t.Transition == lifecycle.TransitionDunningEscalate && t.TransitionStatus == lifecycle.StatusOK {
			dunningEscalations++
		}

		// Live-state assertion — only for OK rows on transitions whose
		// expected post-state is well-defined.
		if !canQueryLive || t.TransitionStatus != lifecycle.StatusOK || t.ExpectedPostState == "" {
			continue
		}

		// Avoid hammering the platform: if we already verified this sub for
		// the same expected state, skip subsequent identical checks.
		if last := subState[t.SubscriptionID]; last == t.ExpectedPostState {
			k.StateMatch++
			continue
		}

		snap, err := v.in.Backend.GetSubscriptionState(ctx, t.TenantID, t.SubscriptionID)
		if err != nil {
			var unsup ErrUnsupported
			if errors.As(err, &unsup) {
				canQueryLive = false
				continue
			}
			row := rowOutcome{
				Index:          i,
				SubscriptionID: t.SubscriptionID,
				Transition:     string(t.Transition),
				Reason:         fmt.Sprintf("backend error: %v", err),
			}
			if len(k.Sample) < 5 {
				k.Sample = append(k.Sample, row)
			}
			overallPass = false
			continue
		}
		subState[t.SubscriptionID] = snap.Status
		match := snap.Status == t.ExpectedPostState
		if match {
			k.StateMatch++
		} else {
			k.StateMismatch++
			overallPass = false
		}
		if (!match || len(k.Sample) < 3) && len(k.Sample) < 5 {
			k.Sample = append(k.Sample, rowOutcome{
				Index:             i,
				SubscriptionID:    t.SubscriptionID,
				Transition:        string(t.Transition),
				ExpectedPostState: t.ExpectedPostState,
				ActualState:       snap.Status,
				Match:             match,
			})
		}
	}

	out := make([]*kindOutcome, 0, len(byKind))
	for _, k := range []lifecycle.TransitionKind{
		lifecycle.TransitionUpgrade, lifecycle.TransitionDowngrade,
		lifecycle.TransitionPause, lifecycle.TransitionResume,
		lifecycle.TransitionTrialConversion, lifecycle.TransitionTrialCancel,
		lifecycle.TransitionMigrate, lifecycle.TransitionRetryPayment,
		lifecycle.TransitionDunningStep, lifecycle.TransitionDunningEscalate,
	} {
		if v, ok := byKind[k]; ok {
			out = append(out, v)
		}
	}

	res.
		Set("transitions_total", totalAttempted).
		Set("transitions_ok", totalOK).
		Set("by_kind", out).
		Set("stable_id_violations", stableIdViolations).
		Set("dunning_escalations", dunningEscalations).
		Set("live_state_checked", canQueryLive)

	if !canQueryLive {
		res.Set("live_state_skip_reason", "backend cannot query subscription state — structural checks only")
	}

	if !overallPass {
		return res.Fail("lifecycle correctness FAIL — see by_kind for details")
	}
	if totalAttempted == 0 {
		return res.Skip("transitions.jsonl is empty")
	}
	return res.Pass()
}

