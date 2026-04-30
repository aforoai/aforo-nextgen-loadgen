package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// migrateRequest mirrors com.aforo.billing.subscription.dto.FullProrationMigrateRequest.
// The full-proration endpoint computes calendar bilateral, wallet, and
// per-metric usage refunds in a single call — exactly what the validator
// (Check 9.e) wants to assert against.
type migrateRequest struct {
	TargetOfferingID string `json:"targetOfferingId"`
	Reason           string `json:"reason,omitempty"`
	// AutoSettleRefund=false ensures we don't generate a credit note —
	// the run shouldn't create real refundable artifacts. Operators with
	// production data set this to true; our validator reads the refund
	// from the response.
	AutoSettleRefund bool `json:"autoSettleRefund"`
}

// migrateResponse is the platform's MigrationOutcome subset we care about.
// Stable-id semantic: source == target subscription id (Check 9.e).
type migrateResponse struct {
	SourceSubscriptionID string  `json:"sourceSubscriptionId"`
	TargetSubscriptionID string  `json:"targetSubscriptionId"`
	CalendarRefundUSD    float64 `json:"calendarRefundUsd"`
	WalletRefund         struct {
		WalletBalance float64 `json:"walletBalance"`
	} `json:"walletRefund"`
}

// FireMigrate moves a sub to a different offering with full pro-ration.
// The platform's migrateInPlace path keeps the subscription id stable —
// the validator confirms source==target on the audit row.
func FireMigrate(ctx context.Context, deps Deps, s Subject) error {
	target := deps.Picker.PickMigrateTarget(s)
	if target == "" {
		return logSkip(deps.Log, s, TransitionMigrate, "no alternate offering on tenant")
	}

	rec := newIntent(s, TransitionMigrate)
	rec.FromOffering = s.CurrentOffer
	rec.ToOffering = target
	rec.IdempotencyKey = idempotencyKey(s, TransitionMigrate)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log migrate intent: %w", err)
	}

	body := migrateRequest{
		TargetOfferingID: target,
		Reason:           "loadgen-migrate",
		AutoSettleRefund: false,
	}
	var resp migrateResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionMigrateProration, s.SubscriptionID),
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
	rec.ExpectedPostState = string(ExpectedPostState(TransitionMigrate, s.State))
	rec.ExpectedProrationCreditUSD = resp.CalendarRefundUSD
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	// Stable-id contract — record what the platform reported so the validator
	// can assert source == target.
	if resp.SourceSubscriptionID != "" && resp.TargetSubscriptionID != "" &&
		resp.SourceSubscriptionID != resp.TargetSubscriptionID {
		// Surface the violation immediately as a follow-up FAIL row — Check 9.e
		// will catch this at validate time too, but the live signal is useful.
		violation := newIntent(s, TransitionMigrate)
		violation.FromOffering = s.CurrentOffer
		violation.ToOffering = target
		violation.TransitionStatus = StatusFail
		violation.Error = fmt.Sprintf(
			"stable-id violation: source=%s target=%s",
			resp.SourceSubscriptionID, resp.TargetSubscriptionID,
		)
		_ = deps.Log.Append(violation)
	}
	return nil
}
