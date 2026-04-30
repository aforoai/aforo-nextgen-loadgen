package server

import (
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/scenarios"
)

// EmbeddedCatalog adapts the bundled scenario YAMLs into the server's
// ScenarioCatalog interface. The list is computed lazily so the cost
// only hits on the first /api/v1/scenarios request.
type EmbeddedCatalog struct {
	cached []ScenarioInfo
	names  map[string]struct{}
}

// NewEmbeddedCatalog parses every built-in scenario YAML up-front and
// caches the lightweight info rows. Scenarios that fail to parse are
// silently skipped — the CLI's `scenarios validate` is the
// authoritative checker; the server tolerates bad rows so a single
// malformed bundle doesn't 500 the whole endpoint.
func NewEmbeddedCatalog() *EmbeddedCatalog {
	c := &EmbeddedCatalog{names: map[string]struct{}{}}
	for _, name := range scenarios.Names() {
		data, err := scenarios.Read(name)
		if err != nil {
			continue
		}
		doc, err := scenario.LoadFromBytes(data)
		if err != nil {
			continue
		}
		s := doc.Scenario
		c.cached = append(c.cached, ScenarioInfo{
			Name:         s.Name,
			Description:  s.Description,
			TargetTPS:    s.TargetTPS,
			DurationSecs: int(s.Duration.Std().Seconds()),
			Tenants:      s.Tenants.Count,
			HighTPS:      s.TargetTPS > HighTPSThreshold,
		})
		c.names[s.Name] = struct{}{}
	}
	return c
}

// List returns a copy so callers can sort/filter without mutating the
// cache.
func (c *EmbeddedCatalog) List() []ScenarioInfo {
	out := make([]ScenarioInfo, len(c.cached))
	copy(out, c.cached)
	return out
}

// Has is the trigger-time membership check.
func (c *EmbeddedCatalog) Has(name string) bool {
	_, ok := c.names[name]
	return ok
}

// Names returns the underlying name set as a slice — used by the
// LocalRunner constructor to whitelist scenario flags.
func (c *EmbeddedCatalog) Names() []string {
	out := make([]string, 0, len(c.names))
	for n := range c.names {
		out = append(out, n)
	}
	return out
}
