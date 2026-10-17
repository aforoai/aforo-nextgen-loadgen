package aforo

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
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

// ProductionHosts returns the sorted, de-duplicated hosts in this target
// that are production Aforo hosts (aforo.ai or any subdomain of it).
//
// Why this exists: the predefined "staging" target, and the "ci" target when
// no AFORO_CI_* override is exported, resolve to the same *.aforo.ai hosts
// as "prod" — there is no separate staging deployment. The June 2026 e2e
// runs recorded as target=staging (runs/e2e-*-2026060*) therefore seeded
// tenants and posted events to production. The guard makes that impossible
// without an explicit acknowledgement.
func (t Target) ProductionHosts() []string {
	seen := map[string]struct{}{}
	for _, raw := range t.URLs {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host == productionDomain || strings.HasSuffix(host, "."+productionDomain) {
			seen[host] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// GuardProduction returns an error when the target points at production
// hosts and the caller has not acknowledged it, either with allowFlag (the
// --allow-production CLI flag) or with AllowProductionEnv set to
// AllowProductionValue. Commands that send no traffic (doctor, dry runs)
// should not call it.
func GuardProduction(t Target, allowFlag bool) error {
	hosts := t.ProductionHosts()
	if len(hosts) == 0 {
		return nil
	}
	if allowFlag || os.Getenv(AllowProductionEnv) == AllowProductionValue {
		return nil
	}
	return fmt.Errorf("refusing to send traffic to production: target %q resolves to %s.\n"+
		"  The \"staging\" target and an un-overridden \"ci\" target point at the production hosts.\n"+
		"  Use --target local, a non-production URL, or --target ci with AFORO_CI_BASE_URL set.\n"+
		"  To write to production on purpose, pass --allow-production or set %s=%s",
		t.Name, strings.Join(hosts, ", "), AllowProductionEnv, AllowProductionValue)
}
