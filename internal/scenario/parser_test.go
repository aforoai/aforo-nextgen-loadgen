package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalValid = `
schema_version: 1
name: minimal
target_tps: 50
duration: 60s
tenants:
  count: 1
  archetypes:
    - name: a
      weight: 1.0
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 5
      subscription_state_mix: { ACTIVE: 1.0 }
      rate_config:
        per_unit_rate_usd: 0.001
`

func TestLoadFromBytes_Minimal(t *testing.T) {
	doc, err := LoadFromBytes([]byte(minimalValid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc == nil || doc.Scenario == nil {
		t.Fatalf("nil document")
	}
	if doc.Root == nil {
		t.Fatalf("root yaml node not retained")
	}
	if doc.Scenario.Name != "minimal" {
		t.Errorf("Name = %q; want %q", doc.Scenario.Name, "minimal")
	}
	if doc.Scenario.Duration.Std() != 60*time.Second {
		t.Errorf("Duration = %v; want 60s", doc.Scenario.Duration.Std())
	}
	if errs := Validate(doc); errs.HasErrors() {
		t.Fatalf("minimal scenario should validate clean; got: %s", errs.Error())
	}
}

func TestLoadFromBytes_EmptyInput(t *testing.T) {
	if _, err := LoadFromBytes(nil); err == nil {
		t.Errorf("nil bytes: want error, got nil")
	}
	if _, err := LoadFromBytes([]byte("   \n  \t\n")); err == nil {
		t.Errorf("whitespace-only bytes: want error, got nil")
	}
}

func TestLoadFromBytes_RejectsUnknownField(t *testing.T) {
	bad := minimalValid + "\nbogus_top_level: 1\n"
	_, err := LoadFromBytes([]byte(bad))
	if err == nil {
		t.Fatalf("want error on unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "bogus_top_level") {
		t.Errorf("error %q should mention the unknown field", err)
	}
}

func TestLoadFromBytes_RejectsInvalidDuration(t *testing.T) {
	bad := strings.ReplaceAll(minimalValid, "duration: 60s", "duration: not-a-duration")
	_, err := LoadFromBytes([]byte(bad))
	if err == nil {
		t.Fatalf("want error on invalid duration, got nil")
	}
}

func TestLoadFromFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tmp.yaml")
	if err := os.WriteFile(path, []byte(minimalValid), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	doc, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if doc.Path != path {
		t.Errorf("Document.Path = %q; want %q", doc.Path, path)
	}
	if doc.Scenario.Name != "minimal" {
		t.Errorf("Name = %q", doc.Scenario.Name)
	}
}

func TestLoadFromFile_Missing(t *testing.T) {
	_, err := LoadFromFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Errorf("want error on missing file, got nil")
	}
}

func TestDuration_MarshalRoundTrip(t *testing.T) {
	cases := []time.Duration{
		60 * time.Second,
		5 * time.Minute,
		2 * time.Hour,
		7 * 24 * time.Hour,
	}
	for _, want := range cases {
		d := Duration(want)
		v, err := d.MarshalYAML()
		if err != nil {
			t.Fatalf("MarshalYAML: %v", err)
		}
		s, ok := v.(string)
		if !ok {
			t.Fatalf("MarshalYAML returned %T; want string", v)
		}
		var parsed Duration
		// Round-trip: parse the marshaled string back.
		got, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", s, err)
		}
		parsed = Duration(got)
		if parsed.Std() != want {
			t.Errorf("round-trip mismatch: got %v; want %v", parsed.Std(), want)
		}
	}
}

func TestIsIndexSegment(t *testing.T) {
	good := []string{"[0]", "[1]", "[42]", "[1000]"}
	bad := []string{"", "[", "]", "[]", "[a]", "[01a]", "name", "[1.2]"}
	for _, s := range good {
		if !isIndexSegment(s) {
			t.Errorf("isIndexSegment(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if isIndexSegment(s) {
			t.Errorf("isIndexSegment(%q) = true; want false", s)
		}
	}
}

func TestParseIndexSegment(t *testing.T) {
	cases := map[string]int{"[0]": 0, "[1]": 1, "[42]": 42, "[1000]": 1000}
	for in, want := range cases {
		if got := parseIndexSegment(in); got != want {
			t.Errorf("parseIndexSegment(%q) = %d; want %d", in, got, want)
		}
	}
}
