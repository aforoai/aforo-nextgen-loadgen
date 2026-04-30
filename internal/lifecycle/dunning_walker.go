package lifecycle

import (
	"context"
	"fmt"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// DunningConfig configures the walker. MaxRetries mirrors the platform's
// dunning.max-attempts (default 3 per Aforo's billing-service config).
type DunningConfig struct {
	MaxRetries int
	// EscalateAfterRetries — when DunningAttempt reaches this, the walker
	// expects the platform to flip the sub to SUSPEND. After SUSPEND, one
	// more failed attempt should escalate to CANCEL.
	EscalateAfterRetries int
}

// DefaultDunningConfig returns the values matching Aforo's reference config.
func DefaultDunningConfig() DunningConfig {
	return DunningConfig{
		MaxRetries:           3,
		EscalateAfterRetries: 3,
	}
}

// DunningWalker tracks per-sub retry counters and decides when to fire
// RetryPayment vs when to assert "platform should have escalated".
//
// Concurrency: safe — the per-sub counter is mutex-guarded.
type DunningWalker struct {
	cfg    DunningConfig
	mu     sync.Mutex
	count  map[string]int
}

// NewDunningWalker constructs a walker.
func NewDunningWalker(cfg DunningConfig) *DunningWalker {
	if cfg.MaxRetries <= 0 {
		cfg = DefaultDunningConfig()
	}
	return &DunningWalker{cfg: cfg, count: map[string]int{}}
}

// Step is invoked when the agent picks DUNNING_STEP for a PAST_DUE sub.
// Increments the per-sub retry counter, fires retry_payment, and — when
// the counter exceeds MaxRetries — expects the platform to have escalated.
//
// The walker logs:
//
//   - one DUNNING_STEP record per attempt below MaxRetries
//   - one DUNNING_ESCALATE record when count > MaxRetries (asserts the
//     sub is now SUSPENDED or CANCELLED on the platform side)
func (w *DunningWalker) Step(ctx context.Context, deps Deps, s Subject) error {
	w.mu.Lock()
	w.count[s.SubscriptionID]++
	current := w.count[s.SubscriptionID]
	w.mu.Unlock()

	kind := TransitionDunningStep
	if current > w.cfg.MaxRetries {
		kind = TransitionDunningEscalate
	}

	rec := newIntent(s, kind)
	rec.DunningAttempt = current
	rec.IdempotencyKey = fmt.Sprintf("%s-step-%d", idempotencyKey(s, TransitionDunningStep), current)

	if kind == TransitionDunningStep {
		// Below the escalation threshold — fire retry_payment and watch
		// for the platform's response.
		if err := deps.Log.Append(rec); err != nil {
			return err
		}
		if err := FireRetryPayment(ctx, deps, s); err != nil {
			// retry_payment already logged its own row; record the dunning
			// step as FAIL with the same error for traceability.
			fail := rec
			fail.TransitionStatus = StatusFail
			fail.Error = FormatError(err)
			_ = deps.Log.Append(fail)
			return err
		}
		ok := rec
		ok.TransitionStatus = StatusOK
		return deps.Log.Append(ok)
	}

	// Escalation step — assert that the platform's view is SUSPEND or
	// CANCEL. The agent doesn't have a fast read path here; we record
	// the assertion as the expected post-state and let the validator
	// (Check 9.f) cross-check with a live GET.
	rec.ExpectedPostState = string(scenario.StateSuspended)
	rec.TransitionStatus = StatusOK
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	deps.Picker.SetLiveState(s.SubscriptionID, scenario.StateSuspended)
	deps.Picker.MarkSuspended(s.SubscriptionID)
	return nil
}

// Counter returns the current retry counter for sub (test introspection).
func (w *DunningWalker) Counter(subID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count[subID]
}

// Reset clears the counter for sub (called when sub recovers to ACTIVE).
func (w *DunningWalker) Reset(subID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.count, subID)
}
