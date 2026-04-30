package runner

import (
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestPartitionManifest_DeterministicDistribution checks that the same
// manifest+N always partitions tenants the same way — required for
// reproducible runs across machines.
func TestPartitionManifest_DeterministicDistribution(t *testing.T) {
	m := makeTestManifest(t, 30)
	c1, _ := partitionManifest(m, 4)
	c2, _ := partitionManifest(m, 4)
	for i := range c1 {
		if len(c1[i].Tenants) != len(c2[i].Tenants) {
			t.Errorf("partition %d: count differs across runs (%d vs %d)", i, len(c1[i].Tenants), len(c2[i].Tenants))
		}
		for j := range c1[i].Tenants {
			if c1[i].Tenants[j].TenantID != c2[i].Tenants[j].TenantID {
				t.Errorf("partition %d row %d: tenant differs (%s vs %s)",
					i, j, c1[i].Tenants[j].TenantID, c2[i].Tenants[j].TenantID)
			}
		}
	}
}

// TestPartitionManifest_PreservesAllTenants — no tenant should be lost
// or duplicated in the partition split.
func TestPartitionManifest_PreservesAllTenants(t *testing.T) {
	m := makeTestManifest(t, 30)
	chunks, err := partitionManifest(m, 4)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	seen := map[string]int{}
	total := 0
	for _, c := range chunks {
		for _, ten := range c.Tenants {
			seen[ten.TenantID]++
			total++
		}
	}
	if total != len(m.Tenants) {
		t.Errorf("total tenants after partition: got %d want %d", total, len(m.Tenants))
	}
	for tid, count := range seen {
		if count != 1 {
			t.Errorf("tenant %s appears in %d partitions (expected 1)", tid, count)
		}
	}
}

// TestSplitTPS_RoundsRobinRemainder validates per-partition TPS division.
func TestSplitTPS_RoundsRobinRemainder(t *testing.T) {
	cases := []struct {
		total int
		n     int
		want  []int
	}{
		{12, 4, []int{3, 3, 3, 3}},
		{10, 3, []int{4, 3, 3}},
		{1, 4, []int{1, 1, 1, 1}}, // floor at 1 per partition
		{100, 4, []int{25, 25, 25, 25}},
	}
	for _, tc := range cases {
		got := splitTPS(tc.total, tc.n)
		if !equalIntSlices(got, tc.want) {
			t.Errorf("splitTPS(%d, %d) = %v, want %v", tc.total, tc.n, got, tc.want)
		}
	}
}

// TestMergeResults_AccumulatesCounters ensures the merge sums per-
// partition counters exactly. This is the core acceptance check:
// "--workers 4 vs --workers 1 same total events."
func TestMergeResults_AccumulatesCounters(t *testing.T) {
	cfg := Config{
		Scenario: &scenario.Scenario{Name: "test", TargetTPS: 100},
		Target:   aforo.Target{Name: "test"},
	}
	parts := []*RunResult{
		{
			EventsGenerated: 100, EventsSucceeded: 95, ClientErrors: 3, ServerErrors: 2,
			PerTenant: map[string]int64{"a": 60, "b": 40},
		},
		{
			EventsGenerated: 100, EventsSucceeded: 98, ClientErrors: 1, ServerErrors: 1,
			PerTenant: map[string]int64{"c": 70, "d": 30},
		},
	}
	merged := mergeResults(cfg, parts)
	if got, want := merged.EventsGenerated, int64(200); got != want {
		t.Errorf("EventsGenerated: got %d want %d", got, want)
	}
	if got, want := merged.EventsSucceeded, int64(193); got != want {
		t.Errorf("EventsSucceeded: got %d want %d", got, want)
	}
	if got, want := merged.PerTenant["a"], int64(60); got != want {
		t.Errorf("per-tenant a: got %d want %d", got, want)
	}
	if got, want := merged.PerTenant["c"], int64(70); got != want {
		t.Errorf("per-tenant c: got %d want %d", got, want)
	}
}

// makeTestManifest produces a manifest with N synthetic tenants for the
// partition tests. Helper.
func makeTestManifest(t *testing.T, n int) *seed.Manifest {
	t.Helper()
	m := seed.NewManifest("run-test", "local", "test", time.Now())
	for i := 0; i < n; i++ {
		m.AppendTenant(seed.ManifestTenant{
			TenantID:   "tenant-id-" + intToStr(i),
			ExternalID: "ext-" + intToStr(i),
			Archetype:  "test-arch",
		})
	}
	m.Finalize()
	return m
}

// intToStr — avoid pulling fmt for the helper.
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + (i % 10))}, digits...)
		i /= 10
	}
	return string(digits)
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

