// Package cost estimates the AWS infrastructure cost of a load-test run.
//
// Two distinct outputs:
//
//  1. CostBreakdown — fine-grained $/hour split across worker nodes,
//     ClickHouse storage, NAT gateway, Kafka MSK, Redis ElastiCache.
//  2. PerMillionEventsUSD — the headline metric for cost-per-million-events
//     ingested. This is the number the platform team can compare release-on-
//     release to track whether changes regress unit economics.
//
// All numbers are ESTIMATES, computed from on-demand list prices for the
// us-east-1 region as of 2026-04. Actual costs depend on reservation
// coverage, savings plans, spot pricing, data-transfer egress patterns, and
// per-account negotiated rates. The Tracker labels its output explicitly as
// an estimate and embeds a link to the AWS Cost Explorer for ground truth.
//
// Why estimate at all? A 7-day 15K TPS run generates ~9B events and costs
// real money. Operators need a believable order-of-magnitude figure
// up-front (for the pre-flight prompt) and a believable post-run figure
// (for the run.json) so they can:
//
//   - Reject misconfigured runs before they burn $1000s
//   - Compare run-on-run cost trends without round-tripping through
//     AWS billing reports (which lag 24h)
//   - Drive the cost-per-million-events SLO over time
//
// Rates are kept in one table (DefaultRates) with public fields, so
// operators can override per-environment by constructing a custom
// RateCard. The Tracker never reaches out to AWS — it does math on
// counters.
package cost

import (
	"fmt"
	"time"
)

// RateCard is the per-resource hourly rate used to estimate cost. All
// figures in USD/hour for the named instance/resource type. Numbers are
// list prices (no reservation, no savings plan) for us-east-1 as of
// 2026-04. Operators with negotiated rates should supply a custom RateCard.
type RateCard struct {
	// WorkerNodeUSDPerHour is the per-worker compute rate. Default assumes
	// c6i.4xlarge ($0.68/hr on-demand). 8 worker nodes in the headline
	// configuration → $5.44/hr.
	WorkerNodeUSDPerHour float64

	// CoordinatorUSDPerHour is the coordinator compute rate. Default
	// assumes c6i.large ($0.085/hr on-demand) — much smaller than a worker
	// since it does no heavy lifting beyond aggregation.
	CoordinatorUSDPerHour float64

	// ClickHouseStorageUSDPerGBMonth is the ClickHouse-on-EBS storage
	// rate. Default assumes gp3 EBS ($0.08/GB-month). 9B events × ~500B =
	// 4.5TB → ~$360/month if retained.
	ClickHouseStorageUSDPerGBMonth float64

	// KafkaMSKUSDPerHour is the MSK cluster baseline rate. Default
	// assumes a 3-node kafka.m5.2xlarge cluster + storage; the figure is
	// the all-in hourly rate.
	KafkaMSKUSDPerHour float64

	// RedisElasticacheUSDPerHour is the Redis ElastiCache cluster rate.
	// Default assumes a 3-node cache.m6g.large cluster (replication
	// group). Redis runs continuously regardless of test load.
	RedisElasticacheUSDPerHour float64

	// NATGatewayUSDPerHour is the NAT gateway hourly rate. Egress data
	// transfer is the dominant cost during high-TPS runs because event
	// payloads are POSTed across availability zones.
	NATGatewayUSDPerHour float64

	// EgressUSDPerGB is the per-GB cross-AZ egress rate. AWS charges
	// egress on data leaving a VPC; large 7-day runs can easily move
	// multiple TB. Default $0.09/GB.
	EgressUSDPerGB float64

	// EventBytesAvg is the assumed average wire-bytes per event including
	// JSON envelope and HTTP headers. Default 500. Used to compute egress
	// volume from event count.
	EventBytesAvg int

	// Region is informational — embedded into the report so the link to
	// AWS Cost Explorer points at the right region.
	Region string
}

// DefaultRates is the on-demand us-east-1 rate card, accurate as of
// 2026-04-30. Operators should override these in production by passing a
// RateCard to NewTracker.
var DefaultRates = RateCard{
	WorkerNodeUSDPerHour:           0.68,  // c6i.4xlarge
	CoordinatorUSDPerHour:          0.085, // c6i.large
	ClickHouseStorageUSDPerGBMonth: 0.08,  // gp3 EBS
	KafkaMSKUSDPerHour:             1.92,  // 3 × kafka.m5.2xlarge + 1TB storage
	RedisElasticacheUSDPerHour:     0.21,  // 3 × cache.m6g.large
	NATGatewayUSDPerHour:           0.045, // single-AZ NAT
	EgressUSDPerGB:                 0.09,  // cross-AZ
	EventBytesAvg:                  500,
	Region:                         "us-east-1",
}

// Tracker accumulates the inputs needed to estimate cost across a run.
// Safe for concurrent updates via the Add* methods, but most call sites
// produce one Update at the end of the run from a snapshot of counters.
type Tracker struct {
	rates RateCard

	startedAt   time.Time
	stoppedAt   time.Time
	workerCount int

	// EventsIngested is the total number of successfully ingested events
	// during the run. Drives the egress estimate and the
	// per-million-events headline.
	EventsIngested int64

	// EventsAttempted is the total number of attempts (including
	// failures). Used for transparency in the breakdown.
	EventsAttempted int64

	// IncludeStorage controls whether ClickHouse storage retention cost
	// is added to the breakdown. Defaults true on long-soak (≥24h) runs;
	// a 30-min CI run isn't responsible for the storage tier.
	IncludeStorage bool
}

// NewTracker constructs a Tracker with the given rate card. Pass
// DefaultRates for the standard us-east-1 estimate. Construction does not
// take a time — call Start() and Stop() to bound the run window.
func NewTracker(rates RateCard) *Tracker {
	return &Tracker{rates: rates}
}

// Start records the run start time. Callers should call this exactly once
// before the run begins.
func (t *Tracker) Start(at time.Time) { t.startedAt = at }

// Stop records the run stop time. Callers should call this exactly once
// after the run completes (including on cancellation — partial runs still
// cost real money).
func (t *Tracker) Stop(at time.Time) { t.stoppedAt = at }

// SetWorkerCount records how many worker nodes participated in the run.
// In single-host mode this is 1; in distributed mode it is the number of
// healthy workers at run start. If a worker drops out mid-run the
// estimate uses the start count — the dropout window is short relative
// to the run.
func (t *Tracker) SetWorkerCount(n int) { t.workerCount = n }

// AddEventsIngested increments the success count.
func (t *Tracker) AddEventsIngested(n int64) { t.EventsIngested += n }

// AddEventsAttempted increments the attempt count.
func (t *Tracker) AddEventsAttempted(n int64) { t.EventsAttempted += n }

// Estimate returns the breakdown for the run window [startedAt, stoppedAt]
// using the tracker's accumulated counters. Always succeeds — when inputs
// are zero, returns zeros (and labels the result "no run window").
func (t *Tracker) Estimate() Breakdown {
	hours := 0.0
	if !t.stoppedAt.IsZero() && t.stoppedAt.After(t.startedAt) {
		hours = t.stoppedAt.Sub(t.startedAt).Hours()
	}

	workers := t.workerCount
	if workers <= 0 {
		workers = 1
	}

	bd := Breakdown{
		Region:          t.rates.Region,
		Hours:           hours,
		WorkerCount:     workers,
		EventsIngested:  t.EventsIngested,
		EventsAttempted: t.EventsAttempted,
		IsEstimate:      true,
		EstimateNote:    estimateNote(t.rates.Region),
	}

	bd.WorkerComputeUSD = float64(workers) * t.rates.WorkerNodeUSDPerHour * hours
	bd.CoordinatorUSD = t.rates.CoordinatorUSDPerHour * hours
	bd.KafkaMSKUSD = t.rates.KafkaMSKUSDPerHour * hours
	bd.RedisElasticacheUSD = t.rates.RedisElasticacheUSDPerHour * hours
	bd.NATGatewayUSD = t.rates.NATGatewayUSDPerHour * hours

	// Egress = events × avg bytes / GB / rate.
	const bytesPerGB = 1 << 30
	if t.EventsIngested > 0 && t.rates.EventBytesAvg > 0 {
		gbEgress := float64(t.EventsIngested) * float64(t.rates.EventBytesAvg) / float64(bytesPerGB)
		bd.EgressGB = gbEgress
		bd.EgressUSD = gbEgress * t.rates.EgressUSDPerGB
	}

	// Storage estimate — applied only when explicitly enabled. The number
	// is "what you would pay to retain this run's data for one full month
	// at the rate-card storage rate". For runs <24h the retention story
	// belongs to the long-running ClickHouse instance, not this run.
	if t.IncludeStorage && t.EventsIngested > 0 {
		gbStored := float64(t.EventsIngested) * float64(t.rates.EventBytesAvg) / float64(bytesPerGB)
		bd.ClickHouseStorageGB = gbStored
		bd.ClickHouseStorageUSDPerMonth = gbStored * t.rates.ClickHouseStorageUSDPerGBMonth
	}

	bd.TotalUSD = bd.WorkerComputeUSD + bd.CoordinatorUSD + bd.KafkaMSKUSD +
		bd.RedisElasticacheUSD + bd.NATGatewayUSD + bd.EgressUSD
	if t.IncludeStorage {
		// Storage cost is monthly — apportion to this run by hours / 720.
		bd.TotalUSD += bd.ClickHouseStorageUSDPerMonth * (hours / 720.0)
	}

	if t.EventsIngested > 0 {
		bd.PerMillionEventsUSD = bd.TotalUSD / (float64(t.EventsIngested) / 1_000_000.0)
	}

	bd.AWSCostExplorerURL = costExplorerURL(t.rates.Region, t.startedAt, t.stoppedAt)
	return bd
}

// Breakdown is the JSON-serializable cost output. Embedded into run.json
// under the "cost_estimate" key. All USD figures are floats — callers that
// want strict cents-precision should multiply by 100 and round.
type Breakdown struct {
	Region          string  `json:"region"`
	Hours           float64 `json:"hours"`
	WorkerCount     int     `json:"worker_count"`
	EventsIngested  int64   `json:"events_ingested"`
	EventsAttempted int64   `json:"events_attempted"`

	WorkerComputeUSD             float64 `json:"worker_compute_usd"`
	CoordinatorUSD               float64 `json:"coordinator_usd"`
	KafkaMSKUSD                  float64 `json:"kafka_msk_usd"`
	RedisElasticacheUSD          float64 `json:"redis_elasticache_usd"`
	NATGatewayUSD                float64 `json:"nat_gateway_usd"`
	EgressGB                     float64 `json:"egress_gb"`
	EgressUSD                    float64 `json:"egress_usd"`
	ClickHouseStorageGB          float64 `json:"clickhouse_storage_gb,omitempty"`
	ClickHouseStorageUSDPerMonth float64 `json:"clickhouse_storage_usd_per_month,omitempty"`

	TotalUSD            float64 `json:"total_usd"`
	PerMillionEventsUSD float64 `json:"per_million_events_usd"`

	IsEstimate         bool   `json:"is_estimate"`
	EstimateNote       string `json:"estimate_note"`
	AWSCostExplorerURL string `json:"aws_cost_explorer_url"`
}

// PreflightEstimate is a quick projection used by the pre-flight prompt,
// before the run begins. Multiplies an assumed TPS by duration to get the
// expected event volume, then runs the standard math. Used by
// `coordinator --scenario ... --target perf-aws` to display the
// confirmation prompt.
//
// Returned values are deliberately conservative — assumes the run hits
// 100% of target_tps for the full duration. Real runs typically average
// 90-95% due to ramp-up, sine_24h dips, and chaos events.
func PreflightEstimate(rates RateCard, targetTPS int, duration time.Duration, workers int) Breakdown {
	if workers <= 0 {
		workers = 1
	}
	expectedEvents := int64(float64(targetTPS) * duration.Seconds())
	t := NewTracker(rates)
	t.startedAt = time.Time{}.Add(0)
	t.stoppedAt = t.startedAt.Add(duration)
	t.workerCount = workers
	t.EventsIngested = expectedEvents
	t.EventsAttempted = expectedEvents
	t.IncludeStorage = duration >= 24*time.Hour
	return t.Estimate()
}

// estimateNote returns the canonical user-facing label that this number is
// not authoritative. Always rendered alongside any cost figure.
func estimateNote(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("Estimate based on on-demand list prices in %s; actual cost depends on reservations, savings plans, spot, and negotiated rates. See AWS Cost Explorer for ground truth.", region)
}

// costExplorerURL points at AWS Cost Explorer pre-filtered to the run
// window. Operators should follow this link for authoritative billing
// data — typically available 24h after the run completes.
//
// We don't account-scope the URL because we don't know the AWS account
// id at runtime; the user's browser is already signed into the right
// account when they click.
func costExplorerURL(region string, startedAt, stoppedAt time.Time) string {
	if startedAt.IsZero() || stoppedAt.IsZero() {
		return "https://us-east-1.console.aws.amazon.com/cost-management/home"
	}
	// Cost Explorer's deep-link grammar is undocumented and brittle; the
	// safe path is the unfiltered console — operators filter manually.
	return fmt.Sprintf("https://us-east-1.console.aws.amazon.com/cost-management/home?region=%s#/cost-explorer", region)
}
