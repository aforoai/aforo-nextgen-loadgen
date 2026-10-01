package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestMetricUsageStore_RecordAndSnapshot(t *testing.T) {
	s := newMetricUsageStore()
	if s.Snapshot() != nil {
		t.Fatal("empty store must snapshot to nil so run.json omits the field")
	}
	s.Record("t1", "API Calls", 1, "SUCCESS")
	s.Record("t1", "API Calls", 1, "ERROR")
	s.Record("t1", "Tokens", 120, "SUCCESS")
	s.Record("t1", "Tokens", 80, "")
	s.Record("t2", "API Calls", 1, "SUCCESS")
	s.Record("", "API Calls", 1, "SUCCESS") // no tenant → ignored
	s.Record("t1", "", 1, "SUCCESS")        // no metric → ignored

	got := s.Snapshot()
	want := map[string]map[string]MetricUsage{
		"t1": {
			"API Calls": {Events: 2, Quantity: 2, MaxQuantity: 1, ByStatus: map[string]StatusUsage{
				"SUCCESS": {Events: 1, Quantity: 1}, "ERROR": {Events: 1, Quantity: 1}}},
			"Tokens": {Events: 2, Quantity: 200, MaxQuantity: 120, ByStatus: map[string]StatusUsage{
				"SUCCESS": {Events: 1, Quantity: 120}, "": {Events: 1, Quantity: 80}}},
		},
		"t2": {
			"API Calls": {Events: 1, Quantity: 1, MaxQuantity: 1, ByStatus: map[string]StatusUsage{
				"SUCCESS": {Events: 1, Quantity: 1}}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot\n got %+v\nwant %+v", got, want)
	}

	// The snapshot is a copy: later events don't change it.
	s.Record("t1", "Tokens", 500, "SUCCESS")
	if got["t1"]["Tokens"].Events != 2 || got["t1"]["Tokens"].ByStatus["SUCCESS"].Events != 1 {
		t.Errorf("snapshot aliased the live store: %+v", got["t1"]["Tokens"])
	}
}

func TestMergeResults_SumsPerTenantMetric(t *testing.T) {
	a := &RunResult{PerTenantMetric: map[string]map[string]MetricUsage{
		"t1": {"Tokens": {Events: 2, Quantity: 200, MaxQuantity: 120,
			ByStatus: map[string]StatusUsage{"SUCCESS": {Events: 2, Quantity: 200}}}},
	}}
	b := &RunResult{PerTenantMetric: map[string]map[string]MetricUsage{
		"t1": {"Tokens": {Events: 3, Quantity: 900, MaxQuantity: 700,
			ByStatus: map[string]StatusUsage{"SUCCESS": {Events: 1, Quantity: 100}, "ERROR": {Events: 2, Quantity: 800}}}},
		"t2": {"API Calls": {Events: 5, Quantity: 5, MaxQuantity: 1}},
	}}
	merged := mergeResults(Config{Scenario: &scenario.Scenario{Name: "merge"}}, []*RunResult{a, b})
	tok := merged.PerTenantMetric["t1"]["Tokens"]
	if tok.Events != 5 || tok.Quantity != 1100 || tok.MaxQuantity != 700 {
		t.Errorf("merged Tokens = %+v", tok)
	}
	if tok.ByStatus["SUCCESS"] != (StatusUsage{Events: 3, Quantity: 300}) || tok.ByStatus["ERROR"] != (StatusUsage{Events: 2, Quantity: 800}) {
		t.Errorf("merged by_status = %+v", tok.ByStatus)
	}
	if merged.PerTenantMetric["t2"]["API Calls"].Events != 5 {
		t.Errorf("t2 not merged: %+v", merged.PerTenantMetric["t2"])
	}
	// Inputs are not mutated.
	if a.PerTenantMetric["t1"]["Tokens"].Events != 2 {
		t.Errorf("merge mutated its input: %+v", a.PerTenantMetric["t1"]["Tokens"])
	}
	// Partitions without the field merge to nil (field stays absent).
	if m := mergeResults(Config{Scenario: &scenario.Scenario{Name: "merge"}}, []*RunResult{{}, {}}); m.PerTenantMetric != nil {
		t.Errorf("want nil PerTenantMetric, got %+v", m.PerTenantMetric)
	}
}

// A run.json written before per_tenant_metric existed decodes with a nil
// map (the validator's signal to estimate the old way), and a result
// without usage does not emit the key.
func TestRunResult_PerTenantMetricJSONCompat(t *testing.T) {
	var old RunResult
	if err := json.Unmarshal([]byte(`{"run_id":"run-1","per_tenant":{"t1":100}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.PerTenantMetric != nil || old.PerTenant["t1"] != 100 {
		t.Fatalf("old run.json decoded as %+v", old)
	}
	raw, err := json.Marshal(&RunResult{RunID: "run-2"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, present := keys["per_tenant_metric"]; present {
		t.Error("per_tenant_metric must be omitted when empty")
	}
}
