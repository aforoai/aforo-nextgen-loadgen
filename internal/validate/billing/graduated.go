package billing

// graduatedCharge implements GRADUATED — each band charges only for the
// events that fall within it. Tiers are processed bottom-up; the running
// "from" pointer advances as bands fill.
//
// Band semantics: tier.UpToUnits is the inclusive upper bound of the band.
// The last band typically uses math.MaxInt64 for "and everything after".
//
// Example tiers [up_to=1000@$0.001, up_to=10000@$0.0008, up_to=Inf@$0.0005]:
//
//	events=15000 → 1000×0.001 + 9000×0.0008 + 5000×0.0005 = 1.00+7.20+2.50 = 10.70
//
// Returned breakdown is one row per band that contributed events; bands
// the volume didn't reach are omitted.
func graduatedCharge(events int64, raw []Tier) (float64, []TierCharge) {
	if events <= 0 || len(raw) == 0 {
		return 0, nil
	}
	tiers := SortTiersAscending(raw)

	var total float64
	var from int64 = 0
	out := make([]TierCharge, 0, len(tiers))

	for _, t := range tiers {
		if events <= from {
			break
		}
		bandTop := t.UpToUnits
		if bandTop <= from {
			// Authoring error: tier upper-bound below the running from
			// pointer. Skip the band rather than emit a negative count.
			continue
		}
		bandSize := bandTop - from
		used := bandSize
		if events-from < bandSize {
			used = events - from
		}
		charge := float64(used) * t.UnitPrice
		if used > 0 {
			charge += t.FlatFee // band-level flat fee (rare but supported)
		}
		out = append(out, TierCharge{
			From:      from,
			To:        from + used,
			Units:     used,
			UnitPrice: t.UnitPrice,
			FlatFee:   t.FlatFee,
			Charge:    charge,
		})
		total += charge
		from = bandTop
	}
	return total, out
}
