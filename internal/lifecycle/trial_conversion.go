package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// convertTrialResponse decodes the platform's response to /convert-trial.
type convertTrialResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	OfferingID string `json:"offeringId"`
}

// FireTrialConversion converts a TRIALING sub to ACTIVE. The platform
// only allows this from TRIALING; other states return 409 (which the
// picker should have filtered out — but we tolerate the race with another
// goroutine that just changed state).
//
// Validator (Check 9.d) verifies billing starts at the conversion timestamp
// — no retroactive charges for the trial period.
func FireTrialConversion(ctx context.Context, deps Deps, s Subject) error {
	rec := newIntent(s, TransitionTrialConversion)
	rec.IdempotencyKey = idempotencyKey(s, TransitionTrialConversion)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log trial-conversion intent: %w", err)
	}

	var resp convertTrialResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionConvertTrial, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		nil,
		&resp,
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
	rec.ExpectedPostState = string(scenario.StateActive)
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	deps.Picker.SetLiveState(s.SubscriptionID, scenario.StateActive)
	return nil
}
