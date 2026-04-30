package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// downgradeRequest mirrors com.aforo.billing.subscription.dto.DowngradeRequest.
type downgradeRequest struct {
	TargetOfferingID string `json:"targetOfferingId"`
	Reason           string `json:"reason,omitempty"`
}

type downgradeResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	OfferingID string `json:"offeringId"`
}

// FireDowngrade attempts to move s to a less-expensive offering. The
// platform applies pro-rata credit at next bill run; the validator (Check 9)
// looks for that credit on the resulting invoice.
func FireDowngrade(ctx context.Context, deps Deps, s Subject) error {
	target := deps.Picker.PickMigrateTarget(s)
	if target == "" {
		return logSkip(deps.Log, s, TransitionDowngrade, "no alternate offering on tenant")
	}

	rec := newIntent(s, TransitionDowngrade)
	rec.FromOffering = s.CurrentOffer
	rec.ToOffering = target
	rec.IdempotencyKey = idempotencyKey(s, TransitionDowngrade)
	// Conservative pro-ration estimate — the agent doesn't know the exact
	// remaining-period proportion, so we record 0 here and let the validator
	// compute the expected number from the actual rate plan + period dates.
	rec.ExpectedProrationCreditUSD = 0
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log downgrade intent: %w", err)
	}

	body := downgradeRequest{TargetOfferingID: target, Reason: "loadgen-downgrade"}
	var resp downgradeResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionDowngrade, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		body,
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
	rec.ExpectedPostState = string(ExpectedPostState(TransitionDowngrade, s.State))
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	if resp.Status != "" {
		deps.Picker.SetLiveState(s.SubscriptionID, fromPlatformStatus(resp.Status))
	}
	return nil
}
