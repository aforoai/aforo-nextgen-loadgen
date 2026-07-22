package scenario

import "fmt"

// Migrate normalizes a loaded Scenario to CurrentSchemaVersion.
//
// For schema version 1 (current) this is a no-op aside from the bounds
// check. Future versions will add upgrade paths here:
//
//	switch s.SchemaVersion {
//	case 1:
//	    upgradeV1toV2(s)
//	    fallthrough
//	case 2:
//	    upgradeV2toV3(s)
//	    fallthrough
//	case CurrentSchemaVersion:
//	    return nil
//	}
//
// Each step mutates the Scenario in place; fallthrough lets a v1 file walk
// all the way up to current. Always preserve the chain — never delete a
// migration step, even after the source version stops appearing in the wild.
func Migrate(s *Scenario) error {
	if s == nil {
		return fmt.Errorf("nil scenario")
	}
	if s.SchemaVersion == 0 {
		return fmt.Errorf("schema_version is required")
	}
	if s.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf(
			"schema_version %d is from a newer release than this build (%d); upgrade aforo-loadgen",
			s.SchemaVersion, CurrentSchemaVersion)
	}
	if s.SchemaVersion < 1 {
		return fmt.Errorf("schema_version %d is unsupported (minimum is 1)", s.SchemaVersion)
	}
	switch s.SchemaVersion {
	case 1:
		upgradeV1toV2(s)
		fallthrough
	case CurrentSchemaVersion:
		return nil
	}
	return nil
}

// upgradeV1toV2 is a no-op walk that just bumps SchemaVersion. The actual
// v1→v2 backward-compat work (backfilling RateCards from legacy top-level
// PricingModel / RateConfig / MetricConfigs / DimensionPricing) happens in
// applyDefaults so both explicit-v2 authors and migrated-from-v1 files
// share the same normalization path — reading `Migrate` and expecting the
// v1 shape to be rewritten in place would violate the "defaults after
// migration" convention.
func upgradeV1toV2(s *Scenario) {
	s.SchemaVersion = CurrentSchemaVersion
}
