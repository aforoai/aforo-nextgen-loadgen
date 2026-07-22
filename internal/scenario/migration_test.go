package scenario

import (
	"strings"
	"testing"
)

func TestMigrate(t *testing.T) {
	cases := []struct {
		name      string
		s         *Scenario
		wantError string // empty = expect nil
	}{
		{
			name:      "nil scenario",
			s:         nil,
			wantError: "nil scenario",
		},
		{
			name:      "missing version",
			s:         &Scenario{},
			wantError: "schema_version is required",
		},
		{
			name:      "negative version",
			s:         &Scenario{SchemaVersion: -1},
			wantError: "is unsupported",
		},
		{
			name:      "newer version",
			s:         &Scenario{SchemaVersion: CurrentSchemaVersion + 1},
			wantError: "newer release",
		},
		{
			name:      "current version",
			s:         &Scenario{SchemaVersion: CurrentSchemaVersion},
			wantError: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Migrate(tc.s)
			if tc.wantError == "" {
				if err != nil {
					t.Errorf("got %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got nil; want error containing %q", tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error %q does not contain %q", err, tc.wantError)
			}
		})
	}
}

func TestCurrentSchemaVersionIsTwo(t *testing.T) {
	// Tripwire: bump this test deliberately when we add schema v3 — the
	// migration chain in Migrate must be updated in the same PR. Also
	// re-check that every /scenarios/*.yaml file has been bumped to the
	// new version (see TestGolden_BuiltInScenariosLoadAndValidate) and
	// that applyDefaults still backfills the old shape into the new one
	// (see TestApplyDefaults_v1RateCardsBackfill).
	if CurrentSchemaVersion != 2 {
		t.Errorf("CurrentSchemaVersion = %d; if you bumped this, update Migrate() with an upgrade path", CurrentSchemaVersion)
	}
}

// TestUpgradeV1toV2_PreservesSchemaShape asserts that a v1 scenario runs
// through Migrate cleanly and comes out with SchemaVersion=2. The actual
// RateCards backfill happens in applyDefaults (not in the migration
// step) — see TestApplyDefaults_v1RateCardsBackfill for that guarantee.
func TestUpgradeV1toV2_PreservesSchemaShape(t *testing.T) {
	s := &Scenario{SchemaVersion: 1}
	if err := Migrate(s); err != nil {
		t.Fatalf("Migrate v1 scenario: %v", err)
	}
	if s.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d; want 2 after Migrate", s.SchemaVersion)
	}
}
