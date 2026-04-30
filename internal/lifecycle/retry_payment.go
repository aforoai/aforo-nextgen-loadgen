package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// retryPaymentResponse decodes /retry-payment. Status reflects success
// (ACTIVE) or persistent failure (still PAST_DUE / SUSPENDED).
type retryPaymentResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	DunningAttempt int   `json:"dunningAttempt"`
}

// FireRetryPayment retries the failed payment on a PAST_DUE/SUSPENDED sub.
// Outcome is platform-decided — we just record the resulting state.
//
// The validator (Check 9.f via the dunning_walker) verifies that subs
// reaching the configured max retry count escalate to SUSPEND/CANCEL.
func FireRetryPayment(ctx context.Context, deps Deps, s Subject) error {
	rec := newIntent(s, TransitionRetryPayment)
	rec.IdempotencyKey = idempotencyKey(s, TransitionRetryPayment)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log retry-payment intent: %w", err)
	}

	var resp retryPaymentResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionRetryPayment, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		nil,
		&resp,
	)
	rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	rec.HTTPStatus = status
	rec.DunningAttempt = resp.DunningAttempt
	if err != nil {
		rec.TransitionStatus = StatusFail
		rec.Error = FormatError(err)
		_ = deps.Log.Append(rec)
		return err
	}
	rec.TransitionStatus = StatusOK
	if resp.Status != "" {
		rec.ExpectedPostState = resp.Status
		deps.Picker.SetLiveState(s.SubscriptionID, fromPlatformStatus(resp.Status))
	} else {
		// Optimistic default — platform usually reports either ACTIVE or
		// PAST_DUE preserved.
		rec.ExpectedPostState = string(scenario.StateActive)
	}
	return deps.Log.Append(rec)
}
