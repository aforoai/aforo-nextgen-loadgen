package seed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// SeederConfig wires the high-level run into the Client + scenario + manifest.
type SeederConfig struct {
	Client         *Client
	Scenario       *scenario.Scenario
	OnlyArchetypes []string
	RunID          string // pre-computed run id; if empty, a fresh one is generated
	ManifestPath   string // optional — if set, manifest is saved here on Run()
	Concurrency    int    // archetype-level worker count; 0 → 4
	Logger         *log.Logger
	Now            func() time.Time

	// ReuseTenantID, when non-empty, causes the seeder to SKIP tenant
	// provisioning and use this tenant ID directly for every archetype slot.
	// Products / metrics / customers / subscriptions are still created
	// fresh per run under this tenant. Intended for CI smoke runs that
	// need the seeded entities to be visible to an operator UI session
	// already authenticated as a specific tenant (e.g. `aforo_dev`).
	//
	// Constraint: only valid when the scenario allocates exactly one
	// tenant slot (count: 1) — otherwise multiple archetype workers would
	// race on the same tenant ID. The CLI enforces this; bare API callers
	// MUST verify allocation themselves.
	//
	// Filed 2026-06-11 to close the "loadgen records invisible in
	// frontend" symptom: loadgen would mint a synthetic tenant id per run
	// (`loadgen-tenant-<archetype>-seed-<date>-<hash>-001`), but the
	// Aforo Product UI is scoped to whatever tenant the operator's
	// VITE_TENANT_ID points at (default `aforo_dev`). Records landed
	// correctly but were filtered out at the tenant boundary.
	ReuseTenantID string

	// Catalog, when non-nil, switches the seeder into DEMO catalog-mode
	// (DEMO-P1, strategy §6.6 / Req #1): instead of the archetype generator's
	// synthetic "Loadgen …" names, the seeder creates EXACTLY the human,
	// real-world Northwind AI entities described by demo-seed-catalog.json,
	// through the same typed API clients + idempotent lookups. This produces
	// the golden tenant that the demo orchestrator (DEMO-P2) clones per
	// visitor.
	//
	// Constraint: catalog-mode targets ONE fixed tenant, so ReuseTenantID MUST
	// be set (NewSeeder enforces this). The Scenario is still required (the
	// Client + name + manifest plumbing reuse it) but its archetype allocation
	// is ignored in catalog-mode.
	Catalog *DemoCatalog
}

// Seeder is the per-run orchestrator. Exposed as a struct (rather than a free
// function) so callers can inject a custom Logger/Clock and call Run() with
// a request-scoped context.
type Seeder struct {
	cfg SeederConfig
}

// NewSeeder validates the inputs and returns a Seeder.
func NewSeeder(cfg SeederConfig) (*Seeder, error) {
	if cfg.Client == nil {
		return nil, errors.New("seeder: Client is nil")
	}
	if cfg.Scenario == nil {
		return nil, errors.New("seeder: Scenario is nil")
	}
	if cfg.Catalog != nil && cfg.ReuseTenantID == "" {
		return nil, errors.New("seeder: catalog-mode requires ReuseTenantID (the golden demo tenant id) — catalog-mode targets one fixed tenant")
	}
	if cfg.RunID == "" {
		cfg.RunID = newRunID(cfg.Now)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Seeder{cfg: cfg}, nil
}

// RunResult is the seed run's summary, returned to the CLI for printing.
type RunResult struct {
	Manifest    *Manifest
	Allocations []ArchetypeAllocation
	Duration    time.Duration
	Errors      []error
}

// Run executes the full seed: archetype allocation → per-tenant provisioning
// → manifest write. Concurrency is bounded by Client's semaphore (HTTP
// concurrency) and by SeederConfig.Concurrency (archetype-level workers).
//
// The manifest is finalized + saved even on partial failure — best-effort
// recording so a re-run with --clean can still tear down what landed.
func (s *Seeder) Run(ctx context.Context) (*RunResult, error) {
	start := s.cfg.Now()
	manifest := NewManifest(s.cfg.RunID, s.cfg.Client.Target().String(), s.cfg.Scenario.Name, start)

	// DEMO catalog-mode: one fixed golden tenant, driven entirely by the
	// real-named catalog. Bypasses the archetype allocator + goroutine fan-out
	// (there is exactly one tenant) and returns early with its own manifest.
	if s.cfg.Catalog != nil {
		res := &RunResult{Manifest: manifest}
		if err := s.seedCatalogTenant(ctx, manifest); err != nil {
			res.Errors = append(res.Errors, err)
		}
		manifest.Finalize()
		res.Duration = s.cfg.Now().Sub(start)
		if s.cfg.ManifestPath != "" {
			if _, err := manifest.Save(s.cfg.ManifestPath); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("save manifest: %w", err))
			}
		}
		return res, nil
	}

	allocs := AllocateTenants(s.cfg.Scenario)
	if len(s.cfg.OnlyArchetypes) > 0 {
		allocs = FilterArchetypes(allocs, s.cfg.OnlyArchetypes)
	}

	errCh := make(chan error, len(allocs)*4)
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.cfg.Concurrency)

	for _, alloc := range allocs {
		if alloc.Count <= 0 {
			continue
		}
		alloc := alloc
		for tenantSeq := 0; tenantSeq < alloc.Count; tenantSeq++ {
			tenantSeq := tenantSeq
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if ctx.Err() != nil {
					errCh <- ctx.Err()
					return
				}
				rng := rngForTenant(s.cfg.Scenario.Seed, alloc.Archetype.Name, tenantSeq)
				if err := s.seedOneTenant(ctx, manifest, alloc.Archetype, tenantSeq+1, rng); err != nil {
					errCh <- fmt.Errorf("[%s/%03d] %w", alloc.Archetype.Name, tenantSeq+1, err)
				}
			}()
		}
	}
	wg.Wait()
	close(errCh)

	manifest.Finalize()

	res := &RunResult{
		Manifest:    manifest,
		Allocations: allocs,
		Duration:    s.cfg.Now().Sub(start),
	}
	for err := range errCh {
		res.Errors = append(res.Errors, err)
	}

	if s.cfg.ManifestPath != "" {
		if _, err := manifest.Save(s.cfg.ManifestPath); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("save manifest: %w", err))
		}
	}
	return res, nil
}

func (s *Seeder) seedOneTenant(ctx context.Context, manifest *Manifest, a scenario.TenantArchetype, seq int, rng *mathrand.Rand) error {
	c := s.cfg.Client
	// Tenant is the ONE entity where externalId is a real backend column
	// (LoadgenTenantResponse on /internal/admin). Other entities use
	// seedKey only as the Idempotency-Key header value. See CONVENTIONS.md.
	tenantExternalID := seedKey("tenant", a.Name, s.cfg.RunID, seq)

	var tenant tenantResponse
	if s.cfg.ReuseTenantID != "" {
		// --reuse-tenant-id path (2026-06-11): skip provisioning and use the
		// supplied tenant ID directly. Downstream provisioners (products,
		// metrics, customers, subscriptions) still create fresh entities
		// scoped to this tenant. The first downstream call surfaces a clear
		// 404 if the tenant does not actually exist at the backend; we
		// deliberately do NOT pre-validate here to avoid a dependency on
		// the LoadgenInternalTenantController lookup-by-id endpoint.
		tenant = tenantResponse{
			ID:         s.cfg.ReuseTenantID,
			ExternalID: s.cfg.ReuseTenantID,
			Name:       fmt.Sprintf("Reused Tenant [%s]", s.cfg.ReuseTenantID),
		}
		tenantExternalID = s.cfg.ReuseTenantID
		s.cfg.Logger.Printf("[%s/%03d] reusing existing tenant id=%s (skipping provisionTenant)", a.Name, seq, tenant.ID)
	} else {
		var err error
		tenant, err = provisionTenant(ctx, c, tenantExternalID, fmt.Sprintf("Loadgen Tenant [%s] %03d", a.Name, seq), a.Name)
		if err != nil {
			return err
		}
	}

	// Per-tenant billing prerequisites that organization-service's normal
	// TenantProvisioningService.provision() would have set up. Loadgen
	// uses LoadgenInternalTenantController directly which skips that
	// flow, so we replicate the missing steps:
	//   - default billing entity (always — many downstream billing ops need it)
	//   - primary payment gateway (only when archetype's billing mode requires it)
	// See internal/seed/billing_setup.go for rationale + env-var contract.
	if _, err := provisionDefaultBillingEntity(ctx, c, tenant.ID); err != nil {
		return err
	}
	if err := provisionPaymentGatewayIfNeeded(ctx, c, tenant.ID, a.BillingMode); err != nil {
		return err
	}

	mt := ManifestTenant{
		TenantID:     tenant.ID,
		ExternalID:   tenantExternalID,
		Archetype:    a.Name,
		PricingModel: a.PricingModel,
		BillingMode:  a.BillingMode,
	}

	// One product per product type in the archetype.
	productIDs := make([]string, 0, len(a.ProductTypes))
	// Carry BOTH id + name: the rate plan needs the name persisted (pricing V57)
	// so billing's metricName-fallback works for seeded usage.
	rateMetrics := make([]ManifestMetric, 0, 4)
	for _, pt := range a.ProductTypes {
		prodSeedKey := seedKey(fmt.Sprintf("product-%s", pt), a.Name, s.cfg.RunID, seq)
		prod, err := provisionProduct(ctx, c, tenant.ID, prodSeedKey, a.Name, pt)
		if err != nil {
			return err
		}
		productIDs = append(productIDs, prod.ID)

		metricSeedKey := seedKey(fmt.Sprintf("metrics-%s", pt), a.Name, s.cfg.RunID, seq)
		metrics, err := provisionMetricsForProduct(ctx, c, tenant.ID, prod.ID, pt, metricSeedKey)
		if err != nil {
			return err
		}
		mProd := ManifestProduct{
			ProductID:   prod.ID,
			Name:        prod.Name,
			SeedKey:     prodSeedKey,
			ProductType: pt,
		}
		for _, m := range metrics {
			rateMetrics = append(rateMetrics, ManifestMetric{ID: m.ID, Name: m.Name})
			mProd.MetricIDs = append(mProd.MetricIDs, m.ID)
			mProd.Metrics = append(mProd.Metrics, ManifestMetric{ID: m.ID, Name: m.Name})
		}
		mt.Products = append(mt.Products, mProd)
	}

	// One rate plan covering all products + their metrics.
	rpSeedKey := seedKey("rateplan", a.Name, s.cfg.RunID, seq)
	rp, err := provisionRatePlan(ctx, c, tenant.ID, a, productIDs, rateMetrics, rpSeedKey)
	if err != nil {
		return err
	}
	mt.RatePlans = append(mt.RatePlans, ManifestRatePlan{
		RatePlanID: rp.ID,
		Name:       rp.Name,
		SeedKey:    rpSeedKey,
		Version:    rp.Version,
		Config:     rateConfigSummary(a),
	})

	plan := planArchetype(a, rng)

	// Group customers by currency so we share one offering per currency.
	currencies := uniqueCurrencies(plan.Customers)
	offByCurrency := make(map[string]offeringResponse, len(currencies))
	for _, cur := range currencies {
		offSeedKey := seedKey(fmt.Sprintf("offering-%s", cur), a.Name, s.cfg.RunID, seq)
		off, err := provisionOffering(ctx, c, tenant.ID, offSeedKey, a, rp.ID, cur)
		if err != nil {
			return err
		}
		offByCurrency[cur] = off
		mt.Offerings = append(mt.Offerings, ManifestOffering{
			OfferingID:  off.ID,
			Code:        off.Code,
			SeedKey:     offSeedKey,
			BillingMode: a.BillingMode,
			Currency:    cur,
		})
	}

	// One customer + subscription chain per plan slot.
	for _, cp := range plan.Customers {
		custSeedKey := seedKey("customer", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
		cust, err := provisionCustomer(ctx, c, tenant.ID, custSeedKey, cp.Currency, a.Name, cp.Seq)
		if err != nil {
			return err
		}
		mc := ManifestCustomer{
			CustomerID: cust.ID,
			Email:      cust.Email,
			SeedKey:    custSeedKey,
			Currency:   cp.Currency,
			Discount:   cp.Discount,
		}

		walletID := ""
		if a.BillingMode == scenario.BillingPrepaid || a.BillingMode == scenario.BillingHybrid {
			walSeedKey := seedKey("wallet", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			wallet, err := provisionWallet(ctx, c, tenant.ID, walSeedKey, cust.ID, cp.Currency, a.RateConfig.WalletInitialBalanceUSD)
			if err != nil {
				return err
			}
			walletID = wallet.ID
		}

		paymentMethodID := ""
		if a.BillingMode == scenario.BillingPostpaid || a.BillingMode == scenario.BillingHybrid {
			pmSeedKey := seedKey("pm", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			pm, err := provisionPaymentMethod(ctx, c, tenant.ID, pmSeedKey, cust.ID, cp.Seq)
			if err != nil {
				return err
			}
			paymentMethodID = pm.ID
		}

		off := offByCurrency[cp.Currency]
		subSeedKey := seedKey("sub", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
		sub, _, err := provisionSubscription(ctx, c, tenant.ID, subSeedKey, off.ID, cust.ID, walletID, paymentMethodID, cp.State, a.Name)
		if err != nil {
			return err
		}

		ms := ManifestSubscription{
			SubscriptionID:         sub.ID,
			CustomerID:             cust.ID,
			OfferingID:             off.ID,
			SeedKey:                subSeedKey,
			Status:                 cp.State,
			WalletID:               walletID,
			PaymentMethodID:        paymentMethodID,
			ExpectedBillingFormula: expectedBillingFormula(a),
		}

		// Apply discount, if any.
		if cp.Discount != nil {
			discSeedKey := seedKey("discount", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			if err := applyDiscount(ctx, c, tenant.ID, discSeedKey, sub.ID, cp.Discount); err != nil {
				return err
			}
		}

		// One API key per subscription. Credential type is product-type aware.
		// The first product type on the archetype drives the credential type
		// (a multi-product archetype like AI_AGENT+API has at least one of each).
		// customerId is required by pricing-service's CreateApiKeyRequest and
		// must match the subscription's customer.
		//
		// pricing-service ApiKeyServiceImpl.createKey rejects non-ACTIVE
		// subscriptions ("Subscription is not active (status: TRIALING)").
		// Loadgen creates the sub with startTrial=true for StateTrialing
		// targets (see subscriptions.go), so the sub is TRIALING at this
		// point — calling provisionAPIKey would 400. Skip key creation
		// for TRIALING; the manifest still records the subscription, just
		// without a bound key. Real-world trial customers also typically
		// don't get keys until they convert, so this matches platform
		// semantics. transitionSubscription is a no-op for TRIALING so
		// nothing else downstream depends on a key being present.
		if cp.State != scenario.StateTrialing {
			pt := a.ProductTypes[0]
			keySeedKey := seedKey("key", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			key, err := provisionAPIKey(ctx, c, tenant.ID, keySeedKey, cust.ID, sub.ID, pt)
			if err != nil {
				return err
			}
			ms.APIKeys = append(ms.APIKeys, toManifestAPIKey(key))
		}

		// Drive the subscription into its target state. CANCELLED/EXPIRED
		// trigger the platform's atomic key-revocation cascade.
		staleSince, err := transitionSubscription(ctx, c, tenant.ID, sub.ID, cp.State)
		if err != nil {
			return err
		}

		if cp.State == scenario.StateCancelled || cp.State == scenario.StateExpired {
			cancelTime := s.cfg.Now()
			if staleSince != nil {
				cancelTime = *staleSince
			}
			if verifyErr := verifyStaleKeys(ctx, c, tenant.ID, &ms, cancelTime); verifyErr != nil {
				return verifyErr
			}
		}

		mc.Subscriptions = append(mc.Subscriptions, ms)
		mt.Customers = append(mt.Customers, mc)
	}

	manifest.AppendTenant(mt)
	return nil
}

func uniqueCurrencies(plans []customerPlan) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, p := range plans {
		if _, ok := seen[p.Currency]; ok {
			continue
		}
		seen[p.Currency] = struct{}{}
		out = append(out, p.Currency)
	}
	return out
}

// seedKey is the deterministic Idempotency-Key string loadgen sends as the
// HTTP `Idempotency-Key` header on every entity create. Shape:
// "loadgen-{kind}-{archetype}-{run}-{seq:03d}". Two callers with the same
// (kind, archetype, runID, seq) produce the same key, so backend's
// IdempotencyResponseService caches the response and a retry returns the
// same body without creating a duplicate.
//
// CONVENTION (see CONVENTIONS.md "Identity model"): seedKey is a LOADGEN-
// INTERNAL concept. It is NOT a backend column on any entity except
// tenants. It is NEVER sent in a request body — only as the
// Idempotency-Key header. Cross-day deterministic identity for lookups
// uses each entity's real backend column (products→name, customers→email,
// offerings→code, etc.); the seedKey only matters within the backend's
// idempotency-cache TTL (24h default).
//
// The "external_id prefix" naming convention remains useful for `--clean`
// scans against the LoadgenTenant /internal/admin endpoint (the one place
// backend genuinely stores externalId), and for grep-debugging in
// loadgen-side logs.
func seedKey(kind, archetype, runID string, seq int) string {
	return fmt.Sprintf("loadgen-%s-%s-%s-%03d", kind, archetype, runID, seq)
}

// rngForTenant returns a deterministic *mathrand.Rand seeded from the
// (scenario seed, archetype name, tenant seq) tuple. This way the same seed
// produces the same per-customer state distribution every run.
func rngForTenant(scenarioSeed int64, archetypeName string, tenantSeq int) *mathrand.Rand {
	hash := fnvHash(archetypeName)
	combined := scenarioSeed ^ int64(hash) ^ int64(tenantSeq)<<32
	return mathrand.New(mathrand.NewSource(combined))
}

func fnvHash(s string) uint32 {
	const prime32 = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

// newRunID generates a stable, sortable run identifier:
// "seed-YYYY-MM-DD-{6 hex chars}". Format chosen so the run id is sortable
// in the file listing alongside other manifest files.
func newRunID(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	t := now().UTC()
	suffix := randomHex(6)
	return fmt.Sprintf("seed-%04d-%02d-%02d-%s", t.Year(), int(t.Month()), t.Day(), suffix)
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// Falls back to math/rand on unlikely entropy failure — the run id
		// is for human readability, not security.
		fallback, _ := rand.Int(rand.Reader, big.NewInt(1<<31-1))
		return fmt.Sprintf("%x", fallback)[:n]
	}
	return hex.EncodeToString(b)[:n]
}
