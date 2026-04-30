package lifecycle

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestIsTerminal(t *testing.T) {
	cases := map[scenario.SubscriptionState]bool{
		scenario.StateActive:    false,
		scenario.StateTrialing:  false,
		scenario.StatePaused:    false,
		scenario.StatePastDue:   false,
		scenario.StateSuspended: false,
		scenario.StateExpired:   true,
		scenario.StateCancelled: true,
	}
	for s, want := range cases {
		if IsTerminal(s) != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, !want, want)
		}
	}
}

func TestCanFireFrom(t *testing.T) {
	type tc struct {
		kind TransitionKind
		from scenario.SubscriptionState
		want bool
	}
	cases := []tc{
		{TransitionUpgrade, scenario.StateActive, true},
		{TransitionUpgrade, scenario.StateTrialing, true},
		{TransitionUpgrade, scenario.StateCancelled, false},
		{TransitionUpgrade, scenario.StateExpired, false},
		{TransitionDowngrade, scenario.StatePaused, false}, // platform: must resume first
		{TransitionPause, scenario.StatePaused, false},     // already paused
		{TransitionPause, scenario.StateActive, true},
		{TransitionResume, scenario.StatePaused, true},
		{TransitionResume, scenario.StateActive, false},
		{TransitionTrialConversion, scenario.StateTrialing, true},
		{TransitionTrialConversion, scenario.StateActive, false},
		{TransitionTrialCancel, scenario.StateActive, false},
		{TransitionTrialCancel, scenario.StateTrialing, true},
		{TransitionMigrate, scenario.StateActive, true},
		{TransitionMigrate, scenario.StateCancelled, false},
		{TransitionRetryPayment, scenario.StatePastDue, true},
		{TransitionRetryPayment, scenario.StateActive, false},
		{TransitionDunningStep, scenario.StatePastDue, true},
	}
	for _, c := range cases {
		got := CanFireFrom(c.kind, c.from)
		if got != c.want {
			t.Errorf("CanFireFrom(%s, %s) = %v, want %v", c.kind, c.from, got, c.want)
		}
	}
}

func TestIsLegalTransition(t *testing.T) {
	type tc struct {
		from scenario.SubscriptionState
		to   scenario.SubscriptionState
		want bool
	}
	cases := []tc{
		{scenario.StateActive, scenario.StateActive, true}, // idempotent
		{scenario.StateActive, scenario.StatePaused, true},
		{scenario.StateActive, scenario.StatePastDue, true},
		{scenario.StateCancelled, scenario.StateActive, false}, // terminal-violation
		{scenario.StateExpired, scenario.StateActive, false},   // terminal-violation
		{scenario.StateTrialing, scenario.StateActive, true},
		{scenario.StateTrialing, scenario.StateExpired, false}, // not in legal next-set
		{scenario.StatePaused, scenario.StateActive, true},
		{scenario.StatePaused, scenario.StatePastDue, false}, // platform: must resume first
		{scenario.StateSuspended, scenario.StateActive, true},
		{scenario.StatePastDue, scenario.StateSuspended, true},
	}
	for _, c := range cases {
		got := IsLegalTransition(c.from, c.to)
		if got != c.want {
			t.Errorf("IsLegalTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestExpectedPostState_PauseResume(t *testing.T) {
	if got := ExpectedPostState(TransitionPause, scenario.StateActive); got != scenario.StatePaused {
		t.Errorf("PAUSE → %s, want PAUSED", got)
	}
	if got := ExpectedPostState(TransitionResume, scenario.StatePaused); got != scenario.StateActive {
		t.Errorf("RESUME → %s, want ACTIVE", got)
	}
}

func TestExpectedPostState_TrialFlow(t *testing.T) {
	if got := ExpectedPostState(TransitionTrialConversion, scenario.StateTrialing); got != scenario.StateActive {
		t.Errorf("TRIAL_CONVERSION → %s, want ACTIVE", got)
	}
	if got := ExpectedPostState(TransitionTrialCancel, scenario.StateTrialing); got != scenario.StateCancelled {
		t.Errorf("TRIAL_CANCEL → %s, want CANCELLED", got)
	}
}

func TestExpectedPostState_PreservesActivePlanChange(t *testing.T) {
	if got := ExpectedPostState(TransitionUpgrade, scenario.StateActive); got != scenario.StateActive {
		t.Errorf("UPGRADE in-place should preserve ACTIVE, got %s", got)
	}
	if got := ExpectedPostState(TransitionMigrate, scenario.StateTrialing); got != scenario.StateTrialing {
		t.Errorf("MIGRATE in-place should preserve TRIALING, got %s", got)
	}
}
