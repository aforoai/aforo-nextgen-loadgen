package aforo

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

// AllowProductionEnv is the environment variable that, set to
// AllowProductionValue, lets a traffic-generating command run against a
// target whose URLs resolve to production hosts. The value is deliberately
// a phrase, not "true", so it cannot be switched on by a generic
// "set every flag to true" script.
const (
	AllowProductionEnv   = "AFORO_LOADGEN_ALLOW_PRODUCTION"
	AllowProductionValue = "i-understand-this-writes-to-production"
)

// productionDomain is the public domain every production Aforo service is
// served under (CLAUDE.md "Quick Reference — Ports & Production URLs").
const productionDomain = "aforo.ai"

// Destination is one URL a command may send traffic to, labelled with where
// the URL came from so a refusal can tell the operator what to change.
type Destination struct {
	// Source names the origin of the URL, e.g. "target usage-ingestor" or
	// "ai_agent_rest (AFORO_LOADGEN_INGEST_URL)".
	Source string
	URL    string
}

// DestinationSource returns extra destinations that are not part of a
// Target — today the drivers that read their endpoint from an environment
// variable. Sources are registered once at init time (see
// RegisterDestinationSource) so every command that calls GuardProduction
// checks them without each call site having to remember.
type DestinationSource func() []Destination

var (
	destSourcesMu sync.RWMutex
	destSources   []DestinationSource
)

// RegisterDestinationSource adds a source of extra destination URLs that
// GuardProduction checks alongside the target's own URLs. Intended to be
// called from package init functions.
func RegisterDestinationSource(src DestinationSource) {
	if src == nil {
		return
	}
	destSourcesMu.Lock()
	defer destSourcesMu.Unlock()
	destSources = append(destSources, src)
}

// registeredDestinations returns the destinations from every registered
// source, evaluated now (environment variables are read at call time).
func registeredDestinations() []Destination {
	destSourcesMu.RLock()
	srcs := append([]DestinationSource(nil), destSources...)
	destSourcesMu.RUnlock()
	var out []Destination
	for _, src := range srcs {
		out = append(out, src()...)
	}
	return out
}

// NormalizeHost returns the lower-cased host of raw with any port and any
// trailing dots removed. raw may be a full URL ("https://x.aforo.ai./v1")
// or a bare authority ("x.aforo.ai:8443"). Returns "" when no host can be
// extracted.
//
// The trailing-dot strip matters: "usage-ingestor.aforo.ai." is the
// fully-qualified form of the same name, resolves to the same address, and
// must not slip past a suffix comparison.
func NormalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if (err != nil || u.Host == "") && !strings.Contains(raw, "://") {
		// Bare authority ("host:port" or "host") — url.Parse reads the
		// host as a scheme or a path. Retry as a scheme-relative URL.
		u, err = url.Parse("//" + raw)
		if err != nil || u.Host == "" {
			return ""
		}
	}
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimRight(host, ".")
	return host
}

// IsProductionHost reports whether host (raw or already normalized) is the
// production domain or a subdomain of it.
func IsProductionHost(host string) bool {
	h := strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return false
	}
	return h == productionDomain || strings.HasSuffix(h, "."+productionDomain)
}

// ProductionHosts returns the sorted, de-duplicated hosts in this target
// that are production Aforo hosts (aforo.ai or any subdomain of it).
//
// Why this exists: the predefined "staging" target, and the "ci" target when
// no AFORO_CI_* override is exported, resolve to the same *.aforo.ai hosts
// as "prod" — there is no separate staging deployment. The June 2026 e2e
// runs recorded as target=staging (runs/e2e-*-2026060*) therefore seeded
// tenants and posted events to production. The guard makes that impossible
// without an explicit acknowledgement.
//
// Only the target's own URLs are considered here; GuardProduction also
// checks registered destinations (driver environment URLs).
func (t Target) ProductionHosts() []string {
	hosts := productionHosts(t.destinations())
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.host)
	}
	return out
}

// destinations lists the target's own URLs as Destinations.
func (t Target) destinations() []Destination {
	out := make([]Destination, 0, len(t.URLs))
	for svc, raw := range t.URLs {
		out = append(out, Destination{Source: "target " + string(svc), URL: raw})
	}
	return out
}

// EffectiveDestinations returns every URL a run against t may send traffic
// to: the target's own URLs plus every registered destination source.
func (t Target) EffectiveDestinations() []Destination {
	return append(t.destinations(), registeredDestinations()...)
}

type prodHost struct {
	host    string
	sources []string
}

// productionHosts returns the production hosts among dests, sorted by host,
// each with the sorted sources that referenced it.
func productionHosts(dests []Destination) []prodHost {
	bySource := map[string]map[string]struct{}{}
	for _, d := range dests {
		host := NormalizeHost(d.URL)
		if !IsProductionHost(host) {
			continue
		}
		if bySource[host] == nil {
			bySource[host] = map[string]struct{}{}
		}
		bySource[host][d.Source] = struct{}{}
	}
	out := make([]prodHost, 0, len(bySource))
	for host, srcs := range bySource {
		ph := prodHost{host: host}
		for s := range srcs {
			ph.sources = append(ph.sources, s)
		}
		sort.Strings(ph.sources)
		out = append(out, ph)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].host < out[j].host })
	return out
}

// GuardProduction returns an error when any URL a run will send to — the
// target's URLs or a registered destination such as a driver's env-var
// endpoint — is a production host and the caller has not acknowledged it,
// either with allowFlag (the --allow-production CLI flag) or with
// AllowProductionEnv set to AllowProductionValue. Commands that send no
// traffic (doctor, dry runs) should not call it.
func GuardProduction(t Target, allowFlag bool) error {
	return GuardProductionDestinations(t.Name, t.EffectiveDestinations(), allowFlag)
}

// GuardProductionDestinations is GuardProduction over an explicit list of
// destinations. label names the target in the error message.
func GuardProductionDestinations(label string, dests []Destination, allowFlag bool) error {
	hosts := productionHosts(dests)
	if len(hosts) == 0 {
		return nil
	}
	if allowFlag || os.Getenv(AllowProductionEnv) == AllowProductionValue {
		return nil
	}
	names := make([]string, 0, len(hosts))
	details := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.host)
		details = append(details, fmt.Sprintf("    %s  (from %s)", h.host, strings.Join(h.sources, ", ")))
	}
	return fmt.Errorf("refusing to send traffic to production: target %q resolves to %s.\n"+
		"%s\n"+
		"  The \"staging\" target and an un-overridden \"ci\" target point at the production hosts,\n"+
		"  and driver endpoint variables (AFORO_LOADGEN_*_URL) are checked too.\n"+
		"  Use --target local, a non-production URL, or --target ci with AFORO_CI_BASE_URL set.\n"+
		"  To write to production on purpose, pass --allow-production or set %s=%s",
		label, strings.Join(names, ", "), strings.Join(details, "\n"), AllowProductionEnv, AllowProductionValue)
}
