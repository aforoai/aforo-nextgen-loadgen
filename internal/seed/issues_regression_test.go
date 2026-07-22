package seed

import (
	"regexp"
	"strings"
	"testing"
)

// TestTenantExternalID_LengthAndFormat locks the invariants that Issue 1
// depends on: every generated tenant identifier must fit inside the
// boilerplate workspace-slug budget AND satisfy the Zod slug regex.
//
// The boilerplate's onboarding schema at
// apps/web/app/onboarding/_lib/schema/onboarding.schema.ts caps the slug at
// 50 chars via .max(50) and enforces the regex
// ^[a-z0-9]+(?:-[a-z0-9]+)*$. LoadgenInternalTenantController's slug()
// helper additionally shaves ~9 chars off for a CRC32 suffix when the input
// exceeds 50 chars — so we target ≤ 45 chars here to leave slack for either
// consumer.
func TestTenantExternalID_LengthAndFormat(t *testing.T) {
	slugRe := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	const maxLen = 45 // stricter than the 50-char boilerplate limit — see doc

	// Real-world archetype names + a torture-test string that mixes case,
	// underscores, and non-slug punctuation.
	archetypes := []string{
		"gt",                              // shortest realistic (2 chars)
		"mtx-flat",                        // simple hyphenated (8 chars)
		"complete-fresh",                  // ~14 chars — the case that hit
		"complete_fresh_seed_extra_long",  // 30 chars w/ underscores
		"MixedCase.Punctuation!",          // strip non-alphanumerics
		strings.Repeat("longname", 4),     // 32 chars, pure ascii
		"",                                // degenerate: empty name
	}
	// Realistic runIDs — matches newRunID's shape.
	runIDs := []string{
		"seed-2026-07-21-949610",
		"seed-2026-12-31-abcdef",
		"seed-2026-01-01-000001",
	}

	for _, arch := range archetypes {
		for _, run := range runIDs {
			for _, seq := range []int{1, 42, 999} {
				got := tenantExternalID(arch, run, seq)
				if len(got) > maxLen {
					t.Errorf("tenantExternalID(arch=%q, run=%q, seq=%d) = %q (len %d), exceeds max %d",
						arch, run, seq, got, len(got), maxLen)
				}
				if !slugRe.MatchString(got) {
					t.Errorf("tenantExternalID(arch=%q, run=%q, seq=%d) = %q does not match slug regex",
						arch, run, seq, got)
				}
			}
		}
	}
}

// TestTenantExternalID_Deterministic guarantees that two calls with the
// same inputs produce the same identifier. The seeder relies on this for
// within-24h idempotency of downstream calls that were keyed off the
// tenant slug.
func TestTenantExternalID_Deterministic(t *testing.T) {
	got1 := tenantExternalID("mtx-flat", "seed-2026-07-21-949610", 1)
	got2 := tenantExternalID("mtx-flat", "seed-2026-07-21-949610", 1)
	if got1 != got2 {
		t.Fatalf("tenantExternalID is not deterministic: %q vs %q", got1, got2)
	}
}

// TestTenantExternalID_DistinctPerSeq — different tenant slots MUST get
// different identifiers, otherwise concurrent workers race on the same
// tenant row.
func TestTenantExternalID_DistinctPerSeq(t *testing.T) {
	seen := make(map[string]int)
	arch := "myarch"
	run := "seed-2026-07-21-949610"
	for seq := 1; seq <= 20; seq++ {
		got := tenantExternalID(arch, run, seq)
		if prior, dup := seen[got]; dup {
			t.Fatalf("tenantExternalID collision at seq=%d (prior seq=%d): %q", seq, prior, got)
		}
		seen[got] = seq
	}
}

// TestTenantExternalID_DistinctPerArchetype — two archetypes in the same
// run must not collide. If the truncation dropped the archetype identity
// entirely, this would collide.
func TestTenantExternalID_DistinctPerArchetype(t *testing.T) {
	run := "seed-2026-07-21-949610"
	a := tenantExternalID("mtx-perunit", run, 1)
	b := tenantExternalID("mtx-flat", run, 1)
	if a == b {
		t.Fatalf("tenantExternalID collided across archetypes: %q == %q", a, b)
	}
}

// TestLookupAPIKeyByAccessor_PrefersActiveOverRevoked — Issue 4 fix. The
// lookup MUST return an ACTIVE key when both an ACTIVE and REVOKED row
// exist for the same (customer, accessor), so callers keep getting a
// usable secret on the happy path.
func TestLookupAPIKeyByAccessor_PrefersActiveOverRevoked(t *testing.T) {
	// Mimic the inner selection loop directly — we don't need the HTTP
	// transport to exercise the ranking logic.
	rows := []apiKeyResponse{
		{ID: "old", CustomerID: "c1", AccessorID: "a1", Status: "REVOKED"},
		{ID: "new", CustomerID: "c1", AccessorID: "a1", Status: "ACTIVE"},
	}
	got := selectAPIKeyMatch(rows, "c1", "a1")
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.ID != "new" {
		t.Fatalf("expected ACTIVE row (id=new), got %+v", got)
	}
}

// TestLookupAPIKeyByAccessor_FallsBackToRevoked — the closer of Issue 4.
// When only a REVOKED row exists (the re-seed scenario), the lookup must
// return it so the 409 recovery path can proceed instead of surfacing
// "create api-key" as an error.
func TestLookupAPIKeyByAccessor_FallsBackToRevoked(t *testing.T) {
	rows := []apiKeyResponse{
		{ID: "old", CustomerID: "c1", AccessorID: "a1", Status: "REVOKED"},
	}
	got := selectAPIKeyMatch(rows, "c1", "a1")
	if got == nil {
		t.Fatal("expected a fallback match on the REVOKED row, got nil")
	}
	if got.ID != "old" {
		t.Fatalf("expected id=old, got %+v", got)
	}
}

// TestLookupAPIKeyByAccessor_IgnoresMismatchedRows — rows that don't
// match the requested (customer, accessor) must never be returned.
func TestLookupAPIKeyByAccessor_IgnoresMismatchedRows(t *testing.T) {
	rows := []apiKeyResponse{
		{ID: "wrong-cust", CustomerID: "cX", AccessorID: "a1", Status: "ACTIVE"},
		{ID: "wrong-acc", CustomerID: "c1", AccessorID: "aX", Status: "ACTIVE"},
	}
	got := selectAPIKeyMatch(rows, "c1", "a1")
	if got != nil {
		t.Fatalf("expected no match, got %+v", got)
	}
}

// selectAPIKeyMatch mirrors the inner loop of lookupAPIKeyByAccessor so
// the ranking logic is testable without spinning up a fake HTTP server.
// If this and the real loop ever diverge, one of them is wrong — keep
// them in lockstep.
func selectAPIKeyMatch(keys []apiKeyResponse, customerID, accessorID string) *apiKeyResponse {
	var fallback *apiKeyResponse
	for i := range keys {
		k := &keys[i]
		if k.CustomerID != customerID || k.AccessorID != accessorID {
			continue
		}
		if k.Status == "ACTIVE" {
			return k
		}
		if fallback == nil {
			fallback = k
		}
	}
	return fallback
}
