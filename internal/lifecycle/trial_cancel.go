package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// cancelTrialBody is the platform's CancelSubscriptionRequest. Loadgen tags
// every cancel so analytics can filter "real customer churn" out.
type cancelTrialBody struct {
	Reason string `json:"reason,omitempty"`
}

// FireTrialCancel cancels a TRIALING sub before its trial ends. The
// resulting state is CANCELLED, the api keys cascade to revoked, and the
// validator (Check 9.f / Check 6.e — stale-key) probes that subsequent
// events on the cancelled key are rejected with 401/403.
//
// We deliberately use the /cancel endpoint (not DELETE) — only /cancel
// triggers the platform's atomic key-revocation cascade.
func FireTrialCancel(ctx context.Context, deps Deps, s Subject) error {
	rec := newIntent(s, TransitionTrialCancel)
	rec.IdempotencyKey = idempotencyKey(s, TransitionTrialCancel)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log trial-cancel intent: %w", err)
	}

	body := cancelTrialBody{Reason: "loadgen-trial-cancel"}
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionCancel, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		body,
		nil,
	)
	rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	rec.HTTPStatus = status
	if err != nil {
		rec.TransitionStatus = StatusFail
		rec.Error = FormatError(err)
		_ = deps.Log.Append(rec)
		return err
	}
	rec.TransitionStatus = StatusOK
	rec.ExpectedPostState = string(scenario.StateCancelled)
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	// Mark the picker so we never pick this sub again.
	deps.Picker.SetLiveState(s.SubscriptionID, scenario.StateCancelled)
	deps.Picker.MarkSuspended(s.SubscriptionID)
	return nil
}
