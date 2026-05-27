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
	tenantExternalID := externalID("tenant", a.Name, s.cfg.RunID, seq)
	tenant, err := provisionTenant(ctx, c, tenantExternalID, fmt.Sprintf("Loadgen Tenant [%s] %03d", a.Name, seq), a.Name)
	if err != nil {
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
	metricIDs := make([]string, 0, 4)
	for _, pt := range a.ProductTypes {
		prodExt := externalID(fmt.Sprintf("product-%s", pt), a.Name, s.cfg.RunID, seq)
		prod, err := provisionProduct(ctx, c, tenant.ID, prodExt, a.Name, pt)
		if err != nil {
			return err
		}
		productIDs = append(productIDs, prod.ID)

		metricExt := externalID(fmt.Sprintf("metrics-%s", pt), a.Name, s.cfg.RunID, seq)
		metrics, err := provisionMetricsForProduct(ctx, c, tenant.ID, prod.ID, pt, metricExt)
		if err != nil {
			return err
		}
		mProd := ManifestProduct{
			ProductID:   prod.ID,
			ExternalID:  prodExt,
			ProductType: pt,
		}
		for _, m := range metrics {
			metricIDs = append(metricIDs, m.ID)
			mProd.MetricIDs = append(mProd.MetricIDs, m.ID)
		}
		mt.Products = append(mt.Products, mProd)
	}

	// One rate plan covering all products + their metrics.
	rpExt := externalID("rateplan", a.Name, s.cfg.RunID, seq)
	rp, err := provisionRatePlan(ctx, c, tenant.ID, a, productIDs, metricIDs, rpExt)
	if err != nil {
		return err
	}
	mt.RatePlans = append(mt.RatePlans, ManifestRatePlan{
		RatePlanID: rp.ID,
		ExternalID: rpExt,
		Version:    rp.Version,
		Config:     rateConfigSummary(a),
	})

	plan := planArchetype(a, rng)

	// Group customers by currency so we share one offering per currency.
	currencies := uniqueCurrencies(plan.Customers)
	offByCurrency := make(map[string]offeringResponse, len(currencies))
	for _, cur := range currencies {
		offExt := externalID(fmt.Sprintf("offering-%s", cur), a.Name, s.cfg.RunID, seq)
		off, err := provisionOffering(ctx, c, tenant.ID, offExt, a, rp.ID, cur)
		if err != nil {
			return err
		}
		offByCurrency[cur] = off
		mt.Offerings = append(mt.Offerings, ManifestOffering{
			OfferingID:  off.ID,
			ExternalID:  offExt,
			BillingMode: a.BillingMode,
			Currency:    cur,
		})
	}

	// One customer + subscription chain per plan slot.
	for _, cp := range plan.Customers {
		custExt := externalID("customer", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
		cust, err := provisionCustomer(ctx, c, tenant.ID, custExt, cp.Currency, a.Name, cp.Seq)
		if err != nil {
			return err
		}
		mc := ManifestCustomer{
			CustomerID: cust.ID,
			ExternalID: custExt,
			Currency:   cp.Currency,
			Discount:   cp.Discount,
		}

		walletID := ""
		if a.BillingMode == scenario.BillingPrepaid || a.BillingMode == scenario.BillingHybrid {
			walExt := externalID("wallet", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			wallet, err := provisionWallet(ctx, c, tenant.ID, walExt, cust.ID, cp.Currency, a.RateConfig.WalletInitialBalanceUSD)
			if err != nil {
				return err
			}
			walletID = wallet.ID
		}

		paymentMethodID := ""
		if a.BillingMode == scenario.BillingPostpaid || a.BillingMode == scenario.BillingHybrid {
			pmExt := externalID("pm", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			pm, err := provisionPaymentMethod(ctx, c, tenant.ID, pmExt, cust.ID, cp.Seq)
			if err != nil {
				return err
			}
			paymentMethodID = pm.ID
		}

		off := offByCurrency[cp.Currency]
		subExt := externalID("sub", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
		sub, _, err := provisionSubscription(ctx, c, tenant.ID, subExt, off.ID, cust.ID, walletID, paymentMethodID, cp.State, a.Name)
		if err != nil {
			return err
		}

		ms := ManifestSubscription{
			SubscriptionID:         sub.ID,
			ExternalID:             subExt,
			Status:                 cp.State,
			WalletID:               walletID,
			PaymentMethodID:        paymentMethodID,
			ExpectedBillingFormula: expectedBillingFormula(a),
		}

		// Apply discount, if any.
		if cp.Discount != nil {
			discExt := externalID("discount", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
			if err := applyDiscount(ctx, c, tenant.ID, discExt, sub.ID, cp.Discount); err != nil {
				return err
			}
		}

		// One API key per subscription. Credential type is product-type aware.
		// The first product type on the archetype drives the credential type
		// (a multi-product archetype like AI_AGENT+API has at least one of each).
		// customerId is required by pricing-service's CreateApiKeyRequest and
		// must match the subscription's customer.
		pt := a.ProductTypes[0]
		keyExt := externalID("key", a.Name, s.cfg.RunID, seq*1_000+cp.Seq)
		key, err := provisionAPIKey(ctx, c, tenant.ID, keyExt, cust.ID, sub.ID, pt)
		if err != nil {
			return err
		}
		ms.APIKeys = append(ms.APIKeys, toManifestAPIKey(key))

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

// externalID is the deterministic naming pattern: "loadgen-{kind}-{archetype}-{run}-{seq:03d}".
// All entity types follow this so a manual `--clean` against a stale manifest
// can also be reconstructed by listing-then-archiving by external_id prefix.
func externalID(kind, archetype, runID string, seq int) string {
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
