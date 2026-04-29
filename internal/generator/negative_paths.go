package generator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand"
	"sort"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// NegativePathKind identifies which (if any) fault was injected into an event.
type NegativePathKind string

const (
	NPNone      NegativePathKind = ""
	NPLate      NegativePathKind = "late_event"
	NPFuture    NegativePathKind = "future_event"
	NPMalformed NegativePathKind = "malformed"
	NPWrongAuth NegativePathKind = "wrong_auth"
	NPStaleKey  NegativePathKind = "stale_key"
	NPOversize  NegativePathKind = "oversize"
)

// AllNegativePaths is the canonical iteration order for metrics + run.json.
var AllNegativePaths = []NegativePathKind{
	NPLate, NPFuture, NPMalformed, NPWrongAuth, NPStaleKey, NPOversize,
}

// NegativePathPlanner picks which negative path (if any) applies to the
// next event. Each event rolls one Bernoulli per kind, in deterministic
// order. If multiple roll true, the first match wins (rare at typical
// percentages — total negative-path share is usually <10%).
type NegativePathPlanner struct {
	cfg         scenario.NegativePaths
	staleSubs   []seed.ManifestSubscription // pre-filtered to Stale==true
	keysPerSub  [][]seed.ManifestAPIKey     // pre-filtered to Revoked==true
	stalePicker IndexPicker
	keyPickers  []IndexPicker
	enabled     bool
	thresholds  [6]float64 // cumulative thresholds for {late, future, malformed, wrongAuth, staleKey, oversize}
	kinds       [6]NegativePathKind
}

// NewNegativePathPlanner inspects the manifest for stale subscriptions and
// builds the per-kind thresholds. If scenario calls for stale_key but the
// manifest has zero stale keys, returns an error — fail at startup.
func NewNegativePathPlanner(cfg scenario.NegativePaths, manifest *seed.Manifest) (*NegativePathPlanner, error) {
	p := &NegativePathPlanner{cfg: cfg}
	p.kinds = [6]NegativePathKind{NPLate, NPFuture, NPMalformed, NPWrongAuth, NPStaleKey, NPOversize}

	share := [6]float64{
		cfg.LateEventsPct,
		cfg.FutureEventsPct,
		cfg.MalformedPct,
		cfg.WrongAuthPct,
		cfg.StaleKeysPct,
		cfg.OversizePct,
	}
	for _, s := range share {
		if s < 0 {
			return nil, fmt.Errorf("negative-path percentage cannot be negative")
		}
	}

	total := 0.0
	for _, s := range share {
		total += s
	}
	if total > 1.0 {
		return nil, fmt.Errorf("negative-path total share %.4f exceeds 1.0 (validator should have caught this)", total)
	}

	if cfg.StaleKeysPct > 0 {
		p.staleSubs, p.keysPerSub = collectStaleSubs(manifest)
		if len(p.staleSubs) == 0 {
			return nil, errors.New("scenario.negative_paths.stale_keys_pct > 0 but manifest has zero stale subscriptions; rerun seed with subscription_state_mix including CANCELLED or EXPIRED")
		}
		// Build per-sub key pickers (uniform over revoked keys for that sub).
		p.keyPickers = make([]IndexPicker, len(p.keysPerSub))
		for i, ks := range p.keysPerSub {
			p.keyPickers[i] = NewIndexPicker(UniformWeights(len(ks)))
		}
		w := make([]float64, len(p.staleSubs))
		// Weight stale subs by # of revoked keys they have so distribution is
		// proportional to actual revoked-key population.
		for i, ks := range p.keysPerSub {
			w[i] = float64(len(ks))
		}
		p.stalePicker = NewIndexPicker(w)
	}

	// Cumulative thresholds — single rng.Float64 chooses among them.
	cum := 0.0
	for i, s := range share {
		cum += s
		p.thresholds[i] = cum
	}
	p.enabled = total > 0
	return p, nil
}

// HasStaleCapacity reports whether the planner can inject stale_key faults.
// Used by the runner to verify the scenario/manifest pairing at startup.
func (p *NegativePathPlanner) HasStaleCapacity() bool { return len(p.staleSubs) > 0 }

// StaleSubsCount reports how many stale subscriptions are available.
func (p *NegativePathPlanner) StaleSubsCount() int { return len(p.staleSubs) }

// Pick rolls for a negative path. Returns NPNone when no fault is selected.
func (p *NegativePathPlanner) Pick(rng *mathrand.Rand) NegativePathKind {
	if !p.enabled {
		return NPNone
	}
	r := rng.Float64()
	for i, t := range p.thresholds {
		if r < t {
			return p.kinds[i]
		}
	}
	return NPNone
}

// Apply mutates the event in place to inject the given fault. Returns an
// error only if the fault is logically infeasible (e.g. stale_key with no
// stale subs — guarded earlier but defended here too).
func (p *NegativePathPlanner) Apply(e *Event, kind NegativePathKind, rng *mathrand.Rand) error {
	switch kind {
	case NPNone:
		return nil
	case NPLate:
		return injectLate(e)
	case NPFuture:
		return injectFuture(e)
	case NPMalformed:
		return injectMalformed(e)
	case NPWrongAuth:
		return injectWrongAuth(e, rng)
	case NPStaleKey:
		return p.injectStaleKey(e, rng)
	case NPOversize:
		return injectOversize(e, rng)
	}
	return fmt.Errorf("unknown negative path %q", kind)
}

// --- per-kind injectors ---

// late_event: backdate the timestamp 2h. Aforo accepts it (>5min late but
// <90d) and flags it as a late arrival.
func injectLate(e *Event) error {
	e.Envelope.EventTimestamp = e.Envelope.EventTimestamp.Add(-2 * time.Hour)
	e.NegativePath = NPLate
	return nil
}

// future_event: forward-date 10 minutes. Aforo's UsageEventValidator
// rejects anything >5min in the future.
func injectFuture(e *Event) error {
	e.Envelope.EventTimestamp = e.Envelope.EventTimestamp.Add(10 * time.Minute)
	e.NegativePath = NPFuture
	return nil
}

// malformed: corrupt the JSON body. We emit it as a raw byte slice so the
// driver doesn't re-marshal a valid envelope. The driver's RawBody field
// takes precedence over Envelope when set.
func injectMalformed(e *Event) error {
	// Build a roughly-shaped envelope with a dangling brace + truncated
	// field. Real malformed traffic in production looks like this when a
	// client SDK is misconfigured or a buffer is truncated mid-emit.
	corrupted := []byte(fmt.Sprintf(
		`{"event_id":%q,"tenant_id":%q,"event_timestamp":%q,"body":{"endpoint":"/api/v1/users"`,
		e.Envelope.EventID,
		e.Envelope.TenantID,
		e.Envelope.EventTimestamp.UTC().Format(time.RFC3339Nano),
	))
	e.RawBody = corrupted
	e.NegativePath = NPMalformed
	return nil
}

// wrong_auth: substitute a fabricated key. MUST never collide with a real
// manifest key. We use crypto/rand for the secret so determinism is
// sacrificed locally — the test for "no overlap with manifest" still
// holds because real keys are generated by Aforo's pricing-service from a
// different namespace ("sk_live_<32hex>" pattern with high entropy).
//
// We tag the event with the fabricated key on Auth so the driver can use it
// directly, bypassing the runner's "lookup customer's key" path.
func injectWrongAuth(e *Event, _ *mathrand.Rand) error {
	// 32 hex chars of cryptographic randomness — collision probability with
	// any real seeded key is < 2^-128.
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("crypto/rand: %w", err)
	}
	fab := "sk_live_fab_" + hex.EncodeToString(buf)
	e.Auth.Token = fab
	e.Auth.IsFabricated = true
	e.NegativePath = NPWrongAuth
	return nil
}

// stale_key: pick one of the stale subscriptions' revoked keys and tag the
// event with it. The event body and timestamp remain valid; only the key
// is wrong.
func (p *NegativePathPlanner) injectStaleKey(e *Event, rng *mathrand.Rand) error {
	if len(p.staleSubs) == 0 {
		return errors.New("stale_key requested but planner has no stale subs")
	}
	subIdx := p.stalePicker.Pick(rng)
	if subIdx < 0 || subIdx >= len(p.staleSubs) {
		return errors.New("stale_key picker returned invalid index")
	}
	sub := p.staleSubs[subIdx]
	keys := p.keysPerSub[subIdx]
	if len(keys) == 0 {
		return fmt.Errorf("stale_key: subscription %s has no revoked keys", sub.SubscriptionID)
	}
	keyIdx := p.keyPickers[subIdx].Pick(rng)
	if keyIdx < 0 || keyIdx >= len(keys) {
		keyIdx = 0
	}
	key := keys[keyIdx]
	e.Auth.Token = key.Secret
	e.Auth.ClientID = key.ClientID
	e.Auth.IsFabricated = false
	e.Envelope.SubscriptionID = sub.SubscriptionID
	e.NegativePath = NPStaleKey
	e.StaleReason = sub.StaleReason
	e.StaleSince = sub.StaleSince
	e.StaleSubscriptionID = sub.SubscriptionID
	return nil
}

// oversize: pad the body until JSON encoding exceeds 10 MiB. The default
// max body size at the Aforo ingress is 10 MiB so this MUST be rejected.
//
// Performance note: we don't need cryptographically random padding — we
// just need a string longer than the limit. Building 10 MiB byte-by-byte
// with rng is ~10 million rng calls per oversize event, which under the
// race detector takes ~50-100 ms each (and tanks the test suite). Instead
// we generate a 1 KiB rng-shaped seed and Repeat it. The resulting body
// is still varied across events (different seeds) and remains well above
// the limit.
func injectOversize(e *Event, rng *mathrand.Rand) error {
	const targetBytes = 10*1024*1024 + 1
	const seedBytes = 1024
	seed := padString(rng, seedBytes)
	repeats := (targetBytes / seedBytes) + 1
	e.Envelope.Body["_oversize_pad"] = strings.Repeat(seed, repeats)
	e.NegativePath = NPOversize
	return nil
}

// --- manifest helpers ---

// collectStaleSubs walks the manifest and returns subscriptions where
// Stale==true plus their revoked keys (only Revoked==true keys are kept,
// since active keys on a stale sub aren't legitimate stale-key fodder).
//
// Output is sorted by SubscriptionID for deterministic indexing.
func collectStaleSubs(m *seed.Manifest) ([]seed.ManifestSubscription, [][]seed.ManifestAPIKey) {
	if m == nil {
		return nil, nil
	}
	var subs []seed.ManifestSubscription
	var keys [][]seed.ManifestAPIKey
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if !s.Stale {
					continue
				}
				revoked := make([]seed.ManifestAPIKey, 0, len(s.APIKeys))
				for _, k := range s.APIKeys {
					if k.Revoked {
						revoked = append(revoked, k)
					}
				}
				if len(revoked) == 0 {
					// Stale sub without revoked keys is a seed harness bug —
					// not our problem to surface here; we just skip.
					continue
				}
				subs = append(subs, s)
				keys = append(keys, revoked)
			}
		}
	}
	// Stable sort by SubscriptionID so weighted picker indices line up
	// across runs with the same manifest.
	sort.SliceStable(subs, func(i, j int) bool {
		return subs[i].SubscriptionID < subs[j].SubscriptionID
	})
	// keys array must follow subs reordering. Build a lookup and resort.
	// (cheaper than zipping into a struct; len(subs) is small relative to
	// per-event work)
	if len(subs) > 1 {
		idx := make(map[string]int, len(subs))
		for i, s := range subs {
			idx[s.SubscriptionID] = i
		}
		newKeys := make([][]seed.ManifestAPIKey, len(subs))
		for _, s := range subs {
			newKeys[idx[s.SubscriptionID]] = nil // initialize
		}
		// Iterate the original (pre-sort) gather order — we don't have it
		// captured separately, so re-walk the manifest preserving the
		// already-resolved revoked-only filter.
		original := make(map[string][]seed.ManifestAPIKey, len(subs))
		for _, t := range m.Tenants {
			for _, c := range t.Customers {
				for _, s := range c.Subscriptions {
					if !s.Stale {
						continue
					}
					revoked := make([]seed.ManifestAPIKey, 0, len(s.APIKeys))
					for _, k := range s.APIKeys {
						if k.Revoked {
							revoked = append(revoked, k)
						}
					}
					if len(revoked) > 0 {
						original[s.SubscriptionID] = revoked
					}
				}
			}
		}
		for sid, ks := range original {
			if i, ok := idx[sid]; ok {
				newKeys[i] = ks
			}
		}
		keys = newKeys
	}
	return subs, keys
}
