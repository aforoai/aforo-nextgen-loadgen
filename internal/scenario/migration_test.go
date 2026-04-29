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

func TestCurrentSchemaVersionIsOne(t *testing.T) {
	// Tripwire: bump this test deliberately when we add schema v2 — the
	// migration chain in Migrate must be updated in the same PR.
	if CurrentSchemaVersion != 1 {
		t.Errorf("CurrentSchemaVersion = %d; if you bumped this, update Migrate() with an upgrade path", CurrentSchemaVersion)
	}
}
