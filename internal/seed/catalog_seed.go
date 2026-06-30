package seed

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// catalog_seed.go — DEMO catalog-mode orchestration (DEMO-P1, Req #1).
//
// seedCatalogTenant drives the human, real-world Northwind AI dataset
// (demo-seed-catalog.json) into ONE fixed golden tenant. It reuses the exact
// same typed API clients, request structs, idempotent name/email/code lookups,
// and rate-plan request builder as the archetype path — ONLY the names come
// from the catalog. So the golden tenant is real-named by construction and
// passes seed-naming-lint. The golden tenant is what the demo orchestrator
// (DEMO-P2) snapshots + clones per visitor.
//
// What catalog-mode v1 creates here (the API-creatable structural core):
//   products → billable units (template-named, already real) → rate plans →
//   offerings → customers → subscriptions (+ wallet/payment-method per billing
//   mode, + one API key per non-trial subscription).
//
// What it deliberately does NOT create here (produced by build-golden.sh's
// downstream steps because they need the real billing pipeline / dunning, not
// a create call):
//   - invoices + credit notes (come from bill runs over the usage backfill),
//   - the payment-driven lifecycle states PAST_DUE / SUSPENDED / EXPIRING_SOON
//     (transitionSubscription deliberately leaves these ACTIVE — see
//     subscriptions.go: faking them via a status write would be a lie; they
//     come from routing a real failed charge through the dunning scheduler),
//   - CPQ quotes, promotions, storefront config, usage trickle.
// See aforo-nextgen-docker/demo/README.md for the full pipeline.
//
// Honesty: the 6 lifecycle states reachable by an honest API call
// (CREATED / TRIALING / ACTIVE / PAUSED / CANCELLED / EXPIRED) ARE driven here;
// the other 3 are tagged in the manifest with their requested state so the
// payments step can take them from there.

// demoWalletInitialBalanceUSD seeds PREPAID/HYBRID demo wallets with a credible
// starting balance so the customer dashboard shows a non-trivial prepaid budget.
const demoWalletInitialBalanceUSD = 5000.0

// demoCatalogArchetypeTag is the synthetic "archetype" label recorded on the
// golden tenant's manifest + used in seedKey() so the deterministic
// Idempotency-Key shape matches the rest of loadgen.
const demoCatalogArchetypeTag = "demo-golden"

func (s *Seeder) seedCatalogTenant(ctx context.Context, manifest *Manifest) error {
	c := s.cfg.Client
	cat := s.cfg.Catalog
	tenantID := s.cfg.ReuseTenantID
	runID := s.cfg.RunID
	logf := s.cfg.Logger.Printf

	logf("[%s] catalog seed starting: tenant=%s products=%d ratePlans=%d offerings=%d customers=%d subscriptions=%d",
		demoCatalogArchetypeTag, tenantID,
		len(cat.Products), len(cat.RatePlans), len(cat.Offerings), len(cat.Customers), len(cat.Subscriptions))

	// Per-tenant billing prerequisites that org-service's normal provisioning
	// would have set up. The reuse-tenant path skips that, so replicate the
	// missing steps (same as seeder.go's archetype path).
	if _, err := provisionDefaultBillingEntity(ctx, c, tenantID); err != nil {
		return fmt.Errorf("default billing entity: %w", err)
	}
	// ONE Stripe gateway serves the whole tenant regardless of how many
	// billing modes its offerings use, so provision it once for the first
	// offering mode that needs a gateway (POSTPAID/HYBRID). Calling it per
	// mode would re-POST the same per-tenant gateway.
	for _, mode := range uniqueOfferingModes(cat) {
		if archetypeNeedsPaymentGateway(mode) {
			if err := provisionPaymentGatewayIfNeeded(ctx, c, tenantID, mode); err != nil {
				return fmt.Errorf("payment gateway: %w", err)
			}
			break
		}
	}

	mt := ManifestTenant{
		TenantID:   tenantID,
		ExternalID: tenantID,
		Archetype:  demoCatalogArchetypeTag,
	}

	// ── Products + their (template-named, real) billable units ──────────────
	type productEntry struct {
		resp    productResponse
		metrics []ManifestMetric
	}
	products := make(map[string]productEntry, len(cat.Products))
	for _, p := range cat.Products {
		prodSeed := seedKey("product-"+p.Key, demoCatalogArchetypeTag, runID, 1)
		prod, err := provisionCatalogProduct(ctx, c, tenantID, prodSeed, p)
		if err != nil {
			return err
		}
		pt := scenario.ProductType(p.Type)
		metricSeed := seedKey("metrics-"+p.Key, demoCatalogArchetypeTag, runID, 1)
		mets, err := provisionMetricsForProduct(ctx, c, tenantID, prod.ID, pt, metricSeed)
		if err != nil {
			return fmt.Errorf("metrics for product %q: %w", p.Key, err)
		}
		mProd := ManifestProduct{ProductID: prod.ID, Name: prod.Name, SeedKey: prodSeed, ProductType: pt}
		mm := make([]ManifestMetric, 0, len(mets))
		for _, m := range mets {
			mm = append(mm, ManifestMetric{ID: m.ID, Name: m.Name})
			mProd.MetricIDs = append(mProd.MetricIDs, m.ID)
			mProd.Metrics = append(mProd.Metrics, ManifestMetric{ID: m.ID, Name: m.Name})
		}
		products[p.Key] = productEntry{resp: prod, metrics: mm}
		mt.Products = append(mt.Products, mProd)
	}

	// ── Rate plans (one per catalog plan, over its product's metrics) ───────
	ratePlans := make(map[string]ratePlanResponse, len(cat.RatePlans))
	for _, rp := range cat.RatePlans {
		pe, ok := products[rp.Product]
		if !ok {
			// Validate() guarantees this ref resolves; defensive guard.
			return fmt.Errorf("rate plan %q references unknown product %q", rp.Key, rp.Product)
		}
		rpSeed := seedKey("rateplan-"+rp.Key, demoCatalogArchetypeTag, runID, 1)
		plan, err := provisionCatalogRatePlan(ctx, c, tenantID, rpSeed, rp, pe.resp.ID, pe.metrics)
		if err != nil {
			return err
		}
		ratePlans[rp.Key] = plan
		mt.RatePlans = append(mt.RatePlans, ManifestRatePlan{
			RatePlanID: plan.ID,
			Name:       plan.Name,
			SeedKey:    rpSeed,
			Version:    plan.Version,
			Config:     rateConfigSummary(synthArchetypeForCatalogPlan(rp)),
		})
	}

	// ── Offerings (wrap a rate plan in a billing mode) ──────────────────────
	offerings := make(map[string]offeringResponse, len(cat.Offerings))
	offeringMode := make(map[string]scenario.BillingMode, len(cat.Offerings))
	for _, o := range cat.Offerings {
		plan, ok := ratePlans[o.RatePlan]
		if !ok {
			return fmt.Errorf("offering %q references unknown rate plan %q", o.Key, o.RatePlan)
		}
		cur := planCurrencyForOffering(cat, o)
		offSeed := seedKey("offering-"+o.Key, demoCatalogArchetypeTag, runID, 1)
		off, err := provisionCatalogOffering(ctx, c, tenantID, offSeed, o, plan.ID, cur)
		if err != nil {
			return err
		}
		offerings[o.Key] = off
		offeringMode[o.Key] = scenario.BillingMode(o.BillingMode)
		mt.Offerings = append(mt.Offerings, ManifestOffering{
			OfferingID:  off.ID,
			Code:        off.Code,
			SeedKey:     offSeed,
			BillingMode: scenario.BillingMode(o.BillingMode),
			Currency:    cur,
		})
	}

	// ── Customers (real names + real emails) ────────────────────────────────
	custCurrency := defaultCurrency(cat)
	customers := make(map[string]customerResponse, len(cat.Customers))
	custManifest := make(map[string]*ManifestCustomer, len(cat.Customers))
	custSeq := 0
	for _, cu := range cat.Customers {
		custSeq++
		custSeed := seedKey("customer-"+cu.Key, demoCatalogArchetypeTag, runID, custSeq)
		cust, err := provisionCatalogCustomer(ctx, c, tenantID, custSeed, cu, custCurrency)
		if err != nil {
			return err
		}
		customers[cu.Key] = cust
		custManifest[cu.Key] = &ManifestCustomer{
			CustomerID: cust.ID,
			Email:      cust.Email,
			SeedKey:    custSeed,
			Currency:   custCurrency,
		}
	}

	// ── Subscriptions (+ wallet/payment-method per billing mode, + API key) ──
	subSeq := 0
	for _, sub := range cat.Subscriptions {
		subSeq++
		cust, ok := customers[sub.Customer]
		if !ok {
			return fmt.Errorf("subscription %q references unknown customer %q", sub.Key, sub.Customer)
		}
		off, ok := offerings[sub.Offering]
		if !ok {
			return fmt.Errorf("subscription %q references unknown offering %q", sub.Key, sub.Offering)
		}
		mode := offeringMode[sub.Offering]
		target := scenario.SubscriptionState(sub.LifecycleState)

		walletID := ""
		if mode == scenario.BillingPrepaid || mode == scenario.BillingHybrid {
			walSeed := seedKey("wallet-"+sub.Key, demoCatalogArchetypeTag, runID, subSeq)
			wallet, err := provisionWallet(ctx, c, tenantID, walSeed, cust.ID, custCurrency, demoWalletInitialBalanceUSD)
			if err != nil {
				return err
			}
			walletID = wallet.ID
		}
		paymentMethodID := ""
		if mode == scenario.BillingPostpaid || mode == scenario.BillingHybrid {
			pmSeed := seedKey("pm-"+sub.Key, demoCatalogArchetypeTag, runID, subSeq)
			pm, err := provisionPaymentMethod(ctx, c, tenantID, pmSeed, cust.ID, subSeq)
			if err != nil {
				return err
			}
			paymentMethodID = pm.ID
		}

		subSeed := seedKey("sub-"+sub.Key, demoCatalogArchetypeTag, runID, subSeq)
		created, err := provisionCatalogSubscription(ctx, c, tenantID, subSeed, off.ID, cust.ID, sub)
		if err != nil {
			return err
		}

		ms := ManifestSubscription{
			SubscriptionID:  created.ID,
			CustomerID:      cust.ID,
			OfferingID:      off.ID,
			SeedKey:         subSeed,
			Status:          target,
			WalletID:        walletID,
			PaymentMethodID: paymentMethodID,
		}

		// One API key per non-trial subscription (TRIALING subs are rejected by
		// pricing-service's key create — see seeder.go). Created while the sub
		// is ACTIVE; for CANCELLED/EXPIRED the later transition revokes it via
		// the platform's atomic key-revocation cascade.
		if target != scenario.StateTrialing {
			pt := productTypeForOffering(cat, sub.Offering)
			keySeed := seedKey("key-"+sub.Key, demoCatalogArchetypeTag, runID, subSeq)
			key, err := provisionAPIKey(ctx, c, tenantID, keySeed, cust.ID, created.ID, pt)
			if err != nil {
				return err
			}
			ms.APIKeys = append(ms.APIKeys, toManifestAPIKey(key))
		}

		// Drive into the target end-state (honest states only; payment-driven
		// states stay ACTIVE here — see file header + subscriptions.go). For
		// CANCELLED/EXPIRED this fires the /cancel|/expire endpoint that
		// triggers the platform's key-revocation cascade; record the stale
		// marker on the manifest for parity with the archetype path.
		staleSince, err := transitionSubscription(ctx, c, tenantID, created.ID, target)
		if err != nil {
			return err
		}
		if staleSince != nil {
			ms.Stale = true
			ms.StaleSince = staleSince
		}

		if mc := custManifest[sub.Customer]; mc != nil {
			mc.Subscriptions = append(mc.Subscriptions, ms)
		}
	}

	// Append customers (with their subscriptions) in catalog order for a stable manifest.
	for _, cu := range cat.Customers {
		if mc := custManifest[cu.Key]; mc != nil {
			mt.Customers = append(mt.Customers, *mc)
		}
	}

	manifest.AppendTenant(mt)
	logf("[%s] catalog seed complete: tenant=%s", demoCatalogArchetypeTag, tenantID)
	return nil
}

// ── Catalog-named creators (mirror the archetype creators; only the NAME differs) ──

func provisionCatalogProduct(ctx context.Context, c *Client, tenantID, seedKey string, p CatalogProduct) (productResponse, error) {
	pt := scenario.ProductType(p.Type)
	if existing, ok, err := lookupProductByName(ctx, c, tenantID, p.Name, pt); err != nil {
		return productResponse{}, fmt.Errorf("lookup product %q: %w", p.Name, err)
	} else if ok {
		return existing, nil
	}
	desc := p.Description
	if desc == "" {
		desc = fmt.Sprintf("%s (%s)", p.Name, pt)
	}
	body := productCreateRequest{
		Name:        p.Name,
		Description: desc,
		ProductType: string(pt),
		Status:      "ACTIVE",
		// Reuse the archetype path's per-type required metadata; the product
		// key is already a URL-safe slug so API base paths read naturally
		// (e.g. translation-api → /v1/translation-api).
		Metadata: productMetadataFor(pt, p.Key),
	}
	createURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathProducts)
	if err != nil {
		return productResponse{}, err
	}
	var resp productResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{TenantID: tenantID, Idempotency: seedKey}); err != nil {
		if aforo.IsConflict(err) {
			if existing, ok, lerr := lookupProductByName(ctx, c, tenantID, p.Name, pt); lerr == nil && ok {
				return existing, nil
			}
		}
		return productResponse{}, fmt.Errorf("create product %q: %w", p.Name, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-product-" + seedKey
		resp.Name = p.Name
		resp.Type = string(pt)
	}
	return resp, nil
}

func provisionCatalogRatePlan(ctx context.Context, c *Client, tenantID, seedKey string, rp CatalogRatePlan, productID string, metrics []ManifestMetric) (ratePlanResponse, error) {
	if existing, ok, err := lookupRatePlanByName(ctx, c, tenantID, rp.Name); err != nil {
		return ratePlanResponse{}, fmt.Errorf("lookup rate plan %q: %w", rp.Name, err)
	} else if ok {
		return existing, nil
	}
	// Reuse the archetype path's request builder (all 6 pricing-model config
	// branches + tier mapping) by projecting the catalog plan onto a synthetic
	// archetype, then override the synthetic name/description/currency.
	synth := synthArchetypeForCatalogPlan(rp)
	body := buildRatePlanRequest(synth, []string{productID}, metrics)
	body.Name = rp.Name
	if rp.Config.Description != "" {
		body.Description = rp.Config.Description
	} else {
		body.Description = rp.Name
	}
	if rp.Currency != "" {
		body.Currency = rp.Currency
	}
	// buildRatePlanRequest only sets BaseFee for FLAT_RATE. Map an
	// INCLUDED_QUOTA plan's platformFeeUsd onto baseFee so an enterprise
	// included-quota plan surfaces its recurring platform fee instead of $0.
	// Deploy-gated: if pricing-service rejects baseFee on a non-flat plan, the
	// golden build halts loudly at this step and this single line is dropped
	// (see aforo-nextgen-docker/demo/README.md "known assumptions").
	if synth.PricingModel == scenario.PricingIncludedQuota && rp.Config.PlatformFeeUsd > 0 {
		body.BaseFee = rp.Config.PlatformFeeUsd
	}
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathRatePlans)
	if err != nil {
		return ratePlanResponse{}, err
	}
	var resp ratePlanResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{TenantID: tenantID, Idempotency: seedKey}); err != nil {
		if aforo.IsConflict(err) {
			if existing, ok, lerr := lookupRatePlanByName(ctx, c, tenantID, rp.Name); lerr == nil && ok {
				return existing, nil
			}
		}
		return ratePlanResponse{}, fmt.Errorf("create rate plan %q: %w", rp.Name, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-rateplan-" + seedKey
		resp.Name = rp.Name
		resp.Version = 1
	}
	return resp, nil
}

func provisionCatalogOffering(ctx context.Context, c *Client, tenantID, seedKey string, o CatalogOffering, ratePlanID, currency string) (offeringResponse, error) {
	code := o.Key // the catalog key is a real, stable, per-tenant-unique code.
	if existing, ok, err := lookupOfferingByCode(ctx, c, tenantID, code); err != nil {
		return offeringResponse{}, fmt.Errorf("lookup offering %q: %w", code, err)
	} else if ok {
		return existing, nil
	}
	desc := o.Description
	if desc == "" {
		desc = o.Name
	}
	body := offeringCreateRequest{
		Code:              code,
		Name:              o.Name,
		Description:       desc,
		Type:              "STANDALONE",
		Status:            "ACTIVE",
		PrimaryRatePlanID: ratePlanID,
		BillingMode:       o.BillingMode,
		Currency:          currency,
	}
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathOfferings)
	if err != nil {
		return offeringResponse{}, err
	}
	var resp offeringResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{TenantID: tenantID, Idempotency: seedKey}); err != nil {
		if aforo.IsConflict(err) {
			if existing, ok, lerr := lookupOfferingByCode(ctx, c, tenantID, code); lerr == nil && ok {
				return existing, nil
			}
		}
		return offeringResponse{}, fmt.Errorf("create offering %q: %w", o.Name, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-offering-" + seedKey
		resp.Code = code
		resp.Name = o.Name
	}
	return resp, nil
}

// provisionCatalogSubscription creates a subscription with dates RE-BASED from
// the catalog's relative-date fields (startedDaysAgo / trialEndsInDays /
// expiredDaysAgo), so a freshly-built golden tenant always reads "started N days
// ago" relative to the build day rather than a frozen calendar date. Mirrors
// provisionSubscription's create-flag logic (startTrial for TRIALING, past
// endDate for EXPIRED) but threads the catalog's backdated start. The wallet /
// payment-method bindings are implicit server-side (one wallet per customer,
// default payment method), exactly as in the archetype path — they are NOT on
// the wire — so this creator doesn't take them.
func provisionCatalogSubscription(ctx context.Context, c *Client, tenantID, seedKey, offeringID, customerID string, sub CatalogSub) (subscriptionResponse, error) {
	if existing, ok, err := lookupSubscriptionByCustomerAndOffering(ctx, c, tenantID, customerID, offeringID); err != nil {
		return subscriptionResponse{}, fmt.Errorf("lookup sub for customer=%q offering=%q: %w", customerID, offeringID, err)
	} else if ok {
		return existing, nil
	}

	now := time.Now().UTC()
	target := scenario.SubscriptionState(sub.LifecycleState)

	// Default to "started a month ago" so even unspecified subs carry some
	// history for the analytics/revenue surfaces.
	startedDaysAgo := sub.StartedDaysAgo
	if startedDaysAgo <= 0 {
		startedDaysAgo = 30
	}
	body := subscriptionCreateRequest{
		OfferingID:   offeringID,
		CustomerID:   customerID,
		BillingCycle: "MONTHLY",
		StartDate:    now.AddDate(0, 0, -startedDaysAgo).Format("2006-01-02"),
	}
	switch target {
	case scenario.StateTrialing:
		// A trial just started — keep startDate recent + set the trial expiry.
		trialDays := sub.TrialEndsInDays
		if trialDays <= 0 {
			trialDays = 14
		}
		t := now.AddDate(0, 0, trialDays)
		body.StartDate = now.Format("2006-01-02")
		body.StartTrial = true
		body.TrialEndsAt = &t
	case scenario.StateExpired:
		// End date in the past so transitionSubscription's /expire (or the
		// SubscriptionExpiryJob) flips it to EXPIRED.
		expiredAgo := sub.ExpiredDaysAgo
		if expiredAgo <= 0 {
			expiredAgo = 1
		}
		body.EndDate = now.AddDate(0, 0, -expiredAgo).Format("2006-01-02")
	}

	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathSubscriptions)
	if err != nil {
		return subscriptionResponse{}, err
	}
	var resp subscriptionResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{TenantID: tenantID, Idempotency: seedKey}); err != nil {
		if aforo.IsConflict(err) {
			if existing, ok, lerr := lookupSubscriptionByCustomerAndOffering(ctx, c, tenantID, customerID, offeringID); lerr == nil && ok {
				return existing, nil
			}
		}
		return subscriptionResponse{}, fmt.Errorf("create subscription %q: %w", sub.Key, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-sub-" + seedKey
		resp.CustomerID = customerID
		resp.OfferingID = offeringID
		resp.Status = "ACTIVE"
	}
	return resp, nil
}

func provisionCatalogCustomer(ctx context.Context, c *Client, tenantID, seedKey string, cu CatalogCustomer, currency string) (customerResponse, error) {
	if existing, ok, err := lookupCustomerByEmail(ctx, c, tenantID, cu.Email); err != nil {
		return customerResponse{}, fmt.Errorf("lookup customer %q: %w", cu.Email, err)
	} else if ok {
		return existing, nil
	}
	body := customerCreateRequest{
		Name:            cu.Name,
		Email:           cu.Email,
		Plan:            catalogCustomerPlan(cu.CompanySize),
		DefaultCurrency: currency,
	}
	createURL, err := c.Target().Path(aforo.ServiceCustomer, aforo.PathCustomers)
	if err != nil {
		return customerResponse{}, err
	}
	var resp customerResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{TenantID: tenantID, Idempotency: seedKey}); err != nil {
		if aforo.IsConflict(err) {
			if existing, ok, lerr := lookupCustomerByEmail(ctx, c, tenantID, cu.Email); lerr == nil && ok {
				return existing, nil
			}
		}
		return customerResponse{}, fmt.Errorf("create customer %q: %w", cu.Name, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-customer-" + seedKey
		resp.Name = cu.Name
		resp.Email = cu.Email
	}
	return resp, nil
}

// ── Catalog-mode helpers ──────────────────────────────────────────────────────

// synthArchetypeForCatalogPlan projects a catalog rate plan onto the
// scenario.TenantArchetype shape buildRatePlanRequest + rateConfigSummary
// consume. Only PricingModel + RateConfig drive billing config; Name is used
// for the (overridden) request name + description only.
func synthArchetypeForCatalogPlan(rp CatalogRatePlan) scenario.TenantArchetype {
	return scenario.TenantArchetype{
		Name:         rp.Key,
		PricingModel: scenario.PricingModel(rp.PricingModel),
		RateConfig:   rp.ToRateConfig(),
	}
}

// catalogCustomerPlan maps the catalog's free-text companySize onto
// customer-service's Plan enum (STANDARD | BUSINESS | ENTERPRISE). Unknown /
// empty defaults to STANDARD.
func catalogCustomerPlan(size string) string {
	switch strings.ToUpper(strings.TrimSpace(size)) {
	case "ENTERPRISE", "LARGE":
		return "ENTERPRISE"
	case "MID", "MID-MARKET", "MIDMARKET", "MEDIUM", "SMB", "GROWTH":
		return "BUSINESS"
	default:
		return "STANDARD"
	}
}

// uniqueOfferingModes returns the distinct billing modes across all offerings,
// so payment-gateway setup runs once per mode the demo actually uses.
func uniqueOfferingModes(cat *DemoCatalog) []scenario.BillingMode {
	seen := map[string]bool{}
	var out []scenario.BillingMode
	for _, o := range cat.Offerings {
		if seen[o.BillingMode] {
			continue
		}
		seen[o.BillingMode] = true
		out = append(out, scenario.BillingMode(o.BillingMode))
	}
	return out
}

// defaultCurrency returns the catalog's company currency, defaulting to USD.
func defaultCurrency(cat *DemoCatalog) string {
	if cur := strings.TrimSpace(cat.Company.Currency); cur != "" {
		return cur
	}
	return "USD"
}

// planCurrencyForOffering returns the currency of the rate plan an offering
// wraps (the offering inherits its plan's currency), falling back to the
// company default.
func planCurrencyForOffering(cat *DemoCatalog, o CatalogOffering) string {
	for _, rp := range cat.RatePlans {
		if rp.Key == o.RatePlan && strings.TrimSpace(rp.Currency) != "" {
			return rp.Currency
		}
	}
	return defaultCurrency(cat)
}

// productTypeForOffering resolves the product type behind an offering
// (offering → rate plan → product → type), used to pick the right API-key
// credential type. Defaults to API if the chain can't be resolved.
func productTypeForOffering(cat *DemoCatalog, offeringKey string) scenario.ProductType {
	var ratePlanKey string
	for _, o := range cat.Offerings {
		if o.Key == offeringKey {
			ratePlanKey = o.RatePlan
			break
		}
	}
	var productKey string
	for _, rp := range cat.RatePlans {
		if rp.Key == ratePlanKey {
			productKey = rp.Product
			break
		}
	}
	for _, p := range cat.Products {
		if p.Key == productKey {
			return scenario.ProductType(p.Type)
		}
	}
	return scenario.ProductAPI
}
