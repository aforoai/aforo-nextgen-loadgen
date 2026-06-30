package seed

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// catalog.go — the demo "golden dataset" naming catalog (DEMO-P1).
//
// Why this exists: the archetype-driven seed path names everything
// synthetically ("Loadgen Product …", "Loadgen Customer … 001"). The public
// demo (strategy §6.6 / Req #1) requires every visitor-facing record to carry a
// human, real-world name. Catalog-mode loads demo-seed-catalog.json (the single
// source of truth, lives in aforo-nextgen-docker/demo/) and creates EXACTLY the
// named Northwind AI entities through the same typed API clients + idempotent
// name-lookups the archetype path uses — so the golden tenant is real-named by
// construction and passes seed-naming-lint.
//
// This file is the MODEL + LOADER + VALIDATOR only. The create orchestration
// lives in catalog_seed.go. Both are additive: nothing here changes the
// existing archetype seed path, so the CI scenarios are unaffected.

// DemoCatalog mirrors demo-seed-catalog.json. Only the structural-core sections
// (company, billableUnits, products, ratePlans, offerings, customers,
// subscriptions) are created by catalog-mode v1; the remaining sections
// (invoices, creditNotes, cpqQuotes, promotions, storefront, usageTrickle) are
// modeled for validation + future build phases (invoices come from bill runs
// over the usage backfill, not direct creation — see build-golden.sh).
type DemoCatalog struct {
	Version       string             `json:"version"`
	Description   string             `json:"description,omitempty"`
	Story         string             `json:"story,omitempty"`
	Company       CatalogCompany     `json:"company"`
	BillableUnits []CatalogUnit      `json:"billableUnits"`
	Products      []CatalogProduct   `json:"products"`
	RatePlans     []CatalogRatePlan  `json:"ratePlans"`
	Offerings     []CatalogOffering  `json:"offerings"`
	Customers     []CatalogCustomer  `json:"customers"`
	Subscriptions []CatalogSub       `json:"subscriptions"`
	Invoices      []CatalogInvoice   `json:"invoices,omitempty"`
	CreditNotes   []CatalogCreditNote `json:"creditNotes,omitempty"`
	CPQQuotes     []CatalogQuote     `json:"cpqQuotes,omitempty"`
	Promotions    []CatalogPromotion `json:"promotions,omitempty"`
	Storefront    *CatalogStorefront `json:"storefront,omitempty"`
	UsageTrickle  *CatalogUsageTrickle `json:"usageTrickle,omitempty"`
}

type CatalogCompany struct {
	Name         string `json:"name"`
	LegalName    string `json:"legalName,omitempty"`
	Tagline      string `json:"tagline,omitempty"`
	SupportEmail string `json:"supportEmail,omitempty"`
	WebsiteURL   string `json:"websiteUrl,omitempty"`
	AddressLine1 string `json:"addressLine1,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	PostalCode   string `json:"postalCode,omitempty"`
	Country      string `json:"country,omitempty"`
	Currency     string `json:"currency,omitempty"`
}

type CatalogUnit struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Aggregation string `json:"aggregation"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
}

type CatalogProduct struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Description   string   `json:"description,omitempty"`
	BillableUnits []string `json:"billableUnits"`
}

// CatalogRateConfig holds the pricing-model-specific knobs. Not every field
// applies to every model; Validate enforces the minimum per model.
type CatalogRateConfig struct {
	FlatFeeUsd        float64           `json:"flatFeeUsd,omitempty"`
	PerUnitRateUsd    float64           `json:"perUnitRateUsd,omitempty"`
	IncludedFreeUnits int64             `json:"includedFreeUnits,omitempty"`
	PlatformFeeUsd    float64           `json:"platformFeeUsd,omitempty"`
	OverageRateUsd    float64           `json:"overageRateUsd,omitempty"`
	PercentageRate    float64           `json:"percentageRate,omitempty"`
	MinFeeUsd         float64           `json:"minFeeUsd,omitempty"`
	BlockSizeUnits    int64             `json:"blockSizeUnits,omitempty"`
	BillingTiming     string            `json:"billingTiming,omitempty"`
	Description       string            `json:"description,omitempty"`
	Tiers             []CatalogRateTier `json:"tiers,omitempty"`
}

// CatalogRateTier is one band of a GRADUATED / VOLUME_TIERED rate. TierEnd is
// the exclusive upper bound; null/nil means "and above" (open-ended top tier).
type CatalogRateTier struct {
	TierStart    int64    `json:"tierStart"`
	TierEnd      *int64   `json:"tierEnd"`
	UnitPriceUsd float64  `json:"unitPriceUsd"`
	FlatFeeUsd   *float64 `json:"flatFeeUsd,omitempty"`
}

type CatalogRatePlan struct {
	Key          string            `json:"key"`
	Name         string            `json:"name"`
	PricingModel string            `json:"pricingModel"`
	Product      string            `json:"product"`     // ref → products[].key
	PrimaryUnit  string            `json:"primaryUnit"` // ref → billableUnits[].key
	Currency     string            `json:"currency"`
	Config       CatalogRateConfig `json:"config"`
}

type CatalogOffering struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	BillingMode string `json:"billingMode"`
	RatePlan    string `json:"ratePlan"` // ref → ratePlans[].key
	Visibility  string `json:"visibility,omitempty"`
	Description string `json:"description,omitempty"`
}

type CatalogCustomer struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Industry    string `json:"industry,omitempty"`
	Email       string `json:"email"`
	Status      string `json:"status,omitempty"`
	CompanySize string `json:"companySize,omitempty"`
	Country     string `json:"country,omitempty"`
}

// CatalogSub describes a subscription named "Customer — Plan (State)", with
// RELATIVE dates (re-based to "now" at build time). Exactly the lifecycle-state
// knob the run engine / transition step needs to drive it to its target state.
type CatalogSub struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	Customer       string `json:"customer"` // ref → customers[].key
	Offering       string `json:"offering"` // ref → offerings[].key
	PlanLabel      string `json:"planLabel,omitempty"`
	LifecycleState string `json:"lifecycleState"`
	StartedDaysAgo int    `json:"startedDaysAgo,omitempty"`
	RenewsInDays   int    `json:"renewsInDays,omitempty"`
	TrialEndsInDays int   `json:"trialEndsInDays,omitempty"`
	ExpiresInDays  int    `json:"expiresInDays,omitempty"`
	PausedDaysAgo  int    `json:"pausedDaysAgo,omitempty"`
	CancelledDaysAgo int  `json:"cancelledDaysAgo,omitempty"`
	SuspendedDaysAgo int  `json:"suspendedDaysAgo,omitempty"`
	ExpiredDaysAgo int    `json:"expiredDaysAgo,omitempty"`
	DunningAttempt int    `json:"dunningAttempt,omitempty"`
}

type CatalogInvoice struct {
	Number           string  `json:"number"`
	Customer         string  `json:"customer"`
	Subscription     string  `json:"subscription"`
	Status           string  `json:"status"`
	AmountUsd        float64 `json:"amountUsd"`
	IssuedDaysAgo    int     `json:"issuedDaysAgo,omitempty"`
	DueDaysFromIssue int     `json:"dueDaysFromIssue,omitempty"`
	PaidDaysAgo      int     `json:"paidDaysAgo,omitempty"`
}

type CatalogCreditNote struct {
	Number         string  `json:"number"`
	Customer       string  `json:"customer"`
	AgainstInvoice string  `json:"againstInvoice"`
	Status         string  `json:"status"`
	AmountUsd      float64 `json:"amountUsd"`
	IssuedDaysAgo  int     `json:"issuedDaysAgo,omitempty"`
	Reason         string  `json:"reason,omitempty"`
}

type CatalogQuote struct {
	Key              string  `json:"key"`
	Name             string  `json:"name"`
	Customer         string  `json:"customer"`
	Offering         string  `json:"offering"`
	Status           string  `json:"status,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	TotalAcvUsd      float64 `json:"totalAcvUsd,omitempty"`
	DiscountPct      float64 `json:"discountPct,omitempty"`
	PaymentTerms     string  `json:"paymentTerms,omitempty"`
	BillingFrequency string  `json:"billingFrequency,omitempty"`
	TermLengthMonths int     `json:"termLengthMonths,omitempty"`
	ValidForDays     int     `json:"validForDays,omitempty"`
	CreatedDaysAgo   int     `json:"createdDaysAgo,omitempty"`
}

type CatalogPromotion struct {
	Code           string  `json:"code"`
	Name           string  `json:"name,omitempty"`
	Type           string  `json:"type"`
	Value          float64 `json:"value"`
	Currency       string  `json:"currency,omitempty"`
	MaxRedemptions int     `json:"maxRedemptions,omitempty"`
	ValidForDays   int     `json:"validForDays,omitempty"`
	Status         string  `json:"status,omitempty"`
}

type CatalogStorefront struct {
	Name             string   `json:"name"`
	Slug             string   `json:"slug,omitempty"`
	BrandColor       string   `json:"brandColor,omitempty"`
	AccentColor      string   `json:"accentColor,omitempty"`
	HeroTitle        string   `json:"heroTitle,omitempty"`
	HeroSubtitle     string   `json:"heroSubtitle,omitempty"`
	LogoText         string   `json:"logoText,omitempty"`
	SupportURL       string   `json:"supportUrl,omitempty"`
	PublicOfferings  []string `json:"publicOfferings,omitempty"`
}

type CatalogUsageTrickle struct {
	Description           string             `json:"description,omitempty"`
	HistoricalBackfillDays int               `json:"historicalBackfillDays,omitempty"`
	RatePerMinute         int                `json:"ratePerMinute,omitempty"`
	ActiveSubscriptions   []string           `json:"activeSubscriptions,omitempty"`
	Metrics               []CatalogTrickleMetric `json:"metrics,omitempty"`
}

type CatalogTrickleMetric struct {
	Unit                 string `json:"unit"`
	PerEventQuantityRange []int64 `json:"perEventQuantityRange,omitempty"`
}

// ── Loading ──────────────────────────────────────────────────────────────────

// LoadCatalog reads + parses + validates a demo-seed-catalog.json file. A
// non-nil error means the catalog is unsafe to seed from (missing refs, bad
// enums, synthetic names) — callers MUST NOT proceed.
func LoadCatalog(path string) (*DemoCatalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %q: %w", path, err)
	}
	return ParseCatalog(raw)
}

// ParseCatalog parses + validates catalog bytes (split out for testing).
func ParseCatalog(raw []byte) (*DemoCatalog, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // catch catalog typos early — a misspelled field is a silent data gap.
	var cat DemoCatalog
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if errs := cat.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid catalog (%d problem(s)):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return &cat, nil
}

// ── Validation ────────────────────────────────────────────────────────────────

var (
	validProductTypes  = toSet(scenario.AllProductTypes)
	validPricingModels = toSet(scenario.AllPricingModels)
	validBillingModes  = toSet(scenario.AllBillingModes)
	validSubStates     = toSet(scenario.AllSubscriptionStates)
	billingTimings     = map[string]bool{"": true, "IN_ADVANCE": true, "IN_ARREARS": true}
	// syntheticName mirrors seed-naming-lint.mjs — a defense-in-depth gate so a
	// synthetic display name can never reach the seeder even if the JS lint is skipped.
	syntheticName = regexp.MustCompile(`(?i)^(test|metric|cust|prod|sub|offer|plan|inv|item)[_\d]|^loadgen\b`)
)

func toSet[T ~string](xs []T) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[string(x)] = true
	}
	return m
}

// Validate checks referential integrity (every ref resolves), enum validity,
// required-field presence, per-pricing-model rate config, and that no
// visitor-facing display name is synthetic. Returns a list of human-readable
// problems (empty == valid) so the caller can surface them all at once.
func (c *DemoCatalog) Validate() []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if strings.TrimSpace(c.Company.Name) == "" {
		add("company.name is required")
	}

	unitKeys := map[string]bool{}
	for i, u := range c.BillableUnits {
		if u.Key == "" || u.Name == "" {
			add("billableUnits[%d]: key and name are required", i)
			continue
		}
		if unitKeys[u.Key] {
			add("billableUnits[%d]: duplicate key %q", i, u.Key)
		}
		unitKeys[u.Key] = true
		checkName(add, fmt.Sprintf("billableUnits[%d].name", i), u.Name)
	}

	productKeys := map[string]bool{}
	for i, p := range c.Products {
		if p.Key == "" || p.Name == "" {
			add("products[%d]: key and name are required", i)
			continue
		}
		if productKeys[p.Key] {
			add("products[%d]: duplicate key %q", i, p.Key)
		}
		productKeys[p.Key] = true
		checkName(add, fmt.Sprintf("products[%d].name", i), p.Name)
		if !validProductTypes[p.Type] {
			add("products[%d] %q: invalid type %q (want API|AI_AGENT|MCP_SERVER|AGENTIC_API)", i, p.Key, p.Type)
		}
		if len(p.BillableUnits) == 0 {
			add("products[%d] %q: at least one billableUnit ref required", i, p.Key)
		}
		for _, uref := range p.BillableUnits {
			if !unitKeys[uref] {
				add("products[%d] %q: billableUnit ref %q not found", i, p.Key, uref)
			}
		}
	}

	planKeys := map[string]bool{}
	for i, rp := range c.RatePlans {
		if rp.Key == "" || rp.Name == "" {
			add("ratePlans[%d]: key and name are required", i)
			continue
		}
		if planKeys[rp.Key] {
			add("ratePlans[%d]: duplicate key %q", i, rp.Key)
		}
		planKeys[rp.Key] = true
		checkName(add, fmt.Sprintf("ratePlans[%d].name", i), rp.Name)
		if !validPricingModels[rp.PricingModel] {
			add("ratePlans[%d] %q: invalid pricingModel %q", i, rp.Key, rp.PricingModel)
		}
		if !productKeys[rp.Product] {
			add("ratePlans[%d] %q: product ref %q not found", i, rp.Key, rp.Product)
		}
		if rp.PrimaryUnit != "" && !unitKeys[rp.PrimaryUnit] {
			add("ratePlans[%d] %q: primaryUnit ref %q not found", i, rp.Key, rp.PrimaryUnit)
		}
		if !billingTimings[rp.Config.BillingTiming] {
			add("ratePlans[%d] %q: invalid billingTiming %q (want IN_ADVANCE|IN_ARREARS)", i, rp.Key, rp.Config.BillingTiming)
		}
		validateRateConfig(add, fmt.Sprintf("ratePlans[%d] %q", i, rp.Key), rp.PricingModel, rp.Config)
	}

	offeringKeys := map[string]bool{}
	for i, o := range c.Offerings {
		if o.Key == "" || o.Name == "" {
			add("offerings[%d]: key and name are required", i)
			continue
		}
		if offeringKeys[o.Key] {
			add("offerings[%d]: duplicate key %q", i, o.Key)
		}
		offeringKeys[o.Key] = true
		checkName(add, fmt.Sprintf("offerings[%d].name", i), o.Name)
		if !validBillingModes[o.BillingMode] {
			add("offerings[%d] %q: invalid billingMode %q (want POSTPAID|PREPAID|HYBRID)", i, o.Key, o.BillingMode)
		}
		if !planKeys[o.RatePlan] {
			add("offerings[%d] %q: ratePlan ref %q not found", i, o.Key, o.RatePlan)
		}
	}

	customerKeys := map[string]bool{}
	for i, cu := range c.Customers {
		if cu.Key == "" || cu.Name == "" || cu.Email == "" {
			add("customers[%d]: key, name and email are required", i)
			continue
		}
		if customerKeys[cu.Key] {
			add("customers[%d]: duplicate key %q", i, cu.Key)
		}
		customerKeys[cu.Key] = true
		checkName(add, fmt.Sprintf("customers[%d].name", i), cu.Name)
	}

	subKeys := map[string]bool{}
	for i, s := range c.Subscriptions {
		if s.Key == "" || s.Name == "" {
			add("subscriptions[%d]: key and name are required", i)
			continue
		}
		if subKeys[s.Key] {
			add("subscriptions[%d]: duplicate key %q", i, s.Key)
		}
		subKeys[s.Key] = true
		checkName(add, fmt.Sprintf("subscriptions[%d].name", i), s.Name)
		if !customerKeys[s.Customer] {
			add("subscriptions[%d] %q: customer ref %q not found", i, s.Key, s.Customer)
		}
		if !offeringKeys[s.Offering] {
			add("subscriptions[%d] %q: offering ref %q not found", i, s.Key, s.Offering)
		}
		if !validSubStates[s.LifecycleState] {
			add("subscriptions[%d] %q: invalid lifecycleState %q", i, s.Key, s.LifecycleState)
		}
	}

	// Cross-section refs on the non-core sections (validated even though
	// catalog-mode v1 doesn't create them — keeps the catalog internally honest).
	for i, inv := range c.Invoices {
		if !customerKeys[inv.Customer] {
			add("invoices[%d] %q: customer ref %q not found", i, inv.Number, inv.Customer)
		}
		if inv.Subscription != "" && !subKeys[inv.Subscription] {
			add("invoices[%d] %q: subscription ref %q not found", i, inv.Number, inv.Subscription)
		}
	}
	invoiceNums := map[string]bool{}
	for _, inv := range c.Invoices {
		invoiceNums[inv.Number] = true
	}
	for i, cn := range c.CreditNotes {
		if !customerKeys[cn.Customer] {
			add("creditNotes[%d] %q: customer ref %q not found", i, cn.Number, cn.Customer)
		}
		if cn.AgainstInvoice != "" && !invoiceNums[cn.AgainstInvoice] {
			add("creditNotes[%d] %q: againstInvoice ref %q not found", i, cn.Number, cn.AgainstInvoice)
		}
	}
	for i, q := range c.CPQQuotes {
		if !customerKeys[q.Customer] {
			add("cpqQuotes[%d] %q: customer ref %q not found", i, q.Name, q.Customer)
		}
		if !offeringKeys[q.Offering] {
			add("cpqQuotes[%d] %q: offering ref %q not found", i, q.Name, q.Offering)
		}
	}
	if c.Storefront != nil {
		for _, oref := range c.Storefront.PublicOfferings {
			if !offeringKeys[oref] {
				add("storefront.publicOfferings: offering ref %q not found", oref)
			}
		}
	}
	if c.UsageTrickle != nil {
		for _, sref := range c.UsageTrickle.ActiveSubscriptions {
			if !subKeys[sref] {
				add("usageTrickle.activeSubscriptions: subscription ref %q not found", sref)
			}
		}
		for _, m := range c.UsageTrickle.Metrics {
			if !unitKeys[m.Unit] {
				add("usageTrickle.metrics: unit ref %q not found", m.Unit)
			}
		}
	}

	return errs
}

func checkName(add func(string, ...any), path, name string) {
	if syntheticName.MatchString(strings.TrimSpace(name)) {
		add("%s = %q is a synthetic name — demo records must use real-world names (Req #1)", path, name)
	}
}

// validateRateConfig enforces the minimum config each pricing model needs to
// produce a non-trivial bill (so a demo plan can't render as $0 by omission).
func validateRateConfig(add func(string, ...any), where, model string, cfg CatalogRateConfig) {
	switch model {
	case string(scenario.PricingFlatRate):
		if cfg.FlatFeeUsd <= 0 {
			add("%s: FLAT_RATE requires config.flatFeeUsd > 0", where)
		}
	case string(scenario.PricingPerUnit):
		if cfg.PerUnitRateUsd <= 0 {
			add("%s: PER_UNIT requires config.perUnitRateUsd > 0", where)
		}
	case string(scenario.PricingPercentage):
		if cfg.PercentageRate <= 0 {
			add("%s: PERCENTAGE requires config.percentageRate > 0", where)
		}
	case string(scenario.PricingIncludedQuota):
		if cfg.IncludedFreeUnits <= 0 && cfg.OverageRateUsd <= 0 && cfg.PlatformFeeUsd <= 0 {
			add("%s: INCLUDED_QUOTA requires platformFeeUsd, includedFreeUnits or overageRateUsd", where)
		}
	case string(scenario.PricingGraduated):
		validateTiers(add, where+" (GRADUATED)", cfg.Tiers)
	case string(scenario.PricingVolumeTiered):
		validateTiers(add, where+" (VOLUME_TIERED)", cfg.Tiers)
	}
}

func validateTiers(add func(string, ...any), where string, tiers []CatalogRateTier) {
	if len(tiers) == 0 {
		add("%s: requires config.tiers", where)
		return
	}
	prevStart := int64(-1)
	for i, t := range tiers {
		if t.TierStart < 0 {
			add("%s: tiers[%d].tierStart must be >= 0", where, i)
		}
		if t.TierStart <= prevStart {
			add("%s: tiers[%d].tierStart must increase monotonically", where, i)
		}
		prevStart = t.TierStart
		if t.TierEnd != nil && *t.TierEnd <= t.TierStart {
			add("%s: tiers[%d].tierEnd must be > tierStart (or null for open-ended top tier)", where, i)
		}
		if t.UnitPriceUsd < 0 {
			add("%s: tiers[%d].unitPriceUsd must be >= 0", where, i)
		}
	}
	if tiers[len(tiers)-1].TierEnd != nil {
		add("%s: the top tier must be open-ended (tierEnd: null)", where)
	}
}

// ── Mapping helpers (catalog → scenario structs the existing creators speak) ──

// TierBands maps catalog tiers to scenario.TierBand. The scenario shape uses an
// inclusive-cumulative UpToUnits upper bound; the open-ended top tier is encoded
// as UpToUnits = 0 (the convention the pricing creators already expect for
// "and above"). build-time only; pure transform, unit-tested.
func (cfg CatalogRateConfig) TierBands() []scenario.TierBand {
	out := make([]scenario.TierBand, 0, len(cfg.Tiers))
	for _, t := range cfg.Tiers {
		band := scenario.TierBand{UnitPriceUSD: t.UnitPriceUsd}
		if t.TierEnd != nil {
			band.UpToUnits = *t.TierEnd
		} // else 0 == open-ended top tier
		if t.FlatFeeUsd != nil {
			band.FlatFeeUSD = *t.FlatFeeUsd
		}
		out = append(out, band)
	}
	return out
}

// ToRateConfig projects a catalog rate plan's config onto the scenario.RateConfig
// the existing provisionRatePlan path consumes, so catalog-mode reuses the exact
// same rate-plan request builder as the archetype path.
func (rp CatalogRatePlan) ToRateConfig() scenario.RateConfig {
	c := rp.Config
	return scenario.RateConfig{
		FlatFeeUSD: c.FlatFeeUsd,
		// buildRatePlanRequest reads PerUnitRateUSD as the per-unit rate for
		// PER_UNIT AND as the overage rate for INCLUDED_QUOTA. The catalog keeps
		// those as two distinct fields (perUnitRateUsd vs overageRateUsd) for
		// human clarity, so fall back to overageRateUsd when perUnitRateUsd is
		// absent — this is what makes an INCLUDED_QUOTA plan's overage actually
		// bill instead of resolving to a $0 rate.
		PerUnitRateUSD:    firstNonZero(c.PerUnitRateUsd, c.OverageRateUsd),
		PercentageRate:    c.PercentageRate,
		MinFeeUSD:         c.MinFeeUsd,
		IncludedFreeUnits: c.IncludedFreeUnits,
		BlockSizeUnits:    c.BlockSizeUnits,
		GraduatedTiers:    pick(rp.PricingModel == string(scenario.PricingGraduated), c.TierBands()),
		VolumeTiers:       pick(rp.PricingModel == string(scenario.PricingVolumeTiered), c.TierBands()),
	}
}

func pick(when bool, v []scenario.TierBand) []scenario.TierBand {
	if when {
		return v
	}
	return nil
}

// firstNonZero returns a if non-zero, else b. Used to let the catalog express
// PER_UNIT's perUnitRateUsd and INCLUDED_QUOTA's overageRateUsd as separate,
// human-readable fields while still feeding the single PerUnitRateUSD slot the
// shared rate-plan request builder consumes.
func firstNonZero(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}
