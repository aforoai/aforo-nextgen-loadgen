package lifecycle

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestDunningWalker_StepsBelowThreshold(t *testing.T) {
	w := NewDunningWalker(DunningConfig{MaxRetries: 3, EscalateAfterRetries: 3})
	if w.Counter("s") != 0 {
		t.Fatal("counter should start at 0")
	}
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		// retry-payment succeeds — counter still increments (we test the escalation gate, not platform behavior).
		return 200, `{"id":"s","status":"PAST_DUE","dunningAttempt":1}`
	}}
	deps, _, buf := newTestDeps(t, h)

	s := Subject{TenantID: "t", SubscriptionID: "s", State: scenario.StatePastDue}
	if err := w.Step(context.Background(), deps, s); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if w.Counter("s") != 1 {
		t.Fatalf("counter after step 1 = %d, want 1", w.Counter("s"))
	}

	recs := decodeRecords(t, buf)
	// We expect at least one DUNNING_STEP row before retry-payment fires.
	stepRows := 0
	for _, r := range recs {
		if r.Transition == TransitionDunningStep {
			stepRows++
		}
	}
	if stepRows == 0 {
		t.Errorf("expected DUNNING_STEP rows, got: %s", buf.String())
	}
}

func TestDunningWalker_EscalatesAfterMaxRetries(t *testing.T) {
	w := NewDunningWalker(DunningConfig{MaxRetries: 2, EscalateAfterRetries: 2})
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		return 200, `{"id":"s","status":"PAST_DUE","dunningAttempt":99}`
	}}
	deps, _, buf := newTestDeps(t, h)
	s := Subject{TenantID: "t", SubscriptionID: "s", State: scenario.StatePastDue}

	for i := 0; i < 3; i++ {
		if err := w.Step(context.Background(), deps, s); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	// At counter=3 (>MaxRetries=2), the walker should write a
	// DUNNING_ESCALATE row and mark sub SUSPENDED.
	recs := decodeRecords(t, buf)
	escalateFound := false
	for _, r := range recs {
		if r.Transition == TransitionDunningEscalate &&
			r.TransitionStatus == StatusOK &&
			r.ExpectedPostState == string(scenario.StateSuspended) {
			escalateFound = true
		}
	}
	if !escalateFound {
		t.Fatalf("expected DUNNING_ESCALATE row, got:\n%s", buf.String())
	}
	if deps.Picker.LiveState("s") != scenario.StateSuspended {
		t.Errorf("after escalation, sub state should be SUSPENDED")
	}
}

func TestDunningWalker_ResetClearsCounter(t *testing.T) {
	w := NewDunningWalker(DunningConfig{MaxRetries: 3})
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		return 200, `{"id":"s","status":"PAST_DUE"}`
	}}
	deps, _, _ := newTestDeps(t, h)
	s := Subject{TenantID: "t", SubscriptionID: "s", State: scenario.StatePastDue}

	_ = w.Step(context.Background(), deps, s)
	_ = w.Step(context.Background(), deps, s)
	w.Reset("s")
	if w.Counter("s") != 0 {
		t.Fatalf("counter after Reset = %d, want 0", w.Counter("s"))
	}
}

func TestDunningWalker_DefaultConfigUsedOnZero(t *testing.T) {
	// Zero MaxRetries → DefaultDunningConfig kicks in.
	w := NewDunningWalker(DunningConfig{})
	if w.cfg.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3 (default)", w.cfg.MaxRetries)
	}
}

func TestDunningWalker_PropagatesRetryFailure(t *testing.T) {
	w := NewDunningWalker(DunningConfig{MaxRetries: 3})
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		return 500, `{"error":"bad gateway"}`
	}}
	deps, _, buf := newTestDeps(t, h)
	s := Subject{TenantID: "t", SubscriptionID: "s", State: scenario.StatePastDue}
	if err := w.Step(context.Background(), deps, s); err == nil {
		t.Fatal("expected error on 500")
	}
	// Walker should have logged a fail row for the dunning step.
	if !strings.Contains(buf.String(), `"transition_status":"FAIL"`) {
		t.Errorf("expected FAIL row, got:\n%s", buf.String())
	}
}

// silence unused warning when building without -race flag
var _ = bytes.Buffer{}
