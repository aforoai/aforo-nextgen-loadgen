package runner

import (
	"math"
	"sort"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
)

// perTenantHistKey indexes the per-tenant slice (and optional per-path /
// per-product breakdown) of HDR histograms.
//
// Memory footprint at the spec's "50 tenants × 4 paths × 4 product types":
//   - 50 × 4 × 4 = 800 histograms
//   - Each HDR histogram (1us..30s, 3 sig figs) ≈ 150 KiB
//   - Total ≈ 120 MiB resident
//
// The spec's documented ceiling is "~400MB"; the actual cost is well below
// that. The runner exposes Stats() that surfaces this so a sanity check on
// 24h runs can flag unexpected growth.
type perTenantHistKey struct {
	TenantID    string
	Path        string // "" = all paths rolled up
	ProductType string // "" = all product types rolled up
}

// perTenantStore holds the per-tenant HDR histograms + lazy per-path and
// per-product breakdowns. Updates funnel through Record().
type perTenantStore struct {
	mu sync.Mutex
	// Primary index: per-tenant aggregate. This is the histogram the
	// PER_TENANT_P99_FAIRNESS assertion reads.
	perTenant map[string]*hdrhistogram.Histogram
	// Optional secondary indices for diagnostic reporting. The keyed shape
	// follows the spec's tenant×path×product breakout, but only entries
	// that actually receive traffic are allocated.
	perTenantByPath        map[string]map[string]*hdrhistogram.Histogram
	perTenantByProductType map[string]map[string]*hdrhistogram.Histogram
	// HDR construction parameters — kept on the store so per-key lazy
	// allocation uses identical bucketing.
	low int64
	hi  int64
	sig int
}

// newPerTenantStore returns a store with conservative HDR parameters
// (1us..30s, 3 sig figs) sized for ingest p99 latencies up to 30 seconds.
func newPerTenantStore() *perTenantStore {
	return &perTenantStore{
		perTenant:              map[string]*hdrhistogram.Histogram{},
		perTenantByPath:        map[string]map[string]*hdrhistogram.Histogram{},
		perTenantByProductType: map[string]map[string]*hdrhistogram.Histogram{},
		low:                    1,
		hi:                     (30 * time.Second).Microseconds(),
		sig:                    3,
	}
}

// Record adds one observation to the relevant histograms. tenant/path/pt
// must be non-empty for the per-tenant primary; per-path and per-product
// indices skip when their column is empty.
func (s *perTenantStore) Record(tenant, path, pt string, latency time.Duration) {
	if tenant == "" {
		return
	}
	v := latency.Microseconds()
	if v < s.low {
		v = s.low
	}
	if v > s.hi {
		v = s.hi
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.perTenant[tenant]
	if h == nil {
		h = hdrhistogram.New(s.low, s.hi, s.sig)
		s.perTenant[tenant] = h
	}
	_ = h.RecordValue(v)

	if path != "" {
		bucket := s.perTenantByPath[tenant]
		if bucket == nil {
			bucket = map[string]*hdrhistogram.Histogram{}
			s.perTenantByPath[tenant] = bucket
		}
		ph := bucket[path]
		if ph == nil {
			ph = hdrhistogram.New(s.low, s.hi, s.sig)
			bucket[path] = ph
		}
		_ = ph.RecordValue(v)
	}

	if pt != "" {
		bucket := s.perTenantByProductType[tenant]
		if bucket == nil {
			bucket = map[string]*hdrhistogram.Histogram{}
			s.perTenantByProductType[tenant] = bucket
		}
		ph := bucket[pt]
		if ph == nil {
			ph = hdrhistogram.New(s.low, s.hi, s.sig)
			bucket[pt] = ph
		}
		_ = ph.RecordValue(v)
	}
}

// PerTenantP99 returns the p99 latency in milliseconds for each tenant
// that has at least one recorded observation. Sorted by tenant id for
// deterministic output.
func (s *perTenantStore) PerTenantP99() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64, len(s.perTenant))
	for tenant, h := range s.perTenant {
		if h.TotalCount() == 0 {
			continue
		}
		out[tenant] = float64(h.ValueAtQuantile(99.0)) / 1000.0
	}
	return out
}

// FairnessReport summarizes the per-tenant p99 distribution.
//
// StddevPct = stddev / mean — the spec's PER_TENANT_P99_FAIRNESS assertion
// thresholds against this ratio. <0.30 means "no tenant is more than ~30%
// off the median tenant's p99", indicating noisy-neighbor isolation works.
type FairnessReport struct {
	TenantsObserved int                `json:"tenants_observed"`
	MeanP99Ms       float64            `json:"mean_p99_ms"`
	StddevP99Ms     float64            `json:"stddev_p99_ms"`
	StddevPct       float64            `json:"stddev_pct"`
	MinP99Ms        float64            `json:"min_p99_ms"`
	MaxP99Ms        float64            `json:"max_p99_ms"`
	WorstOffenders  []TenantP99Outlier `json:"worst_offenders,omitempty"`
}

// TenantP99Outlier is one entry in the FairnessReport.WorstOffenders list —
// the tenants whose p99 deviates most from the mean. Used by the HTML
// report's per-tenant table sorting and by the validate oracle.
type TenantP99Outlier struct {
	TenantID  string  `json:"tenant_id"`
	P99Ms     float64 `json:"p99_ms"`
	DeltaFrac float64 `json:"delta_frac"` // (p99 - mean) / mean
}

// FairnessReport produces the summary needed by the
// PER_TENANT_P99_FAIRNESS check.
func (s *perTenantStore) FairnessReport() FairnessReport {
	p99 := s.PerTenantP99()
	if len(p99) == 0 {
		return FairnessReport{}
	}
	values := make([]float64, 0, len(p99))
	for _, v := range p99 {
		values = append(values, v)
	}
	sort.Float64s(values)
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, v := range values {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(values))
	stddev := 0.0
	if variance > 0 {
		stddev = math.Sqrt(variance)
	}
	stddevPct := 0.0
	if mean > 0 {
		stddevPct = stddev / mean
	}
	report := FairnessReport{
		TenantsObserved: len(values),
		MeanP99Ms:       mean,
		StddevP99Ms:     stddev,
		StddevPct:       stddevPct,
		MinP99Ms:        values[0],
		MaxP99Ms:        values[len(values)-1],
	}
	// Worst offenders: top 5 tenants by deviation from mean (only when
	// the population is large enough for the comparison to be meaningful).
	if len(p99) >= 4 {
		type entry struct {
			tenant string
			delta  float64
			p99    float64
		}
		entries := make([]entry, 0, len(p99))
		for k, v := range p99 {
			delta := 0.0
			if mean > 0 {
				delta = math.Abs(v-mean) / mean
			}
			entries = append(entries, entry{tenant: k, delta: delta, p99: v})
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].delta > entries[j].delta })
		max := 5
		if len(entries) < max {
			max = len(entries)
		}
		for _, e := range entries[:max] {
			frac := 0.0
			if mean > 0 {
				frac = (e.p99 - mean) / mean
			}
			report.WorstOffenders = append(report.WorstOffenders, TenantP99Outlier{
				TenantID:  e.tenant,
				P99Ms:     e.p99,
				DeltaFrac: frac,
			})
		}
	}
	return report
}

// PerTenantPathBreakdown returns the per-tenant per-path latency p99 in
// milliseconds. Sparse — only allocated buckets appear. Used by HTML report.
func (s *perTenantStore) PerTenantPathBreakdown() map[string]map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]float64{}
	for tenant, bucket := range s.perTenantByPath {
		row := map[string]float64{}
		for path, h := range bucket {
			if h.TotalCount() == 0 {
				continue
			}
			row[path] = float64(h.ValueAtQuantile(99.0)) / 1000.0
		}
		if len(row) > 0 {
			out[tenant] = row
		}
	}
	return out
}

// MemoryFootprint reports the rough byte cost of the histograms held by
// the store. Each histogram is ~150KiB at our parameters; we report the
// product so the runner can flag accidental growth.
func (s *perTenantStore) MemoryFootprint() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	const perHist = 150 * 1024
	count := int64(len(s.perTenant))
	for _, b := range s.perTenantByPath {
		count += int64(len(b))
	}
	for _, b := range s.perTenantByProductType {
		count += int64(len(b))
	}
	return count * perHist
}
