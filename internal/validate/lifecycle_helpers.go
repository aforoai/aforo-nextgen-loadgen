package validate

import (
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// scenarioState parses a wire-level state string ("ACTIVE", "active") into
// the typed scenario.SubscriptionState. Tolerant of casing and whitespace —
// transitions.jsonl carries strings sourced from the platform.
func scenarioState(s string) scenario.SubscriptionState {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CREATED":
		return scenario.StateCreated
	case "TRIALING":
		return scenario.StateTrialing
	case "ACTIVE":
		return scenario.StateActive
	case "PAST_DUE":
		return scenario.StatePastDue
	case "PAUSED":
		return scenario.StatePaused
	case "EXPIRING_SOON":
		return scenario.StateExpiringSoon
	case "EXPIRED":
		return scenario.StateExpired
	case "CANCELLED":
		return scenario.StateCancelled
	case "SUSPENDED":
		return scenario.StateSuspended
	}
	return scenario.SubscriptionState(strings.TrimSpace(s))
}

// pickEligibleSub returns the first non-terminal subscription on the first
// tenant that has at least one such sub plus at least 2 offerings. The
// caller (Check 11) needs the second condition for migrate to have a
// different target.
//
// Determinism: walks tenants in manifest order; manifest is finalized by
// Manifest.Finalize() which sorts by external_id, so this is reproducible.
func pickEligibleSub(m *seed.Manifest) (seed.ManifestTenant, seed.ManifestSubscription, bool) {
	for _, t := range m.Tenants {
		if len(t.Offerings) < 2 {
			continue
		}
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if isTerminalState(s.Status) {
					continue
				}
				return t, s, true
			}
		}
	}
	return seed.ManifestTenant{}, seed.ManifestSubscription{}, false
}

// isTerminalState mirrors lifecycle.IsTerminal but lives in the validate
// package so we don't take a dependency just for one boolean. Same source
// of truth: the platform's V3 SubscriptionStateMachine.
func isTerminalState(s scenario.SubscriptionState) bool {
	return s == scenario.StateCancelled || s == scenario.StateExpired
}
