# 2026-04-29 — Session 2: scenario YAML schema

## What changed

Defined the scenario YAML schema — the contract every later component
anchors to (seed harness, run engine, fault injectors, lifecycle, payments,
ERP, chaos, assertions). Implemented load + strict-decode + validation +
migration + 6 starter scenarios + the `scenarios` subcommand.

The headline addition is **`TenantArchetype`**, which lets a single
scenario provision deliberately varied tenants — covering multiple pricing
models, billing modes, currency mixes, subscription state mixes, and
discount mixes. This is what makes a 50- or 500-tenant load run actually
exercise the platform, rather than 500 copies of the same trivial config.

## Files added

### Schema package — `internal/scenario/`

- `types.go` — Scenario + 17 substructure types. Custom `Duration` type
  that decodes `"60s"`, `"5m"`, `"24h"` from YAML. Enums for ProductType
  (4), PricingModel (6), BillingMode (3), SubscriptionState (9),
  Distribution (3), TimePattern (3), TaxEngine (3), StripeMode (2).
- `parser.go` — `LoadFromFile`, `LoadFromBytes`, `Document` (carries the
  raw `yaml.Node` tree so validation errors resolve to file:line:col).
  Strict decode via `yaml.NewDecoder(...).KnownFields(true)`.
- `validator.go` — `Validate(doc) ValidationErrors`. 14 section checkers +
  cross-field rules. Errors sorted by (line, column, path) for
  deterministic output. Tool-friendly format
  `<file>:<line>:<col>: <path>: <msg>`.
- `defaults.go` — `applyDefaults` fills only low-stakes defaults
  (distribution, time_pattern, payload mix when all zero, tax engine,
  stripe mode when payments enabled, safe assertion booleans). Refuses to
  clobber author-set values.
- `migration.go` — `Migrate(s)` enforces `CurrentSchemaVersion = 1`. Stub
  for the v1 → v2 chain when v2 ships.

### Test files — `internal/scenario/`

- `parser_test.go` — minimal valid load, empty input, unknown field
  rejection, invalid duration, file round-trip, missing file, Duration
  marshal/unmarshal, indexSegment helpers.
- `defaults_test.go` — distribution/time_pattern/payload defaults,
  author-set preservation, stripe mode gated on enabled, assertion
  booleans, nil-safety, table-driven `assertionsTouched`.
- `migration_test.go` — nil/missing/negative/newer/current schema
  versions; `CurrentSchemaVersion == 1` tripwire that fires loudly when
  a future session bumps it without updating the migration chain.
- `validator_test.go` — 40+ table-driven cases covering every documented
  rule (kebab-case name, target_tps > 0, duration > 0, weights sum to 1.0,
  pricing/billing/state enum membership, PREPAID/HYBRID requires wallet,
  pricing-model-specific rate_config minimums, stale-key cross-field rule,
  payments enabled requires stripe_mode, ERP enabled requires
  sync_sla_seconds, chaos enabled requires events, cross_tenant_leakage_max
  must be 0, etc.). Plus location formatting and deterministic-order tests.
- `golden_test.go` — every built-in scenario MUST load + validate clean;
  matrix-billing MUST cover all 18 (pricing × billing) combos and have
  >= 30 archetypes; lifecycle-stress MUST have lifecycle.enabled=true and
  at least one transition class non-zero.

### Built-in scenarios — `scenarios/`

- `embed.go` — `embed.FS`-backed catalog with `Names()` and `Read(name)`.
- `embed_test.go` — alphabetical sort, .yaml-only filter, known/unknown
  Read paths, basename-without-extension contract.
- `ci-smoke.yaml` — 1 tenant, 1 archetype, 50 TPS, 60s. CI gate.
- `crawl-e2e.yaml` — 4 tenants (one per product type), 50 TPS, 5m.
- `walk-realistic-50t.yaml` — 50 tenants, 8 archetypes, sine-24h, 2K TPS,
  24h. Light fault injection.
- `run-15k-7day.yaml` — 500 tenants, 12 archetypes, all 16 ingest paths,
  15K TPS, 7d. Headline endurance run. Lifecycle on at low rate.
- `matrix-billing.yaml` — 30 archetypes covering every (pricing × billing)
  combo + 12 variants exercising stale state mixes, multi-currency, and
  discount mixes. 1K TPS, 2h.
- `lifecycle-stress.yaml` — 5 archetypes biased toward transition-eligible
  states. Lifecycle at full intensity. 1K TPS, 4h.

### CLI wiring — `internal/cli/scenarios.go`

- Replaced the Session 1 stub with a parent command + 4 leaves:
  - `scenarios list` — tab-aligned catalog (name, target TPS, duration,
    tenants, archetypes, description).
  - `scenarios validate <file>` — strict load + validate, returns
    non-zero on any failure with file:line:col path.
  - `scenarios show <name>` — prints the raw YAML bytes of a built-in.
  - `scenarios archetypes <name>` — tab-aligned archetype configs.
- `scenarios_test.go` — covers all 4 leaves plus the bare-parent
  prints-help case (TestEverySubcommandExitsZero contract).

### Test wiring — `internal/cli/cli_test.go`

- Removed `scenarios` from the `stubs` list in `TestStubsAdvertiseSession`
  since it's now fully implemented. Kept it in `expectedSubcommands` so
  the help-listing contract still gates renames.

### Docs

- `docs/scenario-schema.md` — full human-facing reference (~10 sections,
  every field, every rule, full reference table for built-in scenarios).
- `README.md` — updated Status to Session 2 ✓, added Quickstart examples
  for all 4 `scenarios` leaves, added "Adding a built-in scenario"
  section, updated subcommand roadmap table.

## Why

Three goals shape the schema:

1. **Lock the contract before downstream work starts.** Sessions 3-12 read
   from this schema. Re-shaping it later means N service rewrites; the
   strict decode + KnownFields(true) + golden-file tests guarantee that
   any drift between the YAML files and the Go types is a build failure.

2. **Tenant variety, declaratively.** A 500-tenant load run on a single
   uniform configuration would prove almost nothing. `TenantArchetype` is
   a weighted bag the seed harness draws from — one scenario covers
   multiple billing/pricing/state/discount/currency profiles realistically.
   Cross-field rules guard the obvious traps (PREPAID without a wallet,
   stale-key injection without stale states).

3. **Validation errors that are actionable.** Every error carries a path
   like `tenants.archetypes[3].weight` plus file:line:col. Drop a bad
   file into CI and the user sees exactly which line to fix.

## Acceptance criteria status

| Criterion                                                              | Status |
| ---------------------------------------------------------------------- | :----: |
| `aforo-loadgen scenarios list` prints 6 scenarios                      | done   |
| All 6 scenarios pass `scenarios validate`                              | done   |
| Bad scenario (stale_keys without CANCELLED) fails with exact path      | done   |
| `scenarios archetypes walk-realistic-50t` prints 8 archetype configs   | done   |
| `make test` passes with > 85% scenario package coverage (got 91.3%)    | done   |
| Strict decode rejects unknown fields                                   | done   |
| Validation errors include file:line:col                                | done   |
| Schema migration chain in place (currently no-op for v1)               | done   |

## Bugs found and fixed in this session

1. **Duplicate-print on validate failure.** First draft of
   `runScenariosValidate` printed the load error to stderr AND returned
   it; main also prints returned errors. Result was the same line twice.
   Fix: print only validation errors (per-line), return only the summary
   error and let main print the summary once.
2. **Float-boundary test was flaky.** `weightsApproxOne(0.999)` test
   case relied on `0.999` round-tripping through float64; it doesn't
   exactly, so the literal sum was just outside tolerance. Fix: pulled
   test cases off the boundary (0.9995 / 1.0005 inside, 0.998 / 1.002
   outside) — we test the rule, not IEEE-754.
3. **Dead loop body in `TestRead_OmitsExtension`.** Drafted with a
   redundant `if got := n; got != n` that the linter would have
   eventually flagged. Fix: replaced with the actual contract assertion
   (`!strings.HasSuffix(n, ".yaml")`).

## Out of scope (deferred)

- Embedding 4 sample _user_ scenarios under `examples/` for blog/docs —
  the in-repo `scenarios/` catalog covers the core methodology; docs
  examples can ship later without schema changes.
- `--strict-tax` flag to require a non-empty `tax.jurisdictions` map —
  current default (mock engine, empty jurisdictions, all-zero rates) is
  acceptable for sessions 3-9; revisit alongside Session 5 (lifecycle).
- Stripe env-var verification (`AFORO_STRIPE_TEST_KEY` set when
  `payments.enabled=true`) — the validator deliberately stays a pure
  static check. The run engine in Session 9 will enforce env-var presence
  and produce a more actionable error than "validate said you set
  stripe_mode but the key is missing".
- Schema v2 migration path. `Migrate()` is structured so the v1 → v2
  upgrade slots in cleanly when v2 lands; adding it now would be
  premature.

## Notes for future sessions

- **Adding a new built-in scenario:** drop the YAML in `scenarios/`,
  add the basename to `expectedNames` in `internal/scenario/golden_test.go`,
  bump the `expectedCount` constant, document under "Reference: built-in
  scenarios" in `docs/scenario-schema.md`. CI fails fast if any of those
  three are missed.
- **Adding a new field to the schema:** append to the relevant struct in
  `types.go`, add a check in the matching `check*` method in
  `validator.go`, add a case in `validator_test.go`, append to the
  matching section in `docs/scenario-schema.md`. Keep field names
  snake_case in YAML and PascalCase in Go (the `yaml:` tag bridges).
- **Renaming or removing a field:** that's a schema-version bump.
  Increment `CurrentSchemaVersion`, update the tripwire in
  `migration_test.go`, add an upgrade step to `Migrate()`. Old scenario
  files keep loading.
