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
	"strings"
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

	// --reuse-tenant-id pre-flight hint (2026-07-22): log-only lookup so the
	// operator sees an early signal if the id is a typo, without gating the
	// run. Fail-open by design — downstream provisionDefaultBillingEntity is
	// the source of truth (a real 404 there stops the worker cleanly). See
	// verifyReusedTenantExists's doc for the rationale.
	if s.cfg.ReuseTenantID != "" && !s.cfg.Client.DryRun() {
		s.verifyReusedTenantExists(ctx)
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

// verifyReusedTenantExists probes the --reuse-tenant-id target ONCE up-front
// so the operator gets an early, actionable hint if the id is a typo,
// instead of watching N goroutines all surface the same root cause via
// their own downstream 404s.
//
// FAIL-OPEN by design: this is a hint, not a gate. A transient lookup
// error (network / permission) or an empty response (backend cache miss,
// fake test server, RLS quirk on freshly-created tenants) does NOT kill
// the run. The downstream provisionDefaultBillingEntity call remains the
// source of truth — if the tenant genuinely doesn't exist, its 404
// produces a definitive error and stops the worker cleanly. The function
// therefore has no error return: every outcome is log-only.
//
// Uses lookupTenantByExternalID (GET /api/v1/internal/tenants?externalId=)
// which is the same endpoint provisionTenant would have called.
func (s *Seeder) verifyReusedTenantExists(ctx context.Context) {
	tenant, ok, err := lookupTenantByExternalID(ctx, s.cfg.Client, s.cfg.ReuseTenantID)
	if err != nil {
		s.cfg.Logger.Printf("[reuse-tenant] pre-flight lookup for %q errored (%v) — continuing; downstream calls will surface a definitive error if the tenant is missing.", s.cfg.ReuseTenantID, err)
		return
	}
	if !ok {
		s.cfg.Logger.Printf("[reuse-tenant] pre-flight lookup for %q returned no match. "+
			"If a definitive 404 follows on the next call, verify the id matches "+
			"the workspace slug in the UI URL (e.g. /home/{account}).",
			s.cfg.ReuseTenantID)
		return
	}
	s.cfg.Logger.Printf("[reuse-tenant] pre-flight lookup ok: tenant %q resolved (name=%q)", s.cfg.ReuseTenantID, tenant.Name)
}

func (s *Seeder) seedOneTenant(ctx context.Context, manifest *Manifest, a scenario.TenantArchetype, seq int, rng *mathrand.Rand) error {
	c := s.cfg.Client
	// Tenant is the ONE entity where externalId is a real backend column
	// (LoadgenTenantResponse on /internal/admin). It is ALSO the identifier
	// the UI onboarding + workspace-slug validators see, which caps at ~41
	// chars in practice. Use the dedicated shortened generator here rather
	// than the general seedKey (which is fine for header-only usage but too
	// long for a slug — see tenantExternalID's doc).
	tenantExtID := tenantExternalID(a.Name, s.cfg.RunID, seq)

	var tenant tenantResponse
	if s.cfg.ReuseTenantID != "" {
		// --reuse-tenant-id path (2026-06-11): skip provisioning and use the
		// supplied tenant ID directly. Downstream provisioners (products,
		// metrics, customers, subscriptions) still create fresh entities
		// scoped to this tenant. Pre-flight lookup is performed by
		// verifyReusedTenantExists at the top of Run() rather than here,
		// so the operator gets a clear error before any worker starts.
		tenant = tenantResponse{
			ID:         s.cfg.ReuseTenantID,
			ExternalID: s.cfg.ReuseTenantID,
			Name:       fmt.Sprintf("Reused Tenant [%s]", s.cfg.ReuseTenantID),
		}
		tenantExtID = s.cfg.ReuseTenantID
		s.cfg.Logger.Printf("[%s/%03d] reusing existing tenant id=%s (skipping provisionTenant)", a.Name, seq, tenant.ID)
	} else {
		var err error
		tenant, err = provisionTenant(ctx, c, tenantExtID, fmt.Sprintf("Loadgen Tenant [%s] %03d", a.Name, seq), a.Name)
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
		ExternalID:   tenantExtID,
		Archetype:    a.Name,
		PricingModel: a.PricingModel,
		BillingMode:  a.BillingMode,
	}

	// N products per product type in the archetype (per Issue 2). Default
	// ProductsPerType is 1 (applyDefaults backfills), so archetypes that
	// don't set the field keep the historical single-product-per-type
	// behavior. When set to N > 1, we create N distinct products of every
	// listed product type (e.g. [API,AGENTIC_API] × 4 = 8 products).
	productsPerType := a.ProductsPerType
	if productsPerType <= 0 {
		productsPerType = 1
	}
	totalProducts := len(a.ProductTypes) * productsPerType
	productIDs := make([]string, 0, totalProducts)

	// Rate-plan metric dedup: two paths produce duplicate metricName entries
	// in the rate plan otherwise, both of which pricing-service rejects with
	// 400 "Duplicate metricName ..." (RatePlanServiceImpl.validate line 1017).
	//   1. ProductsPerType > 1: catalog dedupes metrics by (tenant, name), so
	//      product #2 of the same type returns the SAME metric IDs as product
	//      #1 — appending them twice trips the rate plan check.
	//   2. Metric-name overlap across descriptors: "Data Transfer" appears in
	//      the API + WEBSOCKET_API + GRAPHQL_API + GRPC_API descriptors, and
	//      other names may overlap in the future. Deduping by name is the
	//      safe, forward-compatible fix.
	// Case-insensitive dedup mirrors pricing-service's own predicate
	// (`mc.getMetricName().trim().toLowerCase()`).
	rateMetrics := make([]ManifestMetric, 0, 4*productsPerType)
	seenMetricNames := make(map[string]struct{}, 4*productsPerType)
	for _, pt := range a.ProductTypes {
		for prodIdx := 1; prodIdx <= productsPerType; prodIdx++ {
			// Product seed key + name distinguish products of the same type
			// within a tenant. When productsPerType==1 we keep the historical
			// name shape ("Loadgen Product {archetype} {type}") for backward
			// compatibility with existing manifests + lookup-by-name tests;
			// when >1 we append an ordinal ("... 01", "... 02", ...).
			var prodSeedKey, prodName string
			if productsPerType == 1 {
				prodSeedKey = seedKey(fmt.Sprintf("product-%s", pt), a.Name, s.cfg.RunID, seq)
				prodName = fmt.Sprintf("Loadgen Product %s %s", a.Name, pt)
			} else {
				prodSeedKey = seedKey(fmt.Sprintf("product-%s-%02d", pt, prodIdx), a.Name, s.cfg.RunID, seq)
				prodName = fmt.Sprintf("Loadgen Product %s %s %02d", a.Name, pt, prodIdx)
			}
			prod, err := provisionProductNamed(ctx, c, tenant.ID, prodSeedKey, prodName, a.Name, pt)
			if err != nil {
				return err
			}
			productIDs = append(productIDs, prod.ID)

			// Metrics are tenant-scoped (NOT product-scoped) on the catalog side,
			// so bulk-seeding for productsPerType > 1 returns the SAME metric
			// IDs on every iteration. Still call it every iteration to keep the
			// idempotency chain deterministic and to record per-product metric
			// arrays on the manifest, but rely on the rate-plan dedup below.
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
				mm := ManifestMetric{
					ID:              m.ID,
					Name:            m.Name,
					EventField:      m.EventField,
					AggregationType: m.AggregationType,
				}
				// Per-product manifest keeps the full metric list (safe;
				// duplicates here are informational, not a rate-plan input).
				mProd.MetricIDs = append(mProd.MetricIDs, m.ID)
				mProd.Metrics = append(mProd.Metrics, mm)
				// Rate-plan aggregate dedups by name (case-insensitive) —
				// see block comment on seenMetricNames above.
				nameKey := strings.ToLower(strings.TrimSpace(m.Name))
				if nameKey == "" {
					continue
				}
				if _, dup := seenMetricNames[nameKey]; dup {
					continue
				}
				seenMetricNames[nameKey] = struct{}{}
				rateMetrics = append(rateMetrics, mm)
			}
			mt.Products = append(mt.Products, mProd)
		}
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
// Do NOT reuse seedKey as a tenant externalId — the boilerplate onboarding
// slug schema (apps/web/app/onboarding/_lib/schema/onboarding.schema.ts)
// caps the workspace slug at 50 chars AND the LoadgenInternalTenantController
// derives a storefront subdomain that shrinks the practical budget to ~41
// chars (50 - 8-hex CRC32 - 1 dash). A generated seedKey like
// "loadgen-tenant-complete-fresh-seed-2026-07-21-949610-001" is 56 chars —
// well past both limits. Use tenantExternalID for the tenant identifier
// instead; seedKey stays for Idempotency-Key headers (no wire length limit
// in practice).
func seedKey(kind, archetype, runID string, seq int) string {
	return fmt.Sprintf("loadgen-%s-%s-%s-%03d", kind, archetype, runID, seq)
}

// tenantExternalID builds the tenant's external identifier / slug.
//
// This is the ONE loadgen identifier that travels to the UI: it is written
// as `tenant_id` on tenant_configs, becomes the workspace slug the operator
// sees, and drives the storefront subdomain that
// LoadgenInternalTenantController derives (see its `slug()` helper — the
// helper truncates at 50 chars minus a CRC32 suffix, leaving ~41 chars of
// real budget).
//
// The boilerplate onboarding form (apps/web/app/onboarding/_lib/schema/
// onboarding.schema.ts) rejects any workspace slug longer than 50 chars via
// Zod max(50), which manifests to the operator as "We couldn't create your
// workspace". So this helper must keep the identifier well under 50 chars
// even for long archetype names, while staying deterministic per
// (archetype, runID, seq) so re-runs within the 24h idempotency window
// match the prior identifier.
//
// Shape: "lg-{archetype-trunc}-{6-hex}-{seq:03d}" — target ≤ 32 chars.
//   - "lg-" (3) instead of "loadgen-tenant-" (15) — same identifier
//     purpose, matches the LoadgenInternalTenantController's own "loadgen-"
//     prefix convention.
//   - archetype truncated to first 8 chars — enough for grep debugging
//     without eating the length budget.
//   - 6-hex hash of the full (archetype, runID, seq) tuple — deterministic
//     across re-runs on the same run id, distinguishes archetype slots.
//   - {seq:03d} — human-readable slot ordinal.
//
// Regex compliance: the boilerplate schema requires
// ^[a-z0-9]+(?:-[a-z0-9]+)*$ (lowercase alphanumerics separated by dashes).
// runIDs contain dashes but the hash strips them; archetype names are
// user-controlled but we lowercase and strip non-alphanumerics to be safe.
func tenantExternalID(archetype, runID string, seq int) string {
	arch := strings.ToLower(archetype)
	// Strip anything outside [a-z0-9] to preserve the boilerplate slug regex.
	// Consecutive stripped chars collapse to nothing, then re-cap at 8.
	var b strings.Builder
	b.Grow(len(arch))
	for i := 0; i < len(arch); i++ {
		c := arch[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	archClean := b.String()
	if archClean == "" {
		archClean = "arch"
	}
	if len(archClean) > 8 {
		archClean = archClean[:8]
	}
	// The 6-hex hash disambiguates across concurrent runs / archetypes
	// that happen to share an arch-prefix truncation, but does NOT need to
	// be collision-free on its own — the trailing seq:03d suffix is the
	// primary uniqueness guarantee within one run. Two tenants of the same
	// archetype in the same run always have different seqs, so the full
	// identifier `lg-{arch}-{hash}-{seq}` is unique even if two hash values
	// happened to match. Across DIFFERENT archetypes, the hash input includes
	// the archetype name → different inputs → different hashes generally.
	// (Naive birthday-collision math on 24 bits alone would be ~3% at 1000
	// tenants, but the seq suffix removes that concern.)
	h := fnvHash(fmt.Sprintf("%s|%s|%d", archetype, runID, seq)) & 0xFFFFFF
	return fmt.Sprintf("lg-%s-%06x-%03d", archClean, h, seq)
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
