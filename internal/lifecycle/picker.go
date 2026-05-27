package lifecycle

import (
	"math/rand"
	"sort"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// Subject is one (tenant, customer, subscription) triple along with the
// archetype it came from + the offerings the tenant has available. The
// agent passes Subjects to transition modules, never raw manifest types.
type Subject struct {
	TenantID       string
	Archetype      string
	PricingModel   scenario.PricingModel
	BillingMode    scenario.BillingMode
	CustomerID     string
	SubscriptionID string
	// SubSeedKey is the loadgen-internal Idempotency-Key value that was used
	// when the subscription was provisioned. Useful for grep-debugging
	// across seed + lifecycle logs. NOT a backend column on subscriptions
	// (per CONVENTIONS.md — only tenants legitimately carry externalId).
	SubSeedKey   string
	State        scenario.SubscriptionState
	OfferingIDs  []string // every offering on the tenant — for migration target choice
	CurrentOffer string   // best-effort; empty when seed harness didn't capture
}

// Picker samples eligible Subjects for transition kinds. Sampling is
// deterministic in scenario.seed — the same scenario re-run picks the
// same subs in the same order, so a failing repro is reliable.
//
// Concurrency: safe for use by all per-kind goroutines simultaneously
// (the per-kind RNG is mutex-guarded).
type Picker struct {
	subjects []Subject

	mu  sync.Mutex
	rng *rand.Rand

	// liveStates tracks the agent's local view of each sub's current state.
	// Updated by the agent after a successful transition so subsequent picks
	// don't fire (e.g.) PAUSE on a sub already paused this minute.
	stateMu    sync.RWMutex
	liveStates map[string]scenario.SubscriptionState

	// suspended tracks subs the agent has marked unfit for picking — e.g.
	// one that just got dunning-escalated to SUSPENDED, or one whose
	// pause-resume agent has it scheduled for resume in the future.
	suspendedMu sync.RWMutex
	suspended   map[string]bool
}

// NewPicker walks the manifest and builds the Subject pool. Subjects in
// terminal states are EXCLUDED — they can never be transitioned (Check 10).
func NewPicker(m *seed.Manifest, seed int64) *Picker {
	subjects := []Subject{}
	for _, t := range m.Tenants {
		offerings := offeringIDs(t)
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if IsTerminal(s.Status) {
					continue
				}
				subjects = append(subjects, Subject{
					TenantID:       t.TenantID,
					Archetype:      t.Archetype,
					PricingModel:   t.PricingModel,
					BillingMode:    t.BillingMode,
					CustomerID:     c.CustomerID,
					SubscriptionID: s.SubscriptionID,
					SubSeedKey:     s.SeedKey,
					State:          s.Status,
					OfferingIDs:    offerings,
				})
			}
		}
	}
	// Stable sort by (tenant_id, sub_id) — RNG seeding is then truly
	// reproducible across runs.
	sort.Slice(subjects, func(i, j int) bool {
		if subjects[i].TenantID != subjects[j].TenantID {
			return subjects[i].TenantID < subjects[j].TenantID
		}
		return subjects[i].SubscriptionID < subjects[j].SubscriptionID
	})

	live := make(map[string]scenario.SubscriptionState, len(subjects))
	for _, s := range subjects {
		live[s.SubscriptionID] = s.State
	}
	return &Picker{
		subjects:   subjects,
		rng:        rand.New(rand.NewSource(seed)),
		liveStates: live,
		suspended:  map[string]bool{},
	}
}

// Subjects returns every subject the picker tracks. Caller MUST treat as
// read-only.
func (p *Picker) Subjects() []Subject { return p.subjects }

// EligibleCount reports how many subs are currently in a state where
// `kind` would be a legal attempt. Used by the agent to compute per-kind
// ticker periods.
func (p *Picker) EligibleCount(kind TransitionKind) int {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	p.suspendedMu.RLock()
	defer p.suspendedMu.RUnlock()
	count := 0
	for _, s := range p.subjects {
		if p.suspended[s.SubscriptionID] {
			continue
		}
		if CanFireFrom(kind, p.liveStates[s.SubscriptionID]) {
			count++
		}
	}
	return count
}

// PickFor returns one eligible Subject for kind, or false if none.
//
// Determinism: with the same picker seed and the same call sequence, the
// same Subject is returned on every run. The agent calls PickFor in a
// single goroutine per kind, so call ordering within a kind is stable.
func (p *Picker) PickFor(kind TransitionKind) (Subject, bool) {
	p.stateMu.RLock()
	p.suspendedMu.RLock()
	candidates := make([]Subject, 0, 16)
	for _, s := range p.subjects {
		if p.suspended[s.SubscriptionID] {
			continue
		}
		if CanFireFrom(kind, p.liveStates[s.SubscriptionID]) {
			s.State = p.liveStates[s.SubscriptionID]
			candidates = append(candidates, s)
		}
	}
	p.suspendedMu.RUnlock()
	p.stateMu.RUnlock()
	if len(candidates) == 0 {
		return Subject{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.rng.Intn(len(candidates))
	return candidates[idx], true
}

// SetLiveState updates the agent's view of a sub's state. Transition
// modules MUST call this after a successful API response so the picker
// reflects the platform's new state.
func (p *Picker) SetLiveState(subID string, state scenario.SubscriptionState) {
	p.stateMu.Lock()
	p.liveStates[subID] = state
	p.stateMu.Unlock()
}

// LiveState returns the agent's current view of sub's state.
func (p *Picker) LiveState(subID string) scenario.SubscriptionState {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.liveStates[subID]
}

// MarkSuspended removes a sub from the eligible set without changing its
// state. Used by pause_resume to hold a sub unschedulable while a deferred
// resume is in flight, and by dunning_walker after escalation.
func (p *Picker) MarkSuspended(subID string) {
	p.suspendedMu.Lock()
	p.suspended[subID] = true
	p.suspendedMu.Unlock()
}

// MarkLive un-suspends a sub.
func (p *Picker) MarkLive(subID string) {
	p.suspendedMu.Lock()
	delete(p.suspended, subID)
	p.suspendedMu.Unlock()
}

// PickMigrateTarget returns an offering id that is NOT s.CurrentOffer.
// Returns the empty string if the tenant has zero or one offering — such
// a migrate is a no-op and the agent skips it.
func (p *Picker) PickMigrateTarget(s Subject) string {
	others := make([]string, 0, len(s.OfferingIDs))
	for _, oid := range s.OfferingIDs {
		if oid != s.CurrentOffer {
			others = append(others, oid)
		}
	}
	if len(others) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return others[p.rng.Intn(len(others))]
}

// Probability picks true with probability p. Determinism is preserved
// through the picker's seeded RNG. Used by retry_payment + dunning_walker
// to flip success/failure outcomes without making a network call.
func (p *Picker) Probability(prob float64) bool {
	if prob <= 0 {
		return false
	}
	if prob >= 1 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rng.Float64() < prob
}

func offeringIDs(t seed.ManifestTenant) []string {
	out := make([]string, 0, len(t.Offerings))
	for _, o := range t.Offerings {
		if o.OfferingID != "" {
			out = append(out, o.OfferingID)
		}
	}
	return out
}
