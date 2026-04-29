package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// verifyStaleKeys confirms that a CANCELLED or EXPIRED subscription has had
// its API keys revoked. The Aforo platform guarantees this via:
//
//	Subscription cancel  →  ApiKeyServiceImpl.cancel() sets revoked=true
//	                        atomically in the same @Transactional scope
//	Subscription expire  →  SubscriptionExpiryJob runs every 5 minutes and
//	                        revokes keys for subs whose end_date is in the past
//
// We GET each api-key after the state transition and require revoked=true on
// CANCELLED. For EXPIRED we accept either revoked=true (synchronous internal
// expire endpoint) OR a marker that the platform's expiry job will get to it.
//
// Returns an error only when the contract is violated — e.g. CANCELLED with
// keys still active. Tests assert against the marker fields on each
// ManifestAPIKey.
func verifyStaleKeys(ctx context.Context, c *Client, tenantID string, sub *ManifestSubscription, cancelTime time.Time) error {
	if sub == nil {
		return nil
	}
	switch sub.Status {
	case scenario.StateCancelled:
		return verifyAllKeysRevoked(ctx, c, tenantID, sub, "subscription_cancelled", &cancelTime)
	case scenario.StateExpired:
		// EXPIRED via the synchronous internal endpoint should be revoked
		// immediately; via natural expiry the platform schedules it within
		// 5 minutes. We mark stale_since as cancelTime in either case so the
		// manifest is honest about when we initiated the transition; revoked
		// flag may briefly be false until the next expiry-job tick.
		return verifyAllKeysRevoked(ctx, c, tenantID, sub, "subscription_expired", &cancelTime)
	}
	// Non-terminal states have no stale-key contract.
	return nil
}

// verifyAllKeysRevoked re-fetches each API key on the subscription and updates
// the manifest entries. Returns an error if a CANCELLED subscription's key is
// still active — that's a platform regression, not a transient hiccup.
//
// On dry-run, fetchAPIKey returns a synthetic revoked=true response, so the
// manifest reflects the expected post-revocation state without networking.
func verifyAllKeysRevoked(ctx context.Context, c *Client, tenantID string, sub *ManifestSubscription, reason string, staleSince *time.Time) error {
	sub.Stale = true
	sub.StaleReason = reason
	sub.StaleSince = staleSince

	for i := range sub.APIKeys {
		k := &sub.APIKeys[i]
		if k.KeyID == "" {
			continue
		}
		fetched, err := fetchAPIKey(ctx, c, tenantID, k.KeyID)
		if err != nil {
			// Don't poison the run on a transient lookup failure — log via
			// the error return; caller can decide whether to fail the run.
			return fmt.Errorf("fetch key %s for stale verification: %w", k.KeyID, err)
		}
		k.Revoked = fetched.Revoked
		k.RevokedAt = fetched.RevokedAt

		// CANCELLED is the strict contract — if we initiated /cancel and the
		// key isn't revoked, that's a platform bug we want surfaced.
		if sub.Status == scenario.StateCancelled && !fetched.Revoked {
			return fmt.Errorf("api key %s on CANCELLED sub %s should be revoked but is not", k.KeyID, sub.SubscriptionID)
		}
	}
	return nil
}
