package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// upgradeRequest mirrors com.aforo.billing.subscription.dto.UpgradeRequest.
// Fields the platform requires:
//   - targetOfferingId: new offering to switch to
//   - reason: optional human note (loadgen always tags so analytics can
//     filter "real customer change" from "synthetic")
//   - effectiveAt: omit → now (platform default)
type upgradeRequest struct {
	TargetOfferingID string `json:"targetOfferingId"`
	Reason           string `json:"reason,omitempty"`
}

// upgradeResponse is the platform's SubscriptionDTO subset we care about.
// We don't decode the full body — only the fields used by the validator.
type upgradeResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	OfferingID string `json:"offeringId"`
}

// FireUpgrade attempts to upgrade s to a new offering. Logs an INTENT
// row before the API call, then OUTCOME after. Returns nil on OK or
// SKIP, error on FAIL — the agent records both either way.
func FireUpgrade(ctx context.Context, deps Deps, s Subject) error {
	target := deps.Picker.PickMigrateTarget(s)
	if target == "" {
		// Single-offering tenant — upgrade is a no-op. Log SKIP and bail.
		return logSkip(deps.Log, s, TransitionUpgrade, "no alternate offering on tenant")
	}

	rec := newIntent(s, TransitionUpgrade)
	rec.FromOffering = s.CurrentOffer
	rec.ToOffering = target
	rec.IdempotencyKey = idempotencyKey(s, TransitionUpgrade)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log upgrade intent: %w", err)
	}

	body := upgradeRequest{TargetOfferingID: target, Reason: "loadgen-upgrade"}
	var resp upgradeResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionUpgrade, s.SubscriptionID),
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
	rec.ExpectedPostState = string(ExpectedPostState(TransitionUpgrade, s.State))
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	// Update the picker's view of state so concurrent kinds see fresh data.
	if resp.Status != "" {
		deps.Picker.SetLiveState(s.SubscriptionID, fromPlatformStatus(resp.Status))
	}
	return nil
}
