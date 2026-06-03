package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// metricConfigRequest mirrors pricing-service's MetricConfigRequest — one
// row per (metric, pricing_model) inside a rate plan. The platform's V3 rate
// plan API accepts any combination of metrics, each with its own model.
//
// Field-name + allowed-value contract (verified against pricing-service
// CreateRatePlanRequest.MetricConfigRequest):
//   - JSON field is "model" (NOT "pricingModel"). The Java field name is
//     "model" with default "PER_UNIT"; sending "pricingModel" is silently
//     ignored and every metric is then billed as PER_UNIT, breaking every
//     non-PER_UNIT archetype.
//   - billingTiming allowed values: "ARREARS" | "ADVANCE". The legacy value
//     "IN_ARREARS" is rejected.
//   - JSON field is "ovBehavior" (NOT "overageBehavior") with allowed values
//     "CHARGE" | "BLOCK" | "IGNORE". The legacy value "ALLOW" is rejected.
type metricConfigRequest struct {
	MetricID string                `json:"metricId"`
	Model    scenario.PricingModel `json:"model"`
	// Rate is a pointer so the JSON encoder distinguishes "absent" from
	// "intentionally zero". Pricing-service enforces `@NotNull` on the
	// metric-config rate field for every pricing model, including
	// FLAT_RATE where loadgen sets a placeholder 0 (the actual flat
	// charge lives on ratePlanCreateRequest.BaseFee, but the metric
	// config still has to carry a numeric rate). The previous form
	// `float64 \`json:"rate,omitempty"\`` silently dropped the field
	// for FLAT_RATE archetypes, and staging rejected with 400
	// "Rate is required for pricing model 'FLAT_RATE' on metric X".
	// Drift-fix 2026-06-01.
	Rate          *float64          `json:"rate,omitempty"`
	IncludedFree  int64             `json:"includedFree,omitempty"`
	BlockSize     int64             `json:"blockSize,omitempty"`
	MinFee        float64           `json:"minFee,omitempty"`
	BillingTiming string            `json:"billingTiming,omitempty"`
	Tiers         []rateTierRequest `json:"tiers,omitempty"`
	OvBehavior    string            `json:"ovBehavior,omitempty"`
}

type rateTierRequest struct {
	TierStart int64   `json:"tierStart"`
	TierEnd   int64   `json:"tierEnd"`
	UnitPrice float64 `json:"unitPrice"`
	FlatFee   float64 `json:"flatFee,omitempty"`
	SortOrder int     `json:"sortOrder"`
}

// ratePlanCreateRequest is the V3 rate plan create body. productIds and
// metricConfigs replace the v1 productId/metricId scalars.
//
// Top-level pricingModel is @NotBlank on the server — omitting it returns
// 400 "Pricing model is required". The per-metric `model` is independent;
// the top-level field is the rate plan's primary pricing model and drives
// the wizard UI in aforo-product (no functional billing role today since
// each metric carries its own model).
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateRatePlanRequest.java column.
// Deterministic identity for cross-day lookup is `name` (queried via
// /rate-plans/search?name= by lookupRatePlanByName). Idempotency-Key is
// the loadgen-internal seedKey set by provisionRatePlan.
type ratePlanCreateRequest struct {
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	PricingModel  string                `json:"pricingModel"`
	Currency      string                `json:"currency"`
	BaseFee       float64               `json:"baseFee,omitempty"`
	ProductIDs    []string              `json:"productIds"`
	MetricConfigs []metricConfigRequest `json:"metricConfigs"`
}

// ratePlanResponse mirrors the subset of pricing-service's RatePlanResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response no longer reads `externalId` —
// pricing-service has no such field on the entity or DTO (verified against
// aforo-nextgen-pricing-service/.../RatePlanResponse.java). `name` is the
// deterministic identity key (loadgen sets `Loadgen Rate Plan <archetype>`
// per tenant) and drives lookupRatePlanByName via the controller's
// `/rate-plans/search?name=` filter.
type ratePlanResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// provisionRatePlan creates the rate plan for an archetype's tenant. The
// rate plan covers ALL of the archetype's products (M:N) and ALL of the
// product-type metrics created by provisionMetricsForProduct.
//
// Configuration logic per pricing model:
//
//	FLAT_RATE         baseFee = rate_config.flat_fee_usd
//	PER_UNIT          metricConfig.rate = rate_config.per_unit_rate_usd
//	                  (+ includedFree if rate_config.included_free_units > 0)
//	PERCENTAGE        metricConfig.rate = rate_config.percentage_rate
//	                  (+ minFee if rate_config.min_fee_usd > 0)
//	INCLUDED_QUOTA    metricConfig.includedFree = rate_config.included_free_units
//	                  metricConfig.rate = rate_config.per_unit_rate_usd
//	                  (+ blockSize if rate_config.block_size_units > 0)
//	GRADUATED         metricConfig.tiers = rate_config.graduated_tiers
//	VOLUME_TIERED     metricConfig.tiers = rate_config.volume_tiers
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST.
//   - Cross-day / DB-reset: lookupRatePlanByName runs first and uses
//     pricing-service's /rate-plans/search?name= server-side filter
//     (verified RatePlanController.search accepts ?name=) + client-side
//     exact-match by name.
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. See CONVENTIONS.md.
func provisionRatePlan(ctx context.Context, c *Client, tenantID string, a scenario.TenantArchetype, productIDs []string, metricIDs []string, seedKey string) (ratePlanResponse, error) {
	name := fmt.Sprintf("Loadgen Rate Plan %s", a.Name)

	if existing, ok, err := lookupRatePlanByName(ctx, c, tenantID, name); err != nil {
		return ratePlanResponse{}, fmt.Errorf("lookup rate plan %q: %w", name, err)
	} else if ok {
		return existing, nil
	}

	body := buildRatePlanRequest(a, productIDs, metricIDs)
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathRatePlans)
	if err != nil {
		return ratePlanResponse{}, err
	}
	var resp ratePlanResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupRatePlanByName(ctx, c, tenantID, name)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return ratePlanResponse{}, fmt.Errorf("create rate plan (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-rateplan-" + seedKey
		resp.Name = name
		resp.Version = 1
	}
	return resp, nil
}

// buildRatePlanRequest is exported via test_helpers for golden-file tests
// (returning the same struct lets tests assert exact field values without
// also exercising the HTTP transport).
//
// Drift-fix (rename pass): the externalID parameter was removed when the
// phantom `json:"externalId,omitempty"` field was dropped from
// ratePlanCreateRequest. Tests that previously asserted on req.ExternalID
// should pivot to req.Name (the real backend identity key) or assert on
// the HTTP Idempotency-Key header via DryRunRecord.Headers.
func buildRatePlanRequest(a scenario.TenantArchetype, productIDs []string, metricIDs []string) ratePlanCreateRequest {
	rc := a.RateConfig
	// Name MUST NOT contain square brackets — even though pricing-service
	// today only checks @NotBlank on rate-plan names, the platform-wide
	// convention is ValidBusinessName for any future strictening. Keep
	// loadgen-generated names within [a-zA-Z0-9 \-_.()] for forward-compat.
	body := ratePlanCreateRequest{
		Name:         fmt.Sprintf("Loadgen Rate Plan %s", a.Name),
		Description:  fmt.Sprintf("Auto-provisioned for archetype=%s pricing=%s billing=%s", a.Name, a.PricingModel, a.BillingMode),
		PricingModel: string(a.PricingModel),
		Currency:     "USD",
		ProductIDs:   productIDs,
	}
	if a.PricingModel == scenario.PricingFlatRate {
		body.BaseFee = rc.FlatFeeUSD
	}

	if len(metricIDs) == 0 && a.PricingModel != scenario.PricingFlatRate {
		// Fallback: every non-flat plan needs at least one metric. We synthesize
		// a placeholder metric ID — the integration test will fail loudly if
		// the catalog didn't return one, which is the intended signal.
		metricIDs = []string{"missing-metric"}
	}

	for _, mid := range metricIDs {
		mc := metricConfigRequest{
			MetricID:      mid,
			Model:         a.PricingModel,
			BillingTiming: "ARREARS",
		}
		switch a.PricingModel {
		case scenario.PricingFlatRate:
			// FLAT_RATE has no per-metric pricing — but the platform requires
			// at least one metric on every plan for usage attribution. Set
			// rate to 0 so the metric exists but contributes nothing to the bill.
			mc.Rate = floatPtr(0)
		case scenario.PricingPerUnit:
			mc.Rate = floatPtr(rc.PerUnitRateUSD)
			if rc.IncludedFreeUnits > 0 {
				mc.IncludedFree = rc.IncludedFreeUnits
			}
		case scenario.PricingPercentage:
			mc.Rate = floatPtr(rc.PercentageRate)
			if rc.MinFeeUSD > 0 {
				mc.MinFee = rc.MinFeeUSD
			}
		case scenario.PricingIncludedQuota:
			mc.IncludedFree = rc.IncludedFreeUnits
			mc.Rate = floatPtr(rc.PerUnitRateUSD)
			if rc.BlockSizeUnits > 0 {
				mc.BlockSize = rc.BlockSizeUnits
			}
			// "CHARGE" is the v3 ovBehavior value that means "bill the
			// overage at the metric's rate" — same intent as the legacy
			// "ALLOW" name but matches the pricing-service enum.
			mc.OvBehavior = "CHARGE"
		case scenario.PricingGraduated:
			mc.Tiers = tiersFromBands(rc.GraduatedTiers)
		case scenario.PricingVolumeTiered:
			mc.Tiers = tiersFromBands(rc.VolumeTiers)
		}
		body.MetricConfigs = append(body.MetricConfigs, mc)
	}

	return body
}

func tiersFromBands(bands []scenario.TierBand) []rateTierRequest {
	out := make([]rateTierRequest, len(bands))
	prevEnd := int64(0)
	for i, b := range bands {
		end := b.UpToUnits
		// up_to_units == 0 means "infinity" / "the rest" by scenario convention.
		if end == 0 {
			end = -1
		}
		out[i] = rateTierRequest{
			TierStart: prevEnd,
			TierEnd:   end,
			UnitPrice: b.UnitPriceUSD,
			FlatFee:   b.FlatFeeUSD,
			SortOrder: i,
		}
		prevEnd = b.UpToUnits
	}
	return out
}

// lookupRatePlanByName uses pricing-service's GET /api/v1/rate-plans/search
// with ?name= (substring filter, verified RatePlanController.search supports
// it) + client-side exact match. Loadgen-generated names ("Loadgen Rate Plan
// <archetype>") are unique per tenant so at most one exact match is expected.
func lookupRatePlanByName(ctx context.Context, c *Client, tenantID, name string) (ratePlanResponse, bool, error) {
	// /search is a sibling endpoint of the list root. Build the URL by
	// appending /search to the base /rate-plans path.
	searchURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathRatePlans+"/search")
	if err != nil {
		return ratePlanResponse{}, false, err
	}
	var plans []ratePlanResponse
	if _, err := listAllOptional(ctx, c, searchURL, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"name": {name}},
	}, &plans); err != nil {
		return ratePlanResponse{}, false, err
	}
	for _, p := range plans {
		if p.Name == name {
			return p, true, nil
		}
	}
	return ratePlanResponse{}, false, nil
}

// archiveRatePlan soft-archives a rate plan during --clean.
func archiveRatePlan(ctx context.Context, c *Client, tenantID, ratePlanID string) error {
	if ratePlanID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathRatePlanByID, ratePlanID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive rate plan %s: %w", ratePlanID, err)
	}
	return nil
}

// rateConfigSummary renders the rate config into a map suitable for the
// manifest's rate_plans[].config field. Used by Session 4 billing assertions.
func rateConfigSummary(a scenario.TenantArchetype) map[string]any {
	rc := a.RateConfig
	out := map[string]any{
		"pricing_model": string(a.PricingModel),
	}
	switch a.PricingModel {
	case scenario.PricingFlatRate:
		out["flat_fee_usd"] = rc.FlatFeeUSD
	case scenario.PricingPerUnit:
		out["per_unit_rate_usd"] = rc.PerUnitRateUSD
		if rc.IncludedFreeUnits > 0 {
			out["included_free_units"] = rc.IncludedFreeUnits
		}
	case scenario.PricingPercentage:
		out["percentage_rate"] = rc.PercentageRate
		if rc.MinFeeUSD > 0 {
			out["min_fee_usd"] = rc.MinFeeUSD
		}
		if rc.ChargeBasePerEventUSD > 0 {
			out["charge_base_per_event_usd"] = rc.ChargeBasePerEventUSD
		}
	case scenario.PricingIncludedQuota:
		out["included_free_units"] = rc.IncludedFreeUnits
		out["per_unit_rate_usd"] = rc.PerUnitRateUSD
		if rc.BlockSizeUnits > 0 {
			out["block_size_units"] = rc.BlockSizeUnits
		}
	case scenario.PricingGraduated:
		out["graduated_tiers"] = tierBandsAsMaps(rc.GraduatedTiers)
	case scenario.PricingVolumeTiered:
		out["volume_tiers"] = tierBandsAsMaps(rc.VolumeTiers)
	}
	return out
}

func tierBandsAsMaps(bands []scenario.TierBand) []map[string]any {
	out := make([]map[string]any, len(bands))
	for i, b := range bands {
		m := map[string]any{
			"up_to_units":    b.UpToUnits,
			"unit_price_usd": b.UnitPriceUSD,
		}
		if b.FlatFeeUSD > 0 {
			m["flat_fee_usd"] = b.FlatFeeUSD
		}
		out[i] = m
	}
	return out
}

// floatPtr returns a pointer to the given float64. Used to populate
// metricConfigRequest.Rate so the JSON encoder emits "rate":0 for
// FLAT_RATE archetypes instead of omitting the field via omitempty
// (which causes pricing-service to reject with 400 "Rate is required").
func floatPtr(v float64) *float64 { return &v }
