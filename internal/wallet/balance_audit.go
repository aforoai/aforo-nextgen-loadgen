package wallet

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// AuditRow is one row in the per-customer wallet audit summary written
// alongside wallet_audit.jsonl. Used by Check 17 to assert:
//
//	final_balance ≈ initial_balance − sum(committed_charges)
//	         + sum(released_holds)
//	         + sum(refunds)         [if credit_notes touched the wallet]
//
// Tolerance is a small absolute USD value; rounding noise is ≤ $0.01.
type AuditRow struct {
	TenantID         string  `json:"tenant_id"`
	CustomerID       string  `json:"customer_id"`
	WalletID         string  `json:"wallet_id"`
	Currency         string  `json:"currency"`
	InitialBalance   float64 `json:"initial_balance"`
	FinalBalance     float64 `json:"final_balance"`
	HeldAtRunEnd     float64 `json:"held_at_run_end"`
	HeldAtFinal      float64 `json:"held_at_final"`
	HoldsCreated     int     `json:"holds_created"`
	HoldsReleased    int     `json:"holds_released"`
	HoldsSettled     int     `json:"holds_settled"`
	OutstandingHolds int     `json:"outstanding_holds"`
	BalanceDelta     float64 `json:"balance_delta"`
	HoldsLEBalance   bool    `json:"holds_le_initial_balance"` // sum of holds ≤ initial balance — invariant
	Notes            string  `json:"notes,omitempty"`
}

// LoadAudit reads wallet_audit.jsonl from a run output directory and
// reconstructs the per-customer summary. A nil error with an empty slice
// means the file isn't present (run had no PREPAID/HYBRID customers).
func LoadAudit(dir string) ([]AuditRow, error) {
	path := filepath.Join(dir, "wallet_audit.jsonl")
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	type wireRow struct {
		Type           string  `json:"type"`
		TenantID       string  `json:"tenant_id"`
		CustomerID     string  `json:"customer_id"`
		WalletID       string  `json:"wallet_id"`
		Currency       string  `json:"currency"`
		BalanceUSD     float64 `json:"balance_usd"`
		HeldUSD        float64 `json:"held_usd"`
		HoldUSD        float64 `json:"hold_usd"`
		Phase          string  `json:"phase"`
		State          string  `json:"state"`
		HoldsActive    int     `json:"holds_active"`
	}
	type bucket struct {
		row              AuditRow
		seen             map[string]string // hold id → state
		maxHeldDuringRun float64
	}
	byCust := map[string]*bucket{}
	for {
		var r wireRow
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		key := r.TenantID + "/" + r.CustomerID
		b, ok := byCust[key]
		if !ok {
			b = &bucket{
				row: AuditRow{
					TenantID: r.TenantID, CustomerID: r.CustomerID,
					WalletID: r.WalletID, Currency: r.Currency,
				},
				seen: map[string]string{},
			}
			byCust[key] = b
		}
		switch r.Type {
		case "snapshot":
			switch r.Phase {
			case "pre":
				b.row.InitialBalance = r.BalanceUSD
				b.row.WalletID = r.WalletID
				if r.Currency != "" {
					b.row.Currency = r.Currency
				}
			case "post":
				b.row.HeldAtRunEnd = r.HeldUSD
			case "post-expiry":
				b.row.FinalBalance = r.BalanceUSD
				b.row.HeldAtFinal = r.HeldUSD
				b.row.OutstandingHolds = r.HoldsActive
			}
			if r.HeldUSD > b.maxHeldDuringRun {
				b.maxHeldDuringRun = r.HeldUSD
			}
		case "hold_event":
			// Use a stable hold id key — events without an id are skipped
			// to avoid double-counting transient races.
			holdID := r.WalletID + "/" + r.State + "/" + fmt.Sprintf("%v", r.HoldUSD)
			prior, ok := b.seen[holdID]
			b.seen[holdID] = r.State
			if !ok {
				b.row.HoldsCreated++
			}
			switch r.State {
			case "RELEASED":
				if prior != "RELEASED" {
					b.row.HoldsReleased++
				}
			case "SETTLED":
				if prior != "SETTLED" {
					b.row.HoldsSettled++
				}
			}
		}
	}
	out := make([]AuditRow, 0, len(byCust))
	for _, b := range byCust {
		row := b.row
		row.BalanceDelta = round2(row.FinalBalance - row.InitialBalance)
		// Invariant: sum of pending holds at any point ≤ initial balance.
		row.HoldsLEBalance = b.maxHeldDuringRun <= row.InitialBalance+0.01
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
	return out, nil
}

// Reconcile asserts the balance arithmetic for one row, given the run's
// committed charges (sum of invoices the wallet covered).
//
//   expected_final = initial - committedCharges + refunds
//   |actual_final - expected_final| ≤ tolerance ?
//
// Returns true with empty reason on PASS; false with an actionable reason
// on FAIL.
func Reconcile(row AuditRow, committedCharges, refunds, tolerance float64) (bool, string) {
	expected := round2(row.InitialBalance - committedCharges + refunds)
	delta := math.Abs(row.FinalBalance - expected)
	if tolerance <= 0 {
		tolerance = 0.01
	}
	if delta <= tolerance {
		return true, ""
	}
	return false, fmt.Sprintf(
		"final_balance=%.2f != expected=%.2f (initial %.2f − committed %.2f + refunds %.2f), |delta|=%.4f > tol %.2f",
		row.FinalBalance, expected, row.InitialBalance, committedCharges, refunds, delta, tolerance,
	)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
