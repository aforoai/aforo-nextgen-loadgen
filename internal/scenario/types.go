// Package scenario defines the YAML schema for aforo-loadgen test scenarios.
//
// A Scenario is the contract every other component anchors to. The seed
// harness (Session 3) provisions tenants from the archetype list; the run
// engine (Session 3) shapes traffic from product_mix + ingestion_paths;
// negative-path injection (Session 7), lifecycle stress (Session 6),
// payments (Session 9), ERP (Session 10), and chaos (Session 11) all read
// their config from typed sub-trees here.
//
// Two pieces of the schema are deliberate and load-bearing:
//
//  1. schema_version pins the contract. Migration helpers in migration.go let
//     future versions evolve without breaking older scenario files.
//  2. TenantArchetype lets a single scenario provision deliberately varied
//     tenants — different pricing models, billing modes, currencies,
//     subscription states, discounts. This is what makes a 50- or 500-tenant
//     load test exercise the platform realistically rather than at one
//     uniform configuration.
package scenario

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentSchemaVersion is the schema version this build understands.
// Bump when introducing breaking changes; add a Migrate path for the prior
// version so older files keep loading.
//
// Version history:
//
//	v1 — initial contract (2026-03-xx). Single PricingModel + RateConfig per
//	     archetype; loadgen provisioned exactly one rate plan per tenant.
//	v2 — RateCards []RateCardSpec (2026-07-22). An archetype may declare N
//	     rate cards, each with its own pricing model / rate config / metric
//	     overrides / dimension pricing / offerings, and each with a share of
//	     the archetype's customer population. When RateCards is empty the
//	     migration path in applyDefaults backfills a single spec from the
//	     legacy top-level fields — every v1 scenario keeps loading and
//	     produces byte-identical output. Closes the "one rate card per
//	     product" tester bug: the platform's rate_plan_products M:N junction
//	     was always able to hold N rate cards for one product; loadgen's
//	     scenario schema is the layer that was constraining it to one.
const CurrentSchemaVersion = 2

// ProductType enumerates the four GA product types Aforo bills against.
// Mirrors com.aforo.billing.product.model.ProductType in the platform.
type ProductType string

const (
	ProductAPI        ProductType = "API"
	ProductAIAgent    ProductType = "AI_AGENT"
	ProductMCPServer  ProductType = "MCP_SERVER"
	ProductAgenticAPI ProductType = "AGENTIC_API"
)

// AllProductTypes is the canonical order used in defaults and validation.
var AllProductTypes = []ProductType{
	ProductAPI, ProductAIAgent, ProductMCPServer, ProductAgenticAPI,
}

// PricingModel enumerates the six pricing models supported by the platform's
// PricingCalculatorService. Mirrors values in pricing-service.
type PricingModel string

const (
	PricingPerUnit       PricingModel = "PER_UNIT"
	PricingFlatRate      PricingModel = "FLAT_RATE"
	PricingPercentage    PricingModel = "PERCENTAGE"
	PricingIncludedQuota PricingModel = "INCLUDED_QUOTA"
	PricingGraduated     PricingModel = "GRADUATED"
	PricingVolumeTiered  PricingModel = "VOLUME_TIERED"
)

// AllPricingModels is the canonical order — used by matrix scenarios.
var AllPricingModels = []PricingModel{
	PricingPerUnit, PricingFlatRate, PricingPercentage,
	PricingIncludedQuota, PricingGraduated, PricingVolumeTiered,
}

// BillingMode enumerates Aforo's three billing modes.
type BillingMode string

const (
	BillingPostpaid BillingMode = "POSTPAID"
	BillingPrepaid  BillingMode = "PREPAID"
	BillingHybrid   BillingMode = "HYBRID"
)

// AllBillingModes is the canonical order.
var AllBillingModes = []BillingMode{BillingPostpaid, BillingPrepaid, BillingHybrid}

// SubscriptionState enumerates the 9 states of the platform subscription
// state machine. Stale-key fault injection requires CANCELLED or EXPIRED to
// be present in subscription_state_mix.
type SubscriptionState string

const (
	StateCreated      SubscriptionState = "CREATED"
	StateTrialing     SubscriptionState = "TRIALING"
	StateActive       SubscriptionState = "ACTIVE"
	StatePastDue      SubscriptionState = "PAST_DUE"
	StatePaused       SubscriptionState = "PAUSED"
	StateExpiringSoon SubscriptionState = "EXPIRING_SOON"
	StateExpired      SubscriptionState = "EXPIRED"
	StateCancelled    SubscriptionState = "CANCELLED"
	StateSuspended    SubscriptionState = "SUSPENDED"
)

// AllSubscriptionStates lists the 9 states.
var AllSubscriptionStates = []SubscriptionState{
	StateCreated, StateTrialing, StateActive, StatePastDue, StatePaused,
	StateExpiringSoon, StateExpired, StateCancelled, StateSuspended,
}

// Distribution names the tenant-traffic shape across the tenant population.
type Distribution string

const (
	DistUniform    Distribution = "uniform"
	DistPareto8020 Distribution = "pareto_80_20"
	DistZipf       Distribution = "zipf"
)

// TimePattern names the 24h time-of-day shape applied to event traffic.
type TimePattern string

const (
	TimeConstant TimePattern = "constant"
	TimeSine24h  TimePattern = "sine_24h"
	TimeBursty   TimePattern = "bursty"
)

// TaxEngine names the tax-calculation backend.
type TaxEngine string

const (
	TaxMock    TaxEngine = "mock"
	TaxAvalara TaxEngine = "avalara"
	TaxVertex  TaxEngine = "vertex"
)

// StripeMode is "test" or "live". Production runs always set to "test"
// (this tool never points at a real Stripe account).
type StripeMode string

const (
	StripeTest StripeMode = "test"
	StripeLive StripeMode = "live"
)

// Duration is a time.Duration that decodes from a YAML string ("60s", "24h").
// yaml.v3 doesn't auto-decode time.Duration, so this wrapper does it via
// UnmarshalYAML. Marshaling round-trips through String().
type Duration time.Duration

// UnmarshalYAML decodes "60s", "5m", "24h" etc. into a time.Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string (e.g. \"60s\", \"24h\"): %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back to a Go-format duration string.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Scenario is the top-level YAML document.
//
// Sub-trees default to their zero value when omitted. Validation enforces
// cross-field invariants — see validator.go.
type Scenario struct {
	SchemaVersion    int              `yaml:"schema_version"`
	Name             string           `yaml:"name"`
	Description      string           `yaml:"description,omitempty"`
	TargetTPS        int              `yaml:"target_tps"`
	Duration         Duration         `yaml:"duration"`
	Seed             int64            `yaml:"seed,omitempty"`
	Tenants          Tenants          `yaml:"tenants"`
	TimePattern      TimePattern      `yaml:"time_pattern,omitempty"`
	ProductMix       ProductMix       `yaml:"product_mix,omitempty"`
	IngestionPaths   IngestionPaths   `yaml:"ingestion_paths,omitempty"`
	PayloadVariation PayloadVariation `yaml:"payload_variation,omitempty"`
	NegativePaths    NegativePaths    `yaml:"negative_paths,omitempty"`
	Lifecycle        LifecycleProfile `yaml:"lifecycle,omitempty"`
	Payments         Payments         `yaml:"payments,omitempty"`
	Tax              Tax              `yaml:"tax,omitempty"`
	ERP              ERP              `yaml:"erp,omitempty"`
	CreditNotes      CreditNotes      `yaml:"credit_notes,omitempty"`
	Wallet           Wallet           `yaml:"wallet,omitempty"`
	FX               FX               `yaml:"fx,omitempty"`
	Chaos            Chaos            `yaml:"chaos,omitempty"`
	Assertions       Assertions       `yaml:"assertions,omitempty"`
	Metadata         map[string]any   `yaml:"metadata,omitempty"`
}

// Tenants describes the tenant population to provision.
type Tenants struct {
	Count        int               `yaml:"count"`
	Distribution Distribution      `yaml:"distribution,omitempty"`
	Archetypes   []TenantArchetype `yaml:"archetypes"`
}

// TenantArchetype is a deliberately varied tenant configuration. The seed
// harness instantiates Count tenants from the weighted archetype list so a
// single scenario covers many pricing/billing/state combinations.
type TenantArchetype struct {
	Name                 string                        `yaml:"name"`
	Weight               float64                       `yaml:"weight"`
	PricingModel         PricingModel                  `yaml:"pricing_model"`
	BillingMode          BillingMode                   `yaml:"billing_mode"`
	ProductTypes         []ProductType                 `yaml:"product_types"`
	CustomerCount        int                           `yaml:"customer_count"`
	CurrencyMix          map[string]float64            `yaml:"currency_mix,omitempty"`
	SubscriptionStateMix map[SubscriptionState]float64 `yaml:"subscription_state_mix,omitempty"`
	DiscountMix          map[string]float64            `yaml:"discount_mix,omitempty"`
	RateConfig           RateConfig                    `yaml:"rate_config,omitempty"`

	// ProductsPerType controls how many DISTINCT product catalog entries
	// are created for EACH product type listed in ProductTypes. Default 1
	// preserves the historical single-product-per-type behavior. Set to N
	// to model a tenant with N products of every type (e.g.
	// ProductTypes=[API,AI_AGENT,MCP_SERVER,AGENTIC_API] +
	// ProductsPerType=4 → 16 products per tenant).
	//
	// Rejected values: <0 → validation error. 0 → treated as 1 (default).
	// Sensible cap: 25 per type — beyond that a scenario is likely
	// mis-configured; the validator will reject.
	//
	// The N products of the same type share the same metric names and
	// same rate plan; only the product name differs
	// ("Loadgen Product {archetype} {type} 01", "…02", etc.). This is
	// the intended shape for testing multi-product catalog UI, per-tenant
	// billable-unit dedup, and per-product analytics rollups.
	ProductsPerType int `yaml:"products_per_type,omitempty"`

	// MetricConfigs is an optional per-metric pricing override map keyed by
	// the descriptor's metric NAME (e.g. "Tokens Consumed", "Agent Sessions").
	// Metrics named in this map get the specified pricing model and rate on
	// the rate plan; metrics NOT in the map fall back to the archetype's
	// top-level PricingModel + RateConfig. This lets one archetype exercise
	// heterogeneous rate plans — the shape real customers configure — like:
	//
	//	metric_configs:
	//	  "Tokens Consumed":
	//	    pricing_model: GRADUATED
	//	    graduated_tiers: [...]
	//	  "GPU Hours":
	//	    pricing_model: FLAT_RATE
	//	    flat_fee_usd: 25
	//	  "Agent Sessions":
	//	    pricing_model: PER_UNIT
	//	    per_unit_rate_usd: 0.10
	//
	// Ignored metric names (e.g. typo, or a metric that doesn't exist for
	// the archetype's product types) are silently dropped — the seeder
	// logs a warning at rate-plan build time. Case-insensitive on the map
	// key so YAML author can write either "Tokens Consumed" or
	// "tokens consumed".
	MetricConfigs map[string]MetricOverride `yaml:"metric_configs,omitempty"`

	// DimensionPricing is the per-dimension multiplier map that lands on the
	// rate plan's dimensionPricing JSONB column. Keys are dimension names
	// (MCP `tool_name` / AI_AGENT `capability_name` / AGENTIC_API
	// `endpoint_path`), values are float multipliers applied to the base
	// rate for events carrying that dimension:
	//
	//	dimension_pricing:
	//	  web_search: 1.5      # 50% surcharge for web-search tool calls
	//	  generate_image: 3.0  # 3x for GPU-heavy tools
	//	  vector_search: 2.0
	//
	// Rule 21 (CLAUDE.md): this maps 1:1 to pricing-service's
	// RatePlan.dimensionPricing (canonical shape is a JSONB OBJECT map).
	// The v3 pipeline's AggregateStage.enrichWithDimensionData fans out one
	// billing line per dimension when the metric is PER_UNIT + zero-free —
	// so this only meaningfully exercises MCP_SERVER / AI_AGENT / AGENTIC_API
	// archetypes running one of those pricing models. FLAT_RATE, PERCENTAGE,
	// INCLUDED_QUOTA (with free tier), GRADUATED, and VOLUME_TIERED
	// intentionally stay on the aggregate single-line path (tier math would
	// disagree per fan-out).
	//
	// An empty map is dropped from the request body — no
	// `dimension_pricing:{}` is sent to pricing-service.
	DimensionPricing map[string]float64 `yaml:"dimension_pricing,omitempty"`

	// RateCards is the v2 shape for expressing N rate cards per archetype.
	// Each RateCardSpec carries its own pricing model / rate config / metric
	// overrides / dimension pricing / offerings and takes a share of the
	// archetype's CustomerCount.
	//
	// When absent (or empty), applyDefaults backfills exactly one card from
	// the legacy top-level PricingModel + BillingMode + RateConfig +
	// MetricConfigs + DimensionPricing fields, so every v1 scenario keeps
	// producing byte-identical output. When present, the legacy top-level
	// fields are IGNORED for provisioning (they may still be set for
	// documentation, but the seeder walks RateCards exclusively).
	//
	// Business use case (2026-07-22): a tenant sells the same product at
	// different price points to different customer segments (Starter / Pro /
	// Enterprise). The platform's rate_plan_products M:N junction has always
	// supported this; loadgen's scenario schema is what was forcing one
	// card per product until this field landed.
	RateCards []RateCardSpec `yaml:"rate_cards,omitempty"`
}

// RateCardSpec describes one rate card an archetype provisions on each of
// its tenants. Multiple specs let a single archetype exercise the
// "multiple rate cards per product" business scenario.
type RateCardSpec struct {
	// Name is the display suffix appended to the tenant's rate plan name
	// ("Loadgen Rate Plan <archetype> — <name>"). Must be unique within the
	// archetype's RateCards slice. When absent, defaults to "card-<index>".
	Name string `yaml:"name"`

	// PricingModel for this card. Required when set; falls back to the
	// archetype's top-level PricingModel when omitted.
	PricingModel PricingModel `yaml:"pricing_model,omitempty"`

	// BillingMode override for offerings under this card. Falls back to the
	// archetype's top-level BillingMode when omitted.
	BillingMode BillingMode `yaml:"billing_mode,omitempty"`

	// RateConfig for this card. Fields not set inherit from the archetype's
	// top-level RateConfig (per-field inheritance is NOT applied — the whole
	// struct is either supplied or inherited).
	RateConfig RateConfig `yaml:"rate_config,omitempty"`

	// MetricConfigs are per-metric pricing overrides scoped to THIS card.
	// See TenantArchetype.MetricConfigs for shape.
	MetricConfigs map[string]MetricOverride `yaml:"metric_configs,omitempty"`

	// DimensionPricing rides on this card's rate plan (Rule 21).
	DimensionPricing map[string]float64 `yaml:"dimension_pricing,omitempty"`

	// ProductFilter is optional. When non-empty, this card is bound only to
	// products of the listed types from the archetype's ProductTypes. Empty
	// means "bind to every product in the archetype" (default). Use this
	// when different rate cards should price different product-type subsets
	// on the same tenant (e.g. one card for MCP tools, another for AI
	// agents).
	ProductFilter []ProductType `yaml:"product_filter,omitempty"`

	// Offerings is the per-currency (or explicit) list of offerings that
	// wrap this card. When absent, defaults to ONE offering per currency in
	// the archetype's CurrencyMix (preserves v1 behavior). When present,
	// currencies are taken from OfferingSpec.Currency (defaults to "USD" if
	// unset).
	Offerings []OfferingSpec `yaml:"offerings,omitempty"`

	// CustomerShare is the fraction of the archetype's CustomerCount whose
	// subscription binds to THIS card. Sums across RateCards must equal
	// 1.0 ± 0.001 (validator enforces). When absent, defaults to equal
	// share (1.0 / len(RateCards)) so a v2 scenario with N cards and no
	// shares distributes customers uniformly.
	CustomerShare float64 `yaml:"customer_share,omitempty"`
}

// OfferingSpec describes one offering wrapping a RateCardSpec. Multiple
// specs let a single card be sold with different currencies / billing
// modes / trial windows.
type OfferingSpec struct {
	// Name is the display suffix appended to the offering name. When
	// absent, defaults to the currency ("USD" / "EUR" / ...).
	Name string `yaml:"name,omitempty"`

	// Currency for this offering (ISO-4217 code). When absent, defaults to
	// "USD".
	Currency string `yaml:"currency,omitempty"`

	// BillingMode override for this offering. Falls back to the parent
	// RateCardSpec.BillingMode → archetype BillingMode when omitted.
	BillingMode BillingMode `yaml:"billing_mode,omitempty"`

	// TrialDays override. Falls back to the parent RateCardSpec.RateConfig.TrialDays
	// → archetype RateConfig.TrialDays when omitted.
	TrialDays int `yaml:"trial_days,omitempty"`
}

// MetricOverride carries the per-metric pricing config used by
// TenantArchetype.MetricConfigs. Structurally a slim mirror of RateConfig
// with only the fields relevant to a single metric — flat fees at the plan
// level are on the archetype's RateConfig, not here.
type MetricOverride struct {
	PricingModel      PricingModel `yaml:"pricing_model"`
	PerUnitRateUSD    float64      `yaml:"per_unit_rate_usd,omitempty"`
	PercentageRate    float64      `yaml:"percentage_rate,omitempty"`
	MinFeeUSD         float64      `yaml:"min_fee_usd,omitempty"`
	IncludedFreeUnits int64        `yaml:"included_free_units,omitempty"`
	BlockSizeUnits    int64        `yaml:"block_size_units,omitempty"`
	FlatFeeUSD        float64      `yaml:"flat_fee_usd,omitempty"`
	GraduatedTiers    []TierBand   `yaml:"graduated_tiers,omitempty"`
	VolumeTiers       []TierBand   `yaml:"volume_tiers,omitempty"`
}

// RateConfig holds the pricing-model-specific knobs for an archetype.
//
// Not every field applies to every pricing model. Validation enforces the
// minimum requirements per model — e.g. PREPAID/HYBRID requires
// WalletInitialBalanceUSD > 0.
//
// ChargeBasePerEventUSD is the average per-event "charge base" used by the
// PERCENTAGE pricing oracle. PERCENTAGE bills (events × charge_base × rate)
// — typical use case is payment processing where the base is the average
// transaction amount. When zero, the validator falls back to 1.0 per event,
// which makes the oracle effectively reduce to (events × rate). Set this
// to a representative number (e.g. 100.0 for "$100 average transaction")
// for PERCENTAGE archetypes that want realistic billing assertions.
type RateConfig struct {
	FlatFeeUSD              float64    `yaml:"flat_fee_usd,omitempty"`
	PerUnitRateUSD          float64    `yaml:"per_unit_rate_usd,omitempty"`
	PercentageRate          float64    `yaml:"percentage_rate,omitempty"`
	ChargeBasePerEventUSD   float64    `yaml:"charge_base_per_event_usd,omitempty"`
	MinFeeUSD               float64    `yaml:"min_fee_usd,omitempty"`
	IncludedFreeUnits       int64      `yaml:"included_free_units,omitempty"`
	BlockSizeUnits          int64      `yaml:"block_size_units,omitempty"`
	GraduatedTiers          []TierBand `yaml:"graduated_tiers,omitempty"`
	VolumeTiers             []TierBand `yaml:"volume_tiers,omitempty"`
	WalletInitialBalanceUSD float64    `yaml:"wallet_initial_balance_usd,omitempty"`
	TrialDays               int        `yaml:"trial_days,omitempty"`
}

// TierBand is one band of a GRADUATED or VOLUME_TIERED rate.
type TierBand struct {
	UpToUnits    int64   `yaml:"up_to_units"`
	UnitPriceUSD float64 `yaml:"unit_price_usd"`
	FlatFeeUSD   float64 `yaml:"flat_fee_usd,omitempty"`
}

// ProductMix is the per-product-type traffic share. Weights sum to 1.0.
type ProductMix struct {
	API        float64 `yaml:"API,omitempty"`
	AIAgent    float64 `yaml:"AI_AGENT,omitempty"`
	MCPServer  float64 `yaml:"MCP_SERVER,omitempty"`
	AgenticAPI float64 `yaml:"AGENTIC_API,omitempty"`
}

// Sum is the weight total — used by the validator.
func (p ProductMix) Sum() float64 { return p.API + p.AIAgent + p.MCPServer + p.AgenticAPI }

// IngestionPaths is the per-ingest-channel traffic share. Weights sum to 1.0.
//
// Mirrors the four customer-facing ingestion tiers (REST, SDK, Gateway,
// Webhook, Upload) plus all 9 supported gateway adapters individually.
type IngestionPaths struct {
	RestDirect      float64 `yaml:"rest_direct,omitempty"`
	SDKNode         float64 `yaml:"sdk_node,omitempty"`
	SDKPython       float64 `yaml:"sdk_python,omitempty"`
	SDKJava         float64 `yaml:"sdk_java,omitempty"`
	SDKGo           float64 `yaml:"sdk_go,omitempty"`
	GatewayKong     float64 `yaml:"gateway_kong,omitempty"`
	GatewayApigee   float64 `yaml:"gateway_apigee,omitempty"`
	GatewayAWS      float64 `yaml:"gateway_aws,omitempty"`
	GatewayAzure    float64 `yaml:"gateway_azure,omitempty"`
	GatewayMuleSoft float64 `yaml:"gateway_mulesoft,omitempty"`
	GatewayAPISIX   float64 `yaml:"gateway_apisix,omitempty"`
	GatewayTyk      float64 `yaml:"gateway_tyk,omitempty"`
	GatewayGravitee float64 `yaml:"gateway_gravitee,omitempty"`
	GatewayEnvoy    float64 `yaml:"gateway_envoy,omitempty"`
	WebhookReceiver float64 `yaml:"webhook_receiver,omitempty"`
	CSVUpload       float64 `yaml:"csv_upload,omitempty"`
	// MCPJsonRPC emits real JSON-RPC 2.0 tools/call payloads to a
	// configurable MCP endpoint (AFORO_LOADGEN_MCP_URL). Closes the
	// gateway-plugin-detection gap — every other path either posts to
	// /v1/ingest directly or relies on a gateway acting as a reverse
	// proxy, neither of which exercises the plugin's tools/call
	// detection logic. Pair with product_mix.MCP_SERVER=1.0.
	MCPJsonRPC float64 `yaml:"mcp_jsonrpc,omitempty"`
	// AIAgentREST posts AI_AGENT usage events to a configurable ingest
	// endpoint (AFORO_LOADGEN_INGEST_URL). Same wire contract as
	// rest_direct but scoped to AI_AGENT events so a scenario can point
	// AI_AGENT traffic at a distinct URL and reject non-AI_AGENT events
	// loudly at the driver rather than discover the mis-pairing later
	// in the analytics stack. Closes the per-capability anomaly + dimension
	// pricing coverage gap noted in P8. Pair with product_mix.AI_AGENT=1.0.
	AIAgentREST float64 `yaml:"ai_agent_rest,omitempty"`
	// AIAgentWire POSTs AI_AGENT capability invocations against
	// @aforo/agent-test-server's REST wire protocol
	// (AFORO_LOADGEN_AGENT_URL). Closes the SDK → server → optional-gateway
	// → usage-ingestor coverage gap that ai_agent_rest structurally cannot
	// reach: ai_agent_rest posts the standard ingest envelope directly,
	// bypassing the SDK. ai_agent_wire speaks what a real
	// @aforoai/agent-metering / aforo-agent-metering client would send.
	// Pair with product_mix.AI_AGENT=1.0 and a running instance of
	// aforo-metering-sdks/agent-test-server.
	AIAgentWire float64 `yaml:"ai_agent_wire,omitempty"`
}

// Sum is the weight total — used by the validator.
func (p IngestionPaths) Sum() float64 {
	return p.RestDirect + p.SDKNode + p.SDKPython + p.SDKJava + p.SDKGo +
		p.GatewayKong + p.GatewayApigee + p.GatewayAWS + p.GatewayAzure +
		p.GatewayMuleSoft + p.GatewayAPISIX + p.GatewayTyk + p.GatewayGravitee +
		p.GatewayEnvoy + p.WebhookReceiver + p.CSVUpload + p.MCPJsonRPC +
		p.AIAgentREST + p.AIAgentWire
}

// PayloadVariation chooses the size mix of generated event bodies.
type PayloadVariation struct {
	SmallPct  float64 `yaml:"small_pct,omitempty"`  // ~200 bytes
	MediumPct float64 `yaml:"medium_pct,omitempty"` // ~2KB
	LargePct  float64 `yaml:"large_pct,omitempty"`  // ~20KB nested
}

// Sum returns the total share — should be 1.0 (validator enforces with tolerance).
func (p PayloadVariation) Sum() float64 { return p.SmallPct + p.MediumPct + p.LargePct }

// NegativePaths controls fault injection. All fractions are in [0, 1] and
// represent share of total traffic, NOT share of one another. Setting all
// to zero leaves the run on the happy path.
type NegativePaths struct {
	LateEventsPct   float64 `yaml:"late_events_pct,omitempty"`   // event_timestamp 2h in the past
	FutureEventsPct float64 `yaml:"future_events_pct,omitempty"` // >5min future (rejected)
	MalformedPct    float64 `yaml:"malformed_pct,omitempty"`     // invalid JSON
	WrongAuthPct    float64 `yaml:"wrong_auth_pct,omitempty"`    // fabricated bad credentials
	StaleKeysPct    float64 `yaml:"stale_keys_pct,omitempty"`    // keys from CANCELLED/EXPIRED subs
	OversizePct     float64 `yaml:"oversize_pct,omitempty"`      // >max body size
}

// LifecycleProfile drives subscription state transitions during a run.
// Exercised by Session 6.
type LifecycleProfile struct {
	Enabled                   bool    `yaml:"enabled"`
	UpgradesPerHourPct        float64 `yaml:"upgrades_per_hour_pct,omitempty"`
	DowngradesPerHourPct      float64 `yaml:"downgrades_per_hour_pct,omitempty"`
	PauseResumePerHourPct     float64 `yaml:"pause_resume_per_hour_pct,omitempty"`
	TrialConversionPerHourPct float64 `yaml:"trial_conversion_per_hour_pct,omitempty"`
	TrialCancelPerHourPct     float64 `yaml:"trial_cancel_per_hour_pct,omitempty"`
	MigratePerHourPct         float64 `yaml:"migrate_per_hour_pct,omitempty"`
	RetryPaymentPerHourPct    float64 `yaml:"retry_payment_per_hour_pct,omitempty"`
}

// Payments drives Stripe-mode payment simulation. Exercised by Session 9.
//
// stripe_mode=test pulls STRIPE_TEST_SECRET_KEY from the environment at run
// time. Validator does NOT read the env — it only enforces shape.
//
// SuccessPct + DeclinePct + InsufficientFundsPct must sum to 1.0 when any
// of them is non-zero. Setting all three to zero leaves the runner default
// (which is success=1.0).
type Payments struct {
	Enabled              bool       `yaml:"enabled"`
	StripeMode           StripeMode `yaml:"stripe_mode,omitempty"`
	SuccessPct           float64    `yaml:"success_pct,omitempty"`
	DeclinePct           float64    `yaml:"decline_pct,omitempty"`
	InsufficientFundsPct float64    `yaml:"insufficient_funds_pct,omitempty"`

	// DunningMaxAttempts mirrors the platform's
	// aforo.dunning.max-attempts. Used by the dunning driver to assert
	// escalation to SUSPEND/CANCEL after this many failed retries.
	// Default 3.
	DunningMaxAttempts int `yaml:"dunning_max_attempts,omitempty"`

	// DunningRetryIntervalSeconds compresses the platform retry cadence
	// for load-test purposes. Real dunning runs over days; tests use 60s.
	// Default 60.
	DunningRetryIntervalSeconds int `yaml:"dunning_retry_interval_seconds,omitempty"`

	// IdempotencyPrefix prefixes every Stripe Idempotency-Key generated
	// by the payment driver. Recommended: include the run id when set
	// from the orchestrator. Default "aforo-loadgen".
	IdempotencyPrefix string `yaml:"idempotency_prefix,omitempty"`
}

// Tax configures the tax-calculation engine. Exercised by Session 9.
//
// Two ways to configure jurisdictions:
//
//	Jurisdictions: map[code]rate                   — flat lookup table
//	JurisdictionByCurrency: map[currency]code      — pick a default rate per currency
//
// DefaultJurisdiction is the fallback when neither map matches.
//
// ToleranceUSD is the per-line absolute tolerance the validator allows when
// comparing platform tax_amount to the engine's expected number. 0 → 0.01
// (one cent — covers IEEE rounding).
type Tax struct {
	Engine                 TaxEngine          `yaml:"engine,omitempty"`
	Jurisdictions          map[string]float64 `yaml:"jurisdictions,omitempty"`
	JurisdictionByCurrency map[string]string  `yaml:"jurisdiction_by_currency,omitempty"`
	DefaultJurisdiction    string             `yaml:"default_jurisdiction,omitempty"`
	ToleranceUSD           float64            `yaml:"tolerance_usd,omitempty"`
}

// ERP drives ERP sync simulation. Exercised by Session 9.
//
// MultiERPEnabled overrides the platform aforo.multi-erp.enabled flag at
// runtime — needed for the single-ERP-invariant check (Check 18) to assert
// that a SECOND connect fails with 409 by default.
type ERP struct {
	Enabled               bool               `yaml:"enabled"`
	ProvidersPerTenantMix map[string]float64 `yaml:"providers_per_tenant_mix,omitempty"`
	SyncSLASeconds        int                `yaml:"sync_sla_seconds,omitempty"`

	// MultiERPEnabled controls whether the load test asserts the
	// single-ERP invariant (default false → asserts 409 on second
	// connect) or the multi-ERP path (true → asserts both succeed).
	MultiERPEnabled bool `yaml:"multi_erp_enabled,omitempty"`

	// MaxRetries mirrors the platform's ErpSyncService retry policy.
	// Default 3.
	MaxRetries int `yaml:"max_retries,omitempty"`

	// VerifyExternalIDs requires the validator to confirm each invoice
	// has a non-empty externalDocumentId after sync (provider-side proof
	// of insertion). Default true when erp.enabled.
	VerifyExternalIDs bool `yaml:"verify_external_ids,omitempty"`
}

// CreditNotes drives refund / partial credit simulation.
//
// RefundPct is the share of PAID invoices that get a full credit note.
// PartialPct is the share that get a partial credit note. The two are
// mutually exclusive per invoice — an invoice goes through at most one.
//
// PartialAmountPct controls the amount of the partial: 0.30 means the
// credit note is 30% of the invoice total. 0 → 0.50 (half).
//
// ApplyToInvoicePct is the share of issued credit notes that are applied
// back against an open invoice (vs. cash refunded). 1.0 → all applied.
type CreditNotes struct {
	Enabled           bool    `yaml:"enabled"`
	RefundPct         float64 `yaml:"refund_pct,omitempty"`
	PartialPct        float64 `yaml:"partial_pct,omitempty"`
	PartialAmountPct  float64 `yaml:"partial_amount_pct,omitempty"`
	ApplyToInvoicePct float64 `yaml:"apply_to_invoice_pct,omitempty"`
	Reason            string  `yaml:"reason,omitempty"`
}

// Wallet controls wallet-specific assertions during a run.
//
// HoldTTLSeconds compresses the platform's hold expiry from "billing period"
// down to a short TTL so HoldExpiryScheduler can complete inside a test
// window. Default 60s.
//
// BalanceAuditEnabled enables the pre/post wallet audit (Check 17).
type Wallet struct {
	HoldExpiryAudit     bool `yaml:"hold_expiry_audit,omitempty"`
	HoldTTLSeconds      int  `yaml:"hold_ttl_seconds,omitempty"`
	BalanceAuditEnabled bool `yaml:"balance_audit_enabled,omitempty"`
}

// FX configures multi-currency reproducibility. Pinning rates in the scenario
// keeps assertions stable across runs even though the platform pulls live FX
// at bill-run time in production.
//
// PinnedRates keys are "FROM->TO" — e.g. "USD->EUR": 0.92.
//
// AppliedAt records WHERE in the pipeline the rate is applied. The platform
// applies FX at bill-run time (NOT event-ingest time); Check 14 asserts
// this. Default "bill_run_time".
type FX struct {
	Enabled     bool               `yaml:"enabled,omitempty"`
	PinnedRates map[string]float64 `yaml:"pinned_rates,omitempty"`
	AppliedAt   string             `yaml:"applied_at,omitempty"`
}

// Chaos drives infra-level fault injection. Exercised by Session 11.
type Chaos struct {
	Enabled bool         `yaml:"enabled"`
	Events  []ChaosEvent `yaml:"events,omitempty"`
}

// ChaosEvent is one infra fault. Type is one of the chaos package's
// supported scenarios: kafka_kill, redis_flush, ch_slowdown, net_partition.
//
// The YAML shape supports two ergonomic forms — both decode into the same
// struct via a custom UnmarshalYAML:
//
//	# nested params (canonical)
//	- at: 1h
//	  type: ch_slowdown
//	  duration: 5m
//	  params:
//	    instance_id: i-1234
//	    latency_ms: 500
//
//	# inline params (shorthand)
//	- at: 1h
//	  type: ch_slowdown
//	  duration: 5m
//	  instance_id: i-1234
//	  latency_ms: 500
//
// Inline keys are merged into Params at decode time; nested params keys
// take precedence on conflict. Notes is operator free-text embedded into
// the chaos timeline outcome record.
type ChaosEvent struct {
	At       Duration       `yaml:"at"`
	Type     string         `yaml:"type"`
	Duration Duration       `yaml:"duration"`
	Notes    string         `yaml:"notes,omitempty"`
	Params   map[string]any `yaml:"params,omitempty"`
}

// reservedChaosKeys are the YAML keys handled directly on ChaosEvent;
// every other key on a chaos event node is hoisted into Params.
var reservedChaosKeys = map[string]struct{}{
	"at":       {},
	"type":     {},
	"duration": {},
	"notes":    {},
	"params":   {},
}

// UnmarshalYAML decodes a chaos event, accepting both the canonical
// "params: {...}" shape and the inline shorthand. Inline keys are
// hoisted into Params; nested params keys win on conflict.
//
// The implementation is intentionally hand-rolled instead of using a
// shadow struct because we want to (a) accept arbitrary unknown keys
// without disabling KnownFields(true) globally, and (b) preserve
// position metadata for nested params via yaml.Node decoding.
func (e *ChaosEvent) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("chaos event must be a mapping, got %v", value.Kind)
	}
	merged := map[string]any{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		k := value.Content[i].Value
		v := value.Content[i+1]
		switch k {
		case "at":
			var d Duration
			if err := d.UnmarshalYAML(v); err != nil {
				return fmt.Errorf("chaos.at: %w", err)
			}
			e.At = d
		case "type":
			if err := v.Decode(&e.Type); err != nil {
				return fmt.Errorf("chaos.type: %w", err)
			}
		case "duration":
			var d Duration
			if err := d.UnmarshalYAML(v); err != nil {
				return fmt.Errorf("chaos.duration: %w", err)
			}
			e.Duration = d
		case "notes":
			if err := v.Decode(&e.Notes); err != nil {
				return fmt.Errorf("chaos.notes: %w", err)
			}
		case "params":
			if e.Params == nil {
				e.Params = map[string]any{}
			}
			var p map[string]any
			if err := v.Decode(&p); err != nil {
				return fmt.Errorf("chaos.params: %w", err)
			}
			for pk, pv := range p {
				e.Params[pk] = pv
			}
		default:
			// Inline-shorthand: hoist into Params unless it's reserved.
			if _, reserved := reservedChaosKeys[k]; reserved {
				continue
			}
			var anyVal any
			if err := v.Decode(&anyVal); err != nil {
				return fmt.Errorf("chaos.%s: %w", k, err)
			}
			merged[k] = anyVal
		}
	}
	if len(merged) > 0 {
		if e.Params == nil {
			e.Params = map[string]any{}
		}
		// Inline keys lose to explicit `params:` keys (canonical wins).
		for k, v := range merged {
			if _, ok := e.Params[k]; !ok {
				e.Params[k] = v
			}
		}
	}
	return nil
}

// Assertions are the post-run pass/fail thresholds.
type Assertions struct {
	EventsLostMax                    int     `yaml:"events_lost_max,omitempty"`
	InvoiceRevenueDriftPctMax        float64 `yaml:"invoice_revenue_drift_pct_max,omitempty"`
	P99LatencyMsMax                  int     `yaml:"p99_latency_ms_max,omitempty"`
	PerTenantP99FairnessMaxStddevPct float64 `yaml:"per_tenant_p99_fairness_max_stddev_pct,omitempty"`
	RedisCacheHitRatioMin            float64 `yaml:"redis_cache_hit_ratio_min,omitempty"`
	CrossTenantLeakageMax            int     `yaml:"cross_tenant_leakage_max,omitempty"`
	PerArchetypeBillingMatch         bool    `yaml:"per_archetype_billing_match,omitempty"`
	StaleKeyZeroFalsePositives       bool    `yaml:"stale_key_zero_false_positives,omitempty"`
}
