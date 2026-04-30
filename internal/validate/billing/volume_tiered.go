package billing

// volumeTieredCharge implements VOLUME_TIERED — the entire volume is
// charged at the tier where the total falls. Unlike GRADUATED (each band
// charges only its own events), VOLUME applies one rate to all events.
//
// Example tiers [up_to=1000@$0.001, up_to=10000@$0.0008, up_to=Inf@$0.0005]:
//
//	events=500   → 500   × 0.001  + flat    (lands in band 1)
//	events=1500  → 1500  × 0.0008 + flat    (lands in band 2)
//	events=15000 → 15000 × 0.0005 + flat    (lands in band 3)
//
// Returned breakdown is one row — the qualifying band — for diagnostics.
func volumeTieredCharge(events int64, raw []Tier) (float64, []TierCharge) {
	if events <= 0 || len(raw) == 0 {
		return 0, nil
	}
	tiers := SortTiersAscending(raw)

	for _, t := range tiers {
		if events <= t.UpToUnits {
			charge := float64(events)*t.UnitPrice + t.FlatFee
			return charge, []TierCharge{
				{
					From:      0,
					To:        events,
					Units:     events,
					UnitPrice: t.UnitPrice,
					FlatFee:   t.FlatFee,
					Charge:    charge,
				},
			}
		}
	}
	// events exceeds every tier's upper bound — fall through to the last
	// tier, charging at its rate. This is the "uncapped" interpretation.
	last := tiers[len(tiers)-1]
	charge := float64(events)*last.UnitPrice + last.FlatFee
	return charge, []TierCharge{
		{
			From:      0,
			To:        events,
			Units:     events,
			UnitPrice: last.UnitPrice,
			FlatFee:   last.FlatFee,
			Charge:    charge,
		},
	}
}
