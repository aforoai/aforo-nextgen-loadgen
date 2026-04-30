package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Deps is the bundle every transition module needs. The agent constructs
// it once and passes the same struct to every Fire* call.
type Deps struct {
	Client        *Client
	Log           *TransitionLog
	Picker        *Picker
	Resumes       *ResumeScheduler
	Dunning       *DunningWalker
	RunID         string
	ResumeTimeout time.Duration // detached-context timeout for deferred resume
}

// newIntent builds a TransitionRecord for the INTENT log row, written
// immediately before the API call. Status is StatusPending so the
// validator's counters don't double-count intent + outcome.
//
// Caller mutates the returned record before logging the OUTCOME row:
// flip TransitionStatus to OK/FAIL/SKIPPED, set HTTPStatus + DurationMs.
func newIntent(s Subject, kind TransitionKind) TransitionRecord {
	return TransitionRecord{
		Timestamp:        time.Now().UTC(),
		SubscriptionID:   s.SubscriptionID,
		TenantID:         s.TenantID,
		CustomerID:       s.CustomerID,
		Archetype:        s.Archetype,
		Transition:       kind,
		FromState:        string(s.State),
		TransitionStatus: StatusPending,
	}
}

// logSkip records a SKIPPED transition row and returns nil (the agent
// doesn't treat skips as failures).
func logSkip(log *TransitionLog, s Subject, kind TransitionKind, reason string) error {
	rec := newIntent(s, kind)
	rec.TransitionStatus = StatusSkipped
	rec.Error = reason
	return log.Append(rec)
}

// idempotencyKey derives a deterministic key for a transition.
//
// CONTRACT — same (sub, kind, day-of-month) yields the same key. The day
// component allows retrying the same transition kind on a sub the next day,
// which is the realistic operator pattern. A `time.Now()`-based version
// would defeat the platform's idempotency replay shortcut.
func idempotencyKey(s Subject, kind TransitionKind) string {
	day := time.Now().UTC().Format("2006-01-02")
	raw := fmt.Sprintf("loadgen|%s|%s|%s|%s", s.SubscriptionID, kind, s.TenantID, day)
	sum := sha256.Sum256([]byte(raw))
	return "lg-" + hex.EncodeToString(sum[:])[:32]
}

// fromPlatformStatus maps the platform's wire-level status string to our
// scenario.SubscriptionState type. Tolerant — unknown values map to
// StateActive (the agent's default optimistic assumption) rather than
// crashing.
func fromPlatformStatus(s string) scenario.SubscriptionState {
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
	return scenario.StateActive
}
