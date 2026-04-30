package lifecycle

import "github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"

// TransitionKind names every lifecycle transition the agent can fire.
// Stable strings — they are written to transitions.jsonl and parsed by
// the validator (Checks 9/10).
type TransitionKind string

const (
	TransitionUpgrade         TransitionKind = "UPGRADE"
	TransitionDowngrade       TransitionKind = "DOWNGRADE"
	TransitionPause           TransitionKind = "PAUSE"
	TransitionResume          TransitionKind = "RESUME"
	TransitionTrialConversion TransitionKind = "TRIAL_CONVERSION"
	TransitionTrialCancel     TransitionKind = "TRIAL_CANCEL"
	TransitionMigrate         TransitionKind = "MIGRATE"
	TransitionRetryPayment    TransitionKind = "RETRY_PAYMENT"
	TransitionDunningStep     TransitionKind = "DUNNING_STEP"
	TransitionDunningEscalate TransitionKind = "DUNNING_ESCALATE"
)

// AllTransitionKinds is the canonical iteration order. Used by the agent
// to enumerate per-kind tickers and by tests.
var AllTransitionKinds = []TransitionKind{
	TransitionUpgrade, TransitionDowngrade,
	TransitionPause, TransitionResume,
	TransitionTrialConversion, TransitionTrialCancel,
	TransitionMigrate, TransitionRetryPayment,
	TransitionDunningStep, TransitionDunningEscalate,
}

// IsTerminal reports whether a state is one we never transition out of.
// CANCELLED + EXPIRED are terminal — a CANCELLED→ACTIVE transition is a
// state-machine violation (Check 10).
func IsTerminal(s scenario.SubscriptionState) bool {
	switch s {
	case scenario.StateCancelled, scenario.StateExpired:
		return true
	default:
		return false
	}
}

// CanFireFrom reports whether kind is a legal transition to attempt from
// the given state. The agent uses this to filter candidate subs BEFORE
// firing the API call — a CANCELLED sub never has DOWNGRADE attempted on it.
//
// This mirrors the platform's SubscriptionStateMachine, but is deliberately
// permissive — it allows attempts the platform may reject with 409. We do
// NOT skip "the platform might say no" because some of those 409s are the
// signal we're looking for (e.g. dunning escalation reaching SUSPEND).
//
// Forbidden by definition (caught here):
//   - any transition out of a terminal state (CANCELLED / EXPIRED)
//   - PAUSE on an already-PAUSED sub
//   - RESUME on a non-PAUSED sub
//   - TRIAL_CONVERSION/CANCEL on a non-TRIALING sub
func CanFireFrom(kind TransitionKind, from scenario.SubscriptionState) bool {
	if IsTerminal(from) {
		return false
	}
	switch kind {
	case TransitionUpgrade, TransitionDowngrade:
		return from == scenario.StateActive ||
			from == scenario.StateTrialing ||
			from == scenario.StatePastDue ||
			from == scenario.StateExpiringSoon
	case TransitionPause:
		return from == scenario.StateActive || from == scenario.StateTrialing
	case TransitionResume:
		return from == scenario.StatePaused
	case TransitionTrialConversion, TransitionTrialCancel:
		return from == scenario.StateTrialing
	case TransitionMigrate:
		return from == scenario.StateActive ||
			from == scenario.StateTrialing ||
			from == scenario.StatePaused ||
			from == scenario.StateExpiringSoon ||
			from == scenario.StatePastDue
	case TransitionRetryPayment:
		return from == scenario.StatePastDue || from == scenario.StateSuspended
	case TransitionDunningStep, TransitionDunningEscalate:
		return from == scenario.StatePastDue || from == scenario.StateSuspended
	}
	return false
}

// ExpectedPostState returns the state the platform should land in after a
// successful transition. The validator (Check 9) compares the live sub
// state to this. Empty string means "depends on context — caller decides".
func ExpectedPostState(kind TransitionKind, prior scenario.SubscriptionState) scenario.SubscriptionState {
	switch kind {
	case TransitionUpgrade, TransitionDowngrade:
		// Plan changes are in-place — state is preserved (ACTIVE stays ACTIVE).
		return prior
	case TransitionPause:
		return scenario.StatePaused
	case TransitionResume:
		return scenario.StateActive
	case TransitionTrialConversion:
		return scenario.StateActive
	case TransitionTrialCancel:
		return scenario.StateCancelled
	case TransitionMigrate:
		// Stable-id semantic: same subscription, possibly new offering.
		// Status is preserved; the audit row records the offering change.
		return prior
	case TransitionRetryPayment:
		// Success → ACTIVE; failure → state preserved.
		return scenario.StateActive
	case TransitionDunningEscalate:
		// After max retries, the platform escalates to SUSPEND (and
		// eventually CANCEL — the dunning_walker decides which).
		return scenario.StateSuspended
	}
	return ""
}

// IsLegalTransition is the platform-mirror state-machine check used by
// Check 10. Returns true if from→to is allowed by the V3 SubscriptionStateMachine.
//
// Forbidden patterns surfaced as state-machine violations:
//   - any state → ACTIVE from CANCELLED (terminal violation)
//   - GA → BETA on the maturity stage (regression — separate enforcement)
//   - EXPIRED → anything (terminal)
func IsLegalTransition(from, to scenario.SubscriptionState) bool {
	if from == to {
		return true // idempotent same-state is always legal
	}
	if IsTerminal(from) {
		return false
	}
	allowed := legalNextStates[from]
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// legalNextStates mirrors com.aforo.billing.subscription.state.SubscriptionStateMachine.
// Add new transitions here in lockstep with the platform.
var legalNextStates = map[scenario.SubscriptionState][]scenario.SubscriptionState{
	scenario.StateCreated: {
		scenario.StateTrialing, scenario.StateActive, scenario.StateCancelled,
	},
	scenario.StateTrialing: {
		scenario.StateActive, scenario.StateCancelled, scenario.StatePaused,
	},
	scenario.StateActive: {
		scenario.StatePastDue, scenario.StatePaused, scenario.StateExpiringSoon,
		scenario.StateExpired, scenario.StateCancelled, scenario.StateSuspended,
	},
	scenario.StatePastDue: {
		scenario.StateActive, scenario.StateSuspended, scenario.StateCancelled,
		scenario.StateExpired,
	},
	scenario.StatePaused: {
		scenario.StateActive, scenario.StateCancelled, scenario.StateExpired,
	},
	scenario.StateExpiringSoon: {
		scenario.StateActive, scenario.StateExpired, scenario.StateCancelled,
	},
	scenario.StateSuspended: {
		scenario.StateActive, scenario.StateCancelled, scenario.StateExpired,
	},
}
