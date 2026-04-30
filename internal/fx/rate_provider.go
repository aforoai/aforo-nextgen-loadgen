// Package fx is the loadgen tool's FX (foreign exchange) rate authority.
//
// Aforo bills in USD, EUR, and GBP. The platform fetches live FX at
// bill-run time (NOT at event-ingest time — see Check 14). For load tests
// we PIN rates so assertions are deterministic across runs:
//
//	scenario.fx.pinned_rates:
//	  USD->EUR: 0.92
//	  USD->GBP: 0.79
//	  EUR->USD: 1.087
//
// The Provider interface is what the rest of the test framework programs
// against. The pinned implementation reads from the scenario; a future
// "live" implementation could call api.exchangerate.host. Tests inject
// a stub via NewMapProvider.
//
// Rates are POSITIVE multipliers: amount_in_TO = amount_in_FROM * rate.
// A missing pair returns ErrUnknownPair — callers MUST handle this rather
// than silently returning the input amount in the wrong currency.
package fx

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// ErrUnknownPair is returned when no pinned rate exists for a (from, to) pair.
var ErrUnknownPair = errors.New("fx: unknown currency pair")

// Provider is the contract every FX backend implements.
type Provider interface {
	// Rate returns the multiplier for converting one unit of `from` to `to`.
	// Same currency always returns 1.0 with no error.
	Rate(from, to string) (float64, error)
	// Convert applies the rate to amount and returns the converted amount.
	// Rounded to 6 decimal places — bookkeeping-grade, not full IEEE.
	Convert(amount float64, from, to string) (float64, error)
}

// MapProvider is the concrete implementation backed by a pinned rate table.
//
// Concurrency: read-only after construction. Safe for use by many goroutines.
type MapProvider struct {
	rates map[string]float64
}

// NewMapProvider constructs a MapProvider from a "FROM->TO": rate map.
// Same-currency pairs are inserted automatically with rate 1.0 so callers
// don't have to declare USD->USD.
//
// If reflexive inverse is missing (e.g. EUR->USD given USD->EUR), the
// inverse is computed automatically. Authors who want asymmetric spreads
// (bid/ask) MUST declare both directions explicitly — explicit beats
// inferred.
func NewMapProvider(pinned map[string]float64) *MapProvider {
	rates := make(map[string]float64, len(pinned)+8)
	for pair, rate := range pinned {
		from, to, ok := splitPair(pair)
		if !ok {
			continue
		}
		key := canonical(from, to)
		rates[key] = rate
	}
	// Auto-derive missing inverses.
	for pair, rate := range pinned {
		from, to, ok := splitPair(pair)
		if !ok {
			continue
		}
		invKey := canonical(to, from)
		if _, exists := rates[invKey]; !exists && rate > 0 {
			rates[invKey] = 1.0 / rate
		}
	}
	// Identity rates so Convert(USD->USD) is well-defined.
	currencies := map[string]struct{}{}
	for pair := range pinned {
		from, to, ok := splitPair(pair)
		if !ok {
			continue
		}
		currencies[from] = struct{}{}
		currencies[to] = struct{}{}
	}
	for c := range currencies {
		rates[canonical(c, c)] = 1.0
	}
	return &MapProvider{rates: rates}
}

// Rate returns the multiplier; same-currency = 1.0 even when not in the table.
func (p *MapProvider) Rate(from, to string) (float64, error) {
	from = strings.ToUpper(strings.TrimSpace(from))
	to = strings.ToUpper(strings.TrimSpace(to))
	if from == "" || to == "" {
		return 0, fmt.Errorf("fx: empty currency code (from=%q, to=%q)", from, to)
	}
	if from == to {
		return 1.0, nil
	}
	if r, ok := p.rates[canonical(from, to)]; ok {
		if r <= 0 {
			return 0, fmt.Errorf("fx: non-positive rate for %s->%s: %v", from, to, r)
		}
		return r, nil
	}
	return 0, fmt.Errorf("%w: %s->%s", ErrUnknownPair, from, to)
}

// Convert applies the rate, returning the converted amount rounded to 6 decimal places.
func (p *MapProvider) Convert(amount float64, from, to string) (float64, error) {
	r, err := p.Rate(from, to)
	if err != nil {
		return 0, err
	}
	return roundTo(amount*r, 6), nil
}

// FromScenario returns a Provider built from scenario.fx.pinned_rates plus
// platform-default rates that scenarios commonly omit. The scenario rates
// always win over the defaults — this is just a kindness so simple scenarios
// don't have to redeclare every pair.
func FromScenario(s *scenario.Scenario) *MapProvider {
	pinned := defaultRates()
	if s != nil {
		for pair, rate := range s.FX.PinnedRates {
			pinned[pair] = rate
		}
	}
	return NewMapProvider(pinned)
}

// defaultRates returns reasonable static USD/EUR/GBP rates. These are used
// when a scenario doesn't pin its own rates. Never mutate this map; copy
// per call.
//
// Rates are deliberately STALE-by-design — load tests run against fixed
// numbers, not live markets. A scenario that wants live rates pins them.
func defaultRates() map[string]float64 {
	return map[string]float64{
		"USD->EUR": 0.92,
		"USD->GBP": 0.79,
		"EUR->GBP": 0.86,
	}
}

func splitPair(pair string) (from, to string, ok bool) {
	pair = strings.TrimSpace(pair)
	const sep = "->"
	idx := strings.Index(pair, sep)
	if idx <= 0 || idx+len(sep) >= len(pair) {
		return "", "", false
	}
	from = strings.ToUpper(pair[:idx])
	to = strings.ToUpper(pair[idx+len(sep):])
	if len(from) != 3 || len(to) != 3 {
		return "", "", false
	}
	return from, to, true
}

func canonical(from, to string) string { return from + "->" + to }

func roundTo(v float64, decimals int) float64 {
	mult := math.Pow10(decimals)
	return math.Round(v*mult) / mult
}
