package billing

import (
	"sort"
	"strings"
)

// OutcomeUsage is one per-status row of usage — the shape usage-ingestor's
// GET /api/v1/usage-events/summary/by-outcome returns to billing. Status ""
// means "sent without a status". Quantity is the summed quantity (SUM
// metrics); EventCount is the number of events (COUNT metrics). Both are
// float64 so the validator can split an estimated event total by an
// outcome mix without rounding.
type OutcomeUsage struct {
	Status     string
	Quantity   float64
	EventCount float64
}

// WeightedOutcomes is the result of WeightOutcomes.
type WeightedOutcomes struct {
	Weighted   float64
	Unweighted float64
}

// WeightOutcomes mirrors billing-service OutcomeWeighting.apply
// (com.aforo.billing.invoice.usage):
//
//	weighted = Σ(units for a status × weight for that status)
//
// where "units" is the summed quantity for SUM metrics (and empty /
// legacy aggregation) and the event count for COUNT metrics. A status with
// no configured weight counts at 1.0; so does an event without a status
// (blank, or the literal "null"). Weight keys and statuses are matched
// case-insensitively. Each status line is rounded to 6 dp (billing's scale).
//
// ok is false — and the caller bills the unweighted total — when there are
// no rows, no weights, or the aggregation (MAX / AVG / UNIQUE / anything
// else) can't be split by status.
func WeightOutcomes(outcomes []OutcomeUsage, weights map[string]float64, aggregation string) (WeightedOutcomes, bool) {
	if len(outcomes) == 0 {
		return WeightedOutcomes{}, false
	}
	norm := normalizeWeights(weights)
	if len(norm) == 0 {
		return WeightedOutcomes{}, false
	}
	agg := strings.ToUpper(strings.TrimSpace(aggregation))
	if agg != "" && agg != "SUM" && agg != "COUNT" {
		return WeightedOutcomes{}, false
	}
	byCount := agg == "COUNT"

	var res WeightedOutcomes
	for _, o := range outcomes {
		units := o.Quantity
		if byCount {
			units = o.EventCount
		}
		weight := 1.0
		if key := statusKey(o.Status); key != "" {
			if w, ok := norm[key]; ok {
				weight = w
			}
		}
		res.Weighted += round6(units * weight)
		res.Unweighted += units
	}
	res.Weighted = round6(res.Weighted)
	return res, true
}

// ExpectedOutcomes splits an event total across an outcome mix (status →
// share, shares sum to ≤ 1.0) for COUNT-style estimation: each status gets
// events × share events, one unit per event. Any share the mix doesn't cover
// lands on the "no status" row, which bills at 1.0. Rows are sorted by
// status so the result is deterministic.
func ExpectedOutcomes(events int64, mix map[string]float64) []OutcomeUsage {
	if events <= 0 {
		return nil
	}
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]OutcomeUsage, 0, len(keys)+1)
	covered := 0.0
	total := float64(events)
	for _, k := range keys {
		share := mix[k]
		if share <= 0 {
			continue
		}
		covered += share
		n := total * share
		out = append(out, OutcomeUsage{Status: k, Quantity: n, EventCount: n})
	}
	if rest := 1 - covered; rest > 1e-9 {
		n := total * rest
		out = append(out, OutcomeUsage{Status: "", Quantity: n, EventCount: n})
	}
	return out
}

// normalizeWeights upper-cases + trims weight keys (billing's
// OutcomeWeighting.normalizeWeights). Blank keys are dropped.
func normalizeWeights(weights map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(weights))
	for k, v := range weights {
		key := strings.ToUpper(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		out[key] = v
	}
	return out
}

// statusKey normalises a usage row's status; "" means "no status".
func statusKey(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "null") {
		return ""
	}
	return strings.ToUpper(t)
}
