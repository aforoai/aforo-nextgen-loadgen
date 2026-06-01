package generator

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// Event is the on-the-wire shape the driver consumes. The runner passes
// these through a buffered channel between Generator and Driver — small
// enough to pass by value where convenient but we use pointers everywhere
// so the negative-path injectors can mutate fields cheaply.
type Event struct {
	Envelope      Envelope
	IngestionPath string           // e.g. "rest_direct"
	Archetype     string           // tenant archetype name
	PayloadSize   PayloadSize      // small/medium/large
	NegativePath  NegativePathKind // "" or one of 6
	Auth          EventAuth        // bearer/clientid for the driver
	RawBody       []byte           // when set (malformed), driver sends as-is
	StaleReason   string           // populated only for stale_key
	StaleSince    *time.Time       // populated only for stale_key
	GeneratedAt   time.Time        // wall-clock at generation
	// In-memory routing fields — used by the driver to set HTTP headers
	// but NOT serialized into the request body. The backend's
	// IngestUsageEventRequest has no tenant_id/subscription_id/event_id
	// fields; tenant flows via X-Tenant-Id header, subscription is looked
	// up by customer+metric+timestamp at billing time, and event id is
	// only used for cross-correlating loadgen logs with server-side
	// traces (X-Loadgen-Event-Id header).
	TenantID            string
	SubscriptionID      string
	EventID             string
	StaleSubscriptionID string // populated only for stale_key
}

// EventAuth tells the driver which credential to send. For BEARER_TOKEN
// keys, Token is the secret. For CLIENT_CREDENTIALS, Token is client_secret
// and ClientID is client_id. IsFabricated is true only for wrong_auth.
type EventAuth struct {
	Token        string
	ClientID     string
	IsFabricated bool
}

// Stats are exposed by the Generator so the metrics layer can increment
// counters without reaching into internal state. All counters use atomics
// so concurrent reads from the metrics goroutine never tear.
type Stats struct {
	Generated      atomic.Int64
	NegativeCounts [6]atomic.Int64 // indexed by AllNegativePaths order
	PerArchetype   sync.Map        // archetype name → *atomic.Int64
}

// Inc increments the per-archetype counter atomically.
func (s *Stats) IncArchetype(name string) {
	if v, ok := s.PerArchetype.Load(name); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	c := new(atomic.Int64)
	c.Store(1)
	if actual, loaded := s.PerArchetype.LoadOrStore(name, c); loaded {
		// Lost the race; some other goroutine stored a counter first.
		actual.(*atomic.Int64).Add(1)
	}
}

// Snapshot returns a copy of the per-archetype counts as a flat map.
func (s *Stats) ArchetypeSnapshot() map[string]int64 {
	out := map[string]int64{}
	s.PerArchetype.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// NegativeSnapshot returns a copy of the per-kind counters keyed by name.
func (s *Stats) NegativeSnapshot() map[NegativePathKind]int64 {
	out := make(map[NegativePathKind]int64, len(AllNegativePaths))
	for i, kind := range AllNegativePaths {
		out[kind] = s.NegativeCounts[i].Load()
	}
	return out
}

// Config configures the generator at construction time.
type Config struct {
	Scenario *scenario.Scenario
	Manifest *seed.Manifest
	Now      func() time.Time
	// BufferSize sets the depth of the events channel. Larger = more
	// generator-driver decoupling but more memory. Default 1024.
	BufferSize int
}

// Generator emits Events on its Out channel until the provided context is
// cancelled or the scenario duration elapses.
type Generator struct {
	cfg           Config
	scenario      *scenario.Scenario
	manifest      *seed.Manifest
	tenants       []indexedTenant
	tenantPicker  IndexPicker
	productPicker WeightedPicker
	ingestPicker  WeightedPicker
	payloadPicker PayloadSizePicker
	negPlanner    *NegativePathPlanner
	stats         *Stats
	now           func() time.Time
	out           chan *Event
	done          chan struct{}
	rng           *rand.Rand
	startedAt     time.Time
}

// indexedTenant is the per-tenant resolved bundle the generator picks from.
// Every event one of these is sampled, then a customer/sub/key triple is
// drawn from inside it.
type indexedTenant struct {
	manifestIdx int // index into manifest.Tenants
	tenant      *seed.ManifestTenant
	activeIdx   []customerSubKeyTriple // subscriptions in non-stale states
	// productTypes maps ProductType → metrics on every product of that
	// type in this tenant. We store both the ID (for back-compat) and
	// the Name (required by IngestUsageEventRequest.metricName). The
	// emit path uses Name. Drift-fix 2026-06-01.
	productTypes map[scenario.ProductType][]seed.ManifestMetric
}

// customerSubKeyTriple is one (customer, subscription, key) — used to drive
// happy-path traffic. Only ACTIVE/TRIALING subs land here; CANCELLED/EXPIRED
// are reserved for stale_key injection.
type customerSubKeyTriple struct {
	customer     *seed.ManifestCustomer
	subscription *seed.ManifestSubscription
	key          *seed.ManifestAPIKey
	productType  scenario.ProductType
}

// NewGenerator validates the scenario/manifest pairing and returns a ready
// Generator. The Run method must be called to start emitting events.
func NewGenerator(cfg Config) (*Generator, error) {
	if cfg.Scenario == nil {
		return nil, errors.New("generator: scenario is required")
	}
	if cfg.Manifest == nil {
		return nil, errors.New("generator: manifest is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}

	tenants, err := indexTenants(cfg.Manifest, cfg.Scenario)
	if err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return nil, errors.New("generator: manifest contains zero tenants")
	}

	// Tenant traffic shape depends on scenario.tenants.distribution.
	tenantWeights := tenantWeightsFor(len(tenants), cfg.Scenario.Tenants.Distribution)
	// Sort by external_id so manifests with identical contents but different
	// insertion order produce the same traffic shape.
	sort.SliceStable(tenants, func(i, j int) bool {
		return tenants[i].tenant.ExternalID < tenants[j].tenant.ExternalID
	})

	productPicker := NewWeightedPicker(map[string]float64{
		string(scenario.ProductAPI):        cfg.Scenario.ProductMix.API,
		string(scenario.ProductAIAgent):    cfg.Scenario.ProductMix.AIAgent,
		string(scenario.ProductMCPServer):  cfg.Scenario.ProductMix.MCPServer,
		string(scenario.ProductAgenticAPI): cfg.Scenario.ProductMix.AgenticAPI,
	})
	if productPicker.Len() == 0 || cfg.Scenario.ProductMix.Sum() <= 0 {
		// Default to all-API if not specified.
		productPicker = NewWeightedPicker(map[string]float64{string(scenario.ProductAPI): 1.0})
	}

	ingestPicker := NewWeightedPicker(ingestionWeightsFor(cfg.Scenario.IngestionPaths))
	if ingestPicker.Len() == 0 || cfg.Scenario.IngestionPaths.Sum() <= 0 {
		ingestPicker = NewWeightedPicker(map[string]float64{"rest_direct": 1.0})
	}

	payloadPicker := NewPayloadSizePicker(cfg.Scenario.PayloadVariation)
	negPlanner, err := NewNegativePathPlanner(cfg.Scenario.NegativePaths, cfg.Manifest)
	if err != nil {
		return nil, err
	}

	g := &Generator{
		cfg:           cfg,
		scenario:      cfg.Scenario,
		manifest:      cfg.Manifest,
		tenants:       tenants,
		tenantPicker:  NewIndexPicker(tenantWeights),
		productPicker: productPicker,
		ingestPicker:  ingestPicker,
		payloadPicker: payloadPicker,
		negPlanner:    negPlanner,
		stats:         &Stats{},
		now:           cfg.Now,
		out:           make(chan *Event, cfg.BufferSize),
		done:          make(chan struct{}),
		rng:           rand.New(rand.NewSource(cfg.Scenario.Seed)),
	}
	return g, nil
}

// Out returns the receive channel of generated events. Closed when the
// generator's Run loop exits.
func (g *Generator) Out() <-chan *Event { return g.out }

// Stats exposes counters for the metrics layer. Safe to call any time.
func (g *Generator) Stats() *Stats { return g.stats }

// TenantsActive returns the count of distinct tenants the generator can
// route traffic to. Reported as a gauge by the metrics layer.
func (g *Generator) TenantsActive() int { return len(g.tenants) }

// HasStaleCapacity reports whether stale_key injection has manifest backing.
// Used by the runner to fail fast if the scenario asks for stale_key but
// the manifest lacks them.
func (g *Generator) HasStaleCapacity() bool { return g.negPlanner.HasStaleCapacity() }

// Run drives the generator using the provided pacer until either ctx is
// cancelled or the scenario duration is reached. Closes the Out channel on
// exit so the driver knows to drain.
func (g *Generator) Run(ctx context.Context, pacer Pacer) error {
	g.startedAt = g.now()
	defer close(g.out)
	defer close(g.done)

	deadline := g.startedAt.Add(g.scenario.Duration.Std())
	for {
		// Stop if duration elapsed.
		if !deadline.IsZero() && g.now().After(deadline) {
			return nil
		}
		eventTime, err := pacer.Wait(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		evt, err := g.produce(eventTime)
		if err != nil {
			// Production-level failures (no triples, etc.) bubble up — the
			// runner converts to RunResult.Errors and continues.
			return fmt.Errorf("produce event: %w", err)
		}
		if evt == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case g.out <- evt:
			g.stats.Generated.Add(1)
			g.stats.IncArchetype(evt.Archetype)
			if evt.NegativePath != "" {
				if i := indexOfNegPath(evt.NegativePath); i >= 0 {
					g.stats.NegativeCounts[i].Add(1)
				}
			}
		}
	}
}

// produce composes one event end-to-end. Returns nil when the picked
// tenant has no eligible customer/sub/key for the picked product type —
// this can happen if a scenario product mix asks for AI_AGENT but the
// archetype only provisioned API products. Callers skip nil events and
// continue.
func (g *Generator) produce(eventTime time.Time) (*Event, error) {
	tenantIdx := g.tenantPicker.Pick(g.rng)
	if tenantIdx < 0 || tenantIdx >= len(g.tenants) {
		return nil, fmt.Errorf("tenantPicker out of range: %d", tenantIdx)
	}
	t := g.tenants[tenantIdx]
	productKind := scenario.ProductType(g.productPicker.Pick(g.rng))

	triple, ok := pickTripleForProduct(t, productKind, g.rng)
	if !ok {
		// Tenant doesn't carry that product type. We could fall back to
		// any available product on the tenant; doing so silently changes
		// the product mix. Instead, drop the event but record nothing —
		// the operator's metrics will show generated < target_tps if
		// product mix and archetype product_types disagree.
		return nil, nil
	}

	body := TemplateForProductType(productKind)(g.rng)
	size := g.payloadPicker.Pick(g.rng)
	ApplyPayloadSize(body, size, g.rng)

	// Pick a metric NAME for this event. Backend's IngestUsageEventRequest
	// looks up the metric by name (not id). When the tenant carries no
	// metric for the chosen productType (rare — archetype mismatch), we
	// drop the event because there's nothing for the backend to attribute
	// usage to. The same logic applied to metric IDs in the prior shape.
	metricName := ""
	if metrics := t.productTypes[productKind]; len(metrics) > 0 {
		metricName = metrics[g.rng.Intn(len(metrics))].Name
	}
	if metricName == "" {
		return nil, nil
	}

	eventID := genEventID(g.rng)
	envelope := Envelope{
		CustomerID: triple.customer.CustomerID,
		MetricName: metricName,
		// Quantity defaults to 1 — one event = one unit of the metric.
		// Per-metric scaling (e.g. token counts for LLM products) would
		// require enriching this from the template body; out of MVP scope.
		// Backend enforces @Positive so 1.0 is the safe floor.
		Quantity:       1.0,
		OccurredAt:     eventTime,
		IdempotencyKey: eventID,
		ProductType:    string(productKind),
		// Per-template fields move from the prior `body` wrapper to
		// `metadata`. Backend stores this as JSONB for downstream
		// dimension matching + per-event reporting.
		Metadata: body,
	}

	evt := &Event{
		Envelope:      envelope,
		IngestionPath: g.ingestPicker.Pick(g.rng),
		Archetype:     t.tenant.Archetype,
		PayloadSize:   size,
		Auth: EventAuth{
			Token:    triple.key.Secret,
			ClientID: triple.key.ClientID,
		},
		GeneratedAt: g.now(),
		// In-memory routing fields — propagated to HTTP headers by the
		// driver but not into the serialized body.
		TenantID:       t.tenant.TenantID,
		SubscriptionID: triple.subscription.SubscriptionID,
		EventID:        eventID,
	}

	// Roll for negative-path injection. The planner mutates the event in
	// place and tags evt.NegativePath.
	if kind := g.negPlanner.Pick(g.rng); kind != NPNone {
		if err := g.negPlanner.Apply(evt, kind, g.rng); err != nil {
			// A failed injection is logged via the runner; we drop the event
			// rather than emit a half-injected one.
			return nil, fmt.Errorf("inject %s: %w", kind, err)
		}
	}

	return evt, nil
}

// --- helpers ---

// genEventID returns a 32-hex-char id deterministic per rng state. Even
// though the platform's UsageEventValidator doesn't require uniqueness, a
// stable id helps replay + dedup logic downstream.
func genEventID(rng *rand.Rand) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 32)
	for i := range b {
		b[i] = hex[rng.Intn(16)]
	}
	return string(b)
}

// pickTripleForProduct picks a random eligible customer/sub/key triple on
// the tenant for the given product type. Returns false if the tenant has
// none — callers handle by dropping the event.
func pickTripleForProduct(t indexedTenant, pt scenario.ProductType, rng *rand.Rand) (customerSubKeyTriple, bool) {
	if len(t.activeIdx) == 0 {
		return customerSubKeyTriple{}, false
	}
	// Filter candidates by product type. activeIdx already covers all of the
	// tenant's product types; we walk and collect matches. For tenants with
	// a single product type (the common case) the filter is O(1).
	if len(t.productTypes) == 1 {
		// Fast path: tenant carries exactly one product type.
		for k := range t.productTypes {
			if k != pt {
				return customerSubKeyTriple{}, false
			}
			break
		}
		idx := rng.Intn(len(t.activeIdx))
		return t.activeIdx[idx], true
	}
	candidates := make([]customerSubKeyTriple, 0, len(t.activeIdx))
	for _, c := range t.activeIdx {
		if c.productType == pt {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return customerSubKeyTriple{}, false
	}
	idx := rng.Intn(len(candidates))
	return candidates[idx], true
}

// indexTenants flattens the manifest into the per-tenant "happy path" view.
// ACTIVE / TRIALING / PAUSED / PAST_DUE all generate happy-path traffic
// (their keys aren't revoked); CANCELLED / EXPIRED are skipped here and
// only used by the stale_key injector.
func indexTenants(m *seed.Manifest, _ *scenario.Scenario) ([]indexedTenant, error) {
	if m == nil {
		return nil, errors.New("nil manifest")
	}
	out := make([]indexedTenant, 0, len(m.Tenants))
	for ti := range m.Tenants {
		t := &m.Tenants[ti]
		idx := indexedTenant{
			manifestIdx:  ti,
			tenant:       t,
			productTypes: map[scenario.ProductType][]seed.ManifestMetric{},
		}
		// Map products → product type → metrics (ID+Name pairs).
		// Prefer the new Metrics field; fall back to MetricIDs (with empty
		// names) for manifests written before this drift-fix landed.
		for _, p := range t.Products {
			if len(p.Metrics) > 0 {
				idx.productTypes[p.ProductType] = append(idx.productTypes[p.ProductType], p.Metrics...)
				continue
			}
			for _, id := range p.MetricIDs {
				idx.productTypes[p.ProductType] = append(
					idx.productTypes[p.ProductType],
					seed.ManifestMetric{ID: id},
				)
			}
		}
		// Walk subscriptions; collect happy-path triples.
		productTypes := make([]scenario.ProductType, 0, len(idx.productTypes))
		for k := range idx.productTypes {
			productTypes = append(productTypes, k)
		}
		// Deterministic order so per-event picks reproduce.
		sort.Slice(productTypes, func(i, j int) bool { return string(productTypes[i]) < string(productTypes[j]) })

		for ci := range t.Customers {
			c := &t.Customers[ci]
			for si := range c.Subscriptions {
				sub := &c.Subscriptions[si]
				if sub.Stale {
					continue
				}
				for ki := range sub.APIKeys {
					k := &sub.APIKeys[ki]
					if k.Revoked {
						continue
					}
					// Distribute this triple across the tenant's product
					// types so events sampled by product type still find a
					// triple. Tenants in the manifest don't carry the
					// product-type-per-key edge, so we proxy by rotating
					// through the tenant's list.
					for _, pt := range productTypes {
						idx.activeIdx = append(idx.activeIdx, customerSubKeyTriple{
							customer:     c,
							subscription: sub,
							key:          k,
							productType:  pt,
						})
					}
				}
			}
		}
		if len(idx.activeIdx) == 0 {
			// All subs stale or no keys — skip; this tenant can't drive
			// happy-path traffic. The stale_key injector still uses it
			// via the planner's own walk.
			continue
		}
		out = append(out, idx)
	}
	return out, nil
}

func tenantWeightsFor(n int, dist scenario.Distribution) []float64 {
	switch dist {
	case scenario.DistPareto8020:
		return Pareto80_20Weights(n)
	case scenario.DistZipf:
		return ZipfWeights(n, 1.0)
	default:
		return UniformWeights(n)
	}
}

func ingestionWeightsFor(p scenario.IngestionPaths) map[string]float64 {
	return map[string]float64{
		"rest_direct":      p.RestDirect,
		"sdk_node":         p.SDKNode,
		"sdk_python":       p.SDKPython,
		"sdk_java":         p.SDKJava,
		"sdk_go":           p.SDKGo,
		"gateway_kong":     p.GatewayKong,
		"gateway_apigee":   p.GatewayApigee,
		"gateway_aws":      p.GatewayAWS,
		"gateway_azure":    p.GatewayAzure,
		"gateway_mulesoft": p.GatewayMuleSoft,
		"gateway_apisix":   p.GatewayAPISIX,
		"gateway_tyk":      p.GatewayTyk,
		"gateway_gravitee": p.GatewayGravitee,
		"gateway_envoy":    p.GatewayEnvoy,
		"webhook_receiver": p.WebhookReceiver,
		"csv_upload":       p.CSVUpload,
	}
}

func indexOfNegPath(k NegativePathKind) int {
	for i, n := range AllNegativePaths {
		if n == k {
			return i
		}
	}
	return -1
}
