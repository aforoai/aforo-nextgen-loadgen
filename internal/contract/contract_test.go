package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These tests exercise the verification logic against synthetic OpenAPI specs
// — they do NOT depend on real backend snapshots. The integration test that
// runs against committed openapi/<service>.json lives in
// internal/seed/contract_test.go (TestBackendContract).

// fixturePath returns a tmp file path used to write a synthetic spec for one
// subtest. We write to disk + go through LoadSpec rather than building the
// Spec struct in memory because that exercises the JSON parsing path too.
func writeSynthSpec(t *testing.T, schemas map[string]map[string]any) string {
	t.Helper()
	props := map[string]any{}
	for name, fields := range schemas {
		propMap := map[string]any{}
		for _, f := range fields {
			propMap[f.(string)] = map[string]string{"type": "string"}
		}
		props[name] = map[string]any{
			"type":       "object",
			"properties": propMap,
		}
	}
	doc := map[string]any{
		"openapi": "3.0.1",
		"info":    map[string]string{"title": "synth", "version": "test"},
		"components": map[string]any{
			"schemas": props,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal synth doc: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "openapi", "synth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return dir
}

type sampleResponseGood struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type sampleResponseBad struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ExternalID string `json:"externalId"` // intentionally NOT in schema below
}

type sampleRequestPhantom struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"` // phantom — server drops silently
}

func TestVerify_PerfectMatch_AllFieldsPresent(t *testing.T) {
	repoRoot := writeSynthSpec(t, map[string]map[string]any{
		"SampleResponse": {
			"a": "id",
			"b": "name",
			"c": "email",
		},
	})
	spec, err := LoadSpec(repoRoot, "synth")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	result := Verify(spec, Entry{
		Service: "synth", SchemaName: "SampleResponse",
		StructType: reflect.TypeOf(sampleResponseGood{}),
		Direction:  DirResponse, Expectation: PerfectMatch,
	})
	if !result.OK {
		t.Errorf("expected OK, got mismatches: %v", result.Mismatches)
	}
	if len(result.Mismatches) != 0 {
		t.Errorf("expected zero mismatches, got %v", result.Mismatches)
	}
}

func TestVerify_PerfectMatch_MissingFieldFails(t *testing.T) {
	repoRoot := writeSynthSpec(t, map[string]map[string]any{
		"SampleResponse": {
			"a": "id",
			"b": "name",
		},
	})
	spec, err := LoadSpec(repoRoot, "synth")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	result := Verify(spec, Entry{
		Service: "synth", SchemaName: "SampleResponse",
		StructType: reflect.TypeOf(sampleResponseBad{}),
		Direction:  DirResponse, Expectation: PerfectMatch,
	})
	if result.OK {
		t.Errorf("expected verify to fail on missing externalId field")
	}
	if len(result.Mismatches) != 1 {
		t.Errorf("expected exactly 1 mismatch, got %d: %v", len(result.Mismatches), result.Mismatches)
	}
	if len(result.Mismatches) > 0 && !strings.Contains(result.Mismatches[0], "externalId") {
		t.Errorf("expected mismatch to name externalId, got %q", result.Mismatches[0])
	}
}

func TestVerify_AllowPhantomRequest_TolerantOfMissingFields(t *testing.T) {
	repoRoot := writeSynthSpec(t, map[string]map[string]any{
		"SampleRequest": {
			"a": "name",
		},
	})
	spec, err := LoadSpec(repoRoot, "synth")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	result := Verify(spec, Entry{
		Service: "synth", SchemaName: "SampleRequest",
		StructType: reflect.TypeOf(sampleRequestPhantom{}),
		Direction:  DirRequest, Expectation: AllowPhantomRequest,
	})
	if !result.OK {
		t.Errorf("expected phantom request to pass, got mismatches: %v", result.Mismatches)
	}
	// The phantom field should be reported as skipped.
	foundNote := false
	for _, f := range result.SkippedFields {
		if f == "note" {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Errorf("expected 'note' in SkippedFields, got %v", result.SkippedFields)
	}
}

func TestVerify_MissingSchemaFails(t *testing.T) {
	repoRoot := writeSynthSpec(t, map[string]map[string]any{
		"DifferentSchema": {
			"a": "id",
		},
	})
	spec, err := LoadSpec(repoRoot, "synth")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	result := Verify(spec, Entry{
		Service: "synth", SchemaName: "NonexistentSchema",
		StructType: reflect.TypeOf(sampleResponseGood{}),
		Direction:  DirResponse, Expectation: PerfectMatch,
	})
	if result.OK {
		t.Errorf("expected verify to fail when schema is missing")
	}
}

func TestVerify_SkipReturnsOKWithoutChecking(t *testing.T) {
	// Even though the schema doesn't carry externalId, Skip should
	// short-circuit the verification.
	repoRoot := writeSynthSpec(t, map[string]map[string]any{
		"SampleResponse": {"a": "id"},
	})
	spec, err := LoadSpec(repoRoot, "synth")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	result := Verify(spec, Entry{
		Service: "synth", SchemaName: "SampleResponse",
		StructType: reflect.TypeOf(sampleResponseBad{}),
		Direction:  DirResponse, Expectation: PerfectMatch,
		Skip: "intentionally skipped for this regression",
	})
	if !result.OK {
		t.Errorf("Skip should short-circuit and return OK, got mismatches: %v", result.Mismatches)
	}
}

func TestExtractJSONFieldNames_HandlesDashAndOmitempty(t *testing.T) {
	type sample struct {
		Kept      string `json:"kept"`
		KeptOmit  string `json:"keptOmit,omitempty"`
		Dropped   string `json:"-"`
		NoTag     string
		EmptyTag  string `json:""`
		FieldOpts string `json:",string"` // tag without name — should be skipped
	}
	got := extractJSONFieldNames(reflect.TypeOf(sample{}))
	want := []string{"kept", "keptOmit"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
