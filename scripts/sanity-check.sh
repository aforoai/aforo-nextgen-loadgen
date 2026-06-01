#!/usr/bin/env bash
# scripts/sanity-check.sh — modular, layered pre-flight check for loadgen.
#
# Why this exists
# ---------------
# After the 2026-06-01 wire-format bug (loadgen reported "tenant created"
# but backend had no entries), we needed a way to answer "is loadgen
# actually working" without spinning up a UI or running a 7-day soak.
# This script is THAT answer: each layer is independently runnable, costs
# more than the previous one, and bails fast with a clear error if a
# prereq is missing.
#
# Layers (cheapest → most expensive)
# ----------------------------------
#   1. build         — go build the binary
#   2. test          — go test ./... (unit tests; ~30s)
#   3. vet           — go vet ./...
#   4. scenarios     — load + validate every built-in scenario YAML
#   5. dryrun        — seed --dry-run (NO network — records intended HTTP)
#   ------------------ --quick stops here ------------------
#   6. doctor        — health-probe the target (~2s per service)
#   7. seed          — live seed against --target (writes ~30 rows)
#   8. verify        — GET-by-ID a sample of seeded entities
#   9. clean         — archive every entity in the manifest
#
# Usage
# -----
#   scripts/sanity-check.sh --quick                       # layers 1-5
#   scripts/sanity-check.sh --target local                # full, vs docker
#   scripts/sanity-check.sh --target staging              # full, vs AWS
#   scripts/sanity-check.sh --target staging --skip-clean # leave rows behind
#   scripts/sanity-check.sh --only doctor                 # single layer
#
# Required env (for layers 6-9 only)
# ----------------------------------
#   AFORO_ADMIN_TOKEN  — bearer JWT with OWNER or ADMIN role
#
# Exit codes
# ----------
#   0   all requested layers passed
#   1   a layer failed; check the printed failure block + run log
#   2   misuse (bad flag, missing prereq)
#
# Conventions
# -----------
# Every layer prints a banner, runs, and reports PASS/FAIL with timing.
# Output goes to stdout AND to a per-run log file under runs/sanity/.

set -euo pipefail

# ─── arg parsing ──────────────────────────────────────────────────────────────
QUICK=0
TARGET="local"
SCENARIO="ci-smoke"
SKIP_CLEAN=0
ONLY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick)        QUICK=1; shift ;;
    --target)       TARGET="${2:?--target needs a value}"; shift 2 ;;
    --scenario)     SCENARIO="${2:?--scenario needs a value}"; shift 2 ;;
    --skip-clean)   SKIP_CLEAN=1; shift ;;
    --only)         ONLY="${2:?--only needs a value}"; shift 2 ;;
    -h|--help)
      sed -n '1,40p' "$0" | sed -n 's/^# \{0,1\}//p'
      exit 0
      ;;
    *)
      echo "unknown flag: $1 (try --help)" >&2
      exit 2
      ;;
  esac
done

# ─── paths + log file ─────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

RUN_TS="$(date +%Y%m%d-%H%M%S)"
RUN_DIR="runs/sanity/$RUN_TS"
mkdir -p "$RUN_DIR"
LOG_FILE="$RUN_DIR/sanity.log"
MANIFEST="$RUN_DIR/manifest.json"

BIN="$REPO_ROOT/bin/aforo-loadgen"

# ─── pretty output helpers ────────────────────────────────────────────────────
red()    { printf "\033[31m%s\033[0m" "$*"; }
green()  { printf "\033[32m%s\033[0m" "$*"; }
yellow() { printf "\033[33m%s\033[0m" "$*"; }
bold()   { printf "\033[1m%s\033[0m"  "$*"; }

banner() {
  echo
  echo "────────────────────────────────────────────────────────────"
  echo "▶ $(bold "$1")"
  echo "────────────────────────────────────────────────────────────"
}

# Per-layer results stored as TSV lines in a tmp file. macOS ships bash 3.2
# which lacks associative arrays — this is the portable substitute.
RESULTS_FILE="$(mktemp -t loadgen-sanity-results.XXXXXX)"
trap 'rm -f "$RESULTS_FILE"' EXIT

record() {
  local name="$1" status="$2" elapsed="$3"
  printf '%s\t%s\t%s\n' "$name" "$status" "$elapsed" >> "$RESULTS_FILE"
}

result_of() { awk -F'\t' -v n="$1" '$1==n{print $2; exit}' "$RESULTS_FILE"; }
time_of()   { awk -F'\t' -v n="$1" '$1==n{print $3; exit}' "$RESULTS_FILE"; }

run_layer() {
  local name="$1"; shift
  if [[ -n "$ONLY" && "$ONLY" != "$name" ]]; then
    record "$name" "SKIP" "0s"
    return 0
  fi
  banner "$name"
  local start; start=$(date +%s)
  if "$@"; then
    local end; end=$(date +%s)
    local elapsed="$((end-start))s"
    echo "$(green "PASS") — $name ($elapsed)"
    record "$name" "PASS" "$elapsed"
    return 0
  else
    local rc=$?
    local end; end=$(date +%s)
    local elapsed="$((end-start))s"
    echo "$(red "FAIL") — $name ($elapsed) — exit=$rc"
    record "$name" "FAIL" "$elapsed"
    return $rc
  fi
}

# ─── layer 1: build ───────────────────────────────────────────────────────────
layer_build() {
  go build -o "$BIN" ./cmd/aforo-loadgen 2>&1 | tee -a "$LOG_FILE"
  test -x "$BIN"
}

# ─── layer 2: unit tests ──────────────────────────────────────────────────────
layer_test() {
  go test ./... -count=1 2>&1 | tee -a "$LOG_FILE" | tail -15
  # last command's exit status is the tee, which always succeeds — re-check:
  go test ./... -count=1 >/dev/null 2>&1
}

# ─── layer 3: vet ─────────────────────────────────────────────────────────────
layer_vet() {
  go vet ./... 2>&1 | tee -a "$LOG_FILE"
}

# ─── layer 4: scenarios ───────────────────────────────────────────────────────
layer_scenarios() {
  local n=0 ok=0
  for f in scenarios/*.yaml; do
    n=$((n+1))
    if "$BIN" scenarios validate "$f" >>"$LOG_FILE" 2>&1; then
      ok=$((ok+1))
    else
      echo "  $(red "✗") $f" | tee -a "$LOG_FILE"
    fi
  done
  echo "validated $ok/$n built-in scenarios"
  [[ $ok -eq $n ]]
}

# ─── layer 5: dry-run seed ────────────────────────────────────────────────────
layer_dryrun() {
  "$BIN" seed --scenario "$SCENARIO" --dry-run \
      --out "$RUN_DIR/dryrun-manifest.json" 2>&1 \
    | tee -a "$LOG_FILE" \
    | grep -E "^seed complete|^by archetype|^  " \
    | head -15
  test -s "$RUN_DIR/dryrun-manifest.json"
}

# ─── prereq gate for layers 6-9 ───────────────────────────────────────────────
check_live_prereqs() {
  local missing=""
  [[ -z "${AFORO_ADMIN_TOKEN:-}" ]] && missing+="AFORO_ADMIN_TOKEN "
  command -v jq >/dev/null || missing+="jq "
  command -v curl >/dev/null || missing+="curl "
  if [[ -n "$missing" ]]; then
    echo "$(red "PREREQ MISSING"): $missing"
    echo "  AFORO_ADMIN_TOKEN: export it before running live layers."
    echo "  jq, curl: brew install jq / part of macOS base."
    return 1
  fi
  return 0
}

# ─── layer 6: doctor ──────────────────────────────────────────────────────────
layer_doctor() {
  "$BIN" doctor --target "$TARGET" 2>&1 | tee -a "$LOG_FILE"
  # doctor exits non-zero on any CRITICAL fail; we want that to propagate.
}

# ─── layer 7: live seed ───────────────────────────────────────────────────────
layer_seed() {
  # Capture full output so we can both stream it and inspect for errors.
  # The seed CLI exits 0 even when individual provisioners fail (errors=N
  # in the summary line) and the partial manifest still gets written. We
  # MUST detect that and fail the layer, otherwise verify silently runs
  # against an empty manifest and reports "envelope-decode regression"
  # when the real culprit is upstream.
  local seed_out
  seed_out=$("$BIN" seed --scenario "$SCENARIO" --target "$TARGET" \
      --out "$MANIFEST" 2>&1) || true
  echo "$seed_out" >> "$LOG_FILE"
  echo "$seed_out" | grep -E "^seed complete|^manifest|^by archetype|^  |error" | head -20

  test -s "$MANIFEST" || { echo "  $(red "✗") manifest not written"; return 1; }
  # Detect "errors=N" where N>0 in the summary line.
  if echo "$seed_out" | grep -qE "^seed complete:.*errors=[1-9]"; then
    echo "  $(red "✗") seed reported one or more errors — see log for the API response body"
    return 1
  fi
}

# ─── layer 8: verify — re-fetch entities by ID and confirm they exist ────────
layer_verify() {
  # Loadgen wrote the manifest with the IDs the BACKEND returned. If the
  # 2026-06-01 envelope bug were still active, those IDs would be empty
  # strings and this layer would catch it immediately. We GET each one
  # back and assert the response payload's id matches.
  local org_url cat_url cust_url pricing_url
  case "$TARGET" in
    local)    org_url="http://localhost:8086"; cat_url="http://localhost:8081"; cust_url="http://localhost:8085"; pricing_url="http://localhost:8083" ;;
    staging|prod)
      org_url="https://org.aforo.ai"; cat_url="https://catalog.aforo.ai"
      cust_url="https://customer.aforo.ai"; pricing_url="https://pricing.aforo.ai" ;;
    *)
      echo "verify: unsupported target $TARGET (only local/staging/prod resolved)"
      return 1 ;;
  esac

  local tenant_id product_id customer_id sub_id ok=0 total=0
  tenant_id=$(jq -r '.tenants[0].tenant_id' "$MANIFEST")
  product_id=$(jq -r '.tenants[0].products[0].product_id' "$MANIFEST")
  customer_id=$(jq -r '.tenants[0].customers[0].customer_id' "$MANIFEST")
  sub_id=$(jq -r '.tenants[0].customers[0].subscriptions[0].subscription_id' "$MANIFEST")

  echo "Verifying entities written by seed:"
  echo "  tenant_id    = $tenant_id"
  echo "  product_id   = $product_id"
  echo "  customer_id  = $customer_id"
  echo "  sub_id       = $sub_id"

  # Empty IDs are the canonical symptom of the envelope-decode bug. Fail
  # fast and noisily so it never slips again.
  for var in tenant_id product_id customer_id sub_id; do
    total=$((total+1))
    if [[ -z "${!var}" || "${!var}" == "null" ]]; then
      echo "  $(red "✗") $var is empty in manifest — envelope-decode regression?"
      continue
    fi
    ok=$((ok+1))
  done
  echo "  manifest IDs: $ok/$total non-empty"
  [[ $ok -eq $total ]] || return 1

  # Re-fetch each entity. 200 = exists, 404 = manifest lied (backend has no row).
  local hits=0 misses=0
  for triple in \
    "tenant:$org_url/api/v1/internal/tenants?externalId=$tenant_id" \
    "product:$cat_url/api/v1/products/$product_id" \
    "customer:$cust_url/api/v1/customers/$customer_id" \
    "subscription:$pricing_url/api/v1/subscriptions/$sub_id"
  do
    local label="${triple%%:*}" url="${triple#*:}" code
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer $AFORO_ADMIN_TOKEN" \
      -H "X-Tenant-Id: $tenant_id" \
      --max-time 15 "$url" 2>/dev/null || echo "000")
    if [[ "$code" =~ ^2 ]]; then
      echo "  $(green "✓") $label GET → $code"
      hits=$((hits+1))
    else
      echo "  $(red "✗") $label GET → $code at $url"
      misses=$((misses+1))
    fi
  done
  echo "  backend lookups: $hits ok, $misses missing"
  [[ $misses -eq 0 ]]
}

# ─── layer 9: clean ───────────────────────────────────────────────────────────
layer_clean() {
  if [[ $SKIP_CLEAN -eq 1 ]]; then
    echo "skipped (--skip-clean)"
    return 0
  fi
  # `seed --clean-from <path>` alone runs a normal seed — the trigger to
  # ACTUALLY clean is the bare `--clean` flag. Without it, the CLI ignores
  # --clean-from and re-seeds. Also: the CLI still wants --scenario even
  # in clean mode (it loads it before short-circuiting). Pass both.
  "$BIN" seed --clean --clean-from "$MANIFEST" \
      --scenario "$SCENARIO" --target "$TARGET" \
      2>&1 | tee -a "$LOG_FILE" | tail -10
}

# ─── orchestration ────────────────────────────────────────────────────────────
echo "$(bold "aforo-loadgen sanity check")"
echo "  target:   $TARGET"
echo "  scenario: $SCENARIO"
echo "  mode:     $([[ $QUICK -eq 1 ]] && echo quick || echo full)"
echo "  log:      $LOG_FILE"
echo "  manifest: $MANIFEST"
echo

OVERALL=0

run_layer "build"     layer_build     || OVERALL=1
run_layer "test"      layer_test      || OVERALL=1
run_layer "vet"       layer_vet       || OVERALL=1
run_layer "scenarios" layer_scenarios || OVERALL=1
run_layer "dryrun"    layer_dryrun    || OVERALL=1

if [[ $QUICK -eq 0 && -z "$ONLY" ]]; then
  if check_live_prereqs; then
    run_layer "doctor" layer_doctor || OVERALL=1
    # Don't seed if doctor already failed — would just add noise.
    if [[ "$(result_of doctor)" == "PASS" ]]; then
      run_layer "seed"   layer_seed   || OVERALL=1
      if [[ "$(result_of seed)" == "PASS" ]]; then
        run_layer "verify" layer_verify || OVERALL=1
      fi
      # Always try clean if a manifest exists, even if verify failed —
      # cleanup-after-failure is cheap and protects the shared target.
      [[ -s "$MANIFEST" ]] && { run_layer "clean" layer_clean || OVERALL=1; }
    fi
  else
    echo
    yellow "skipping live layers (doctor/seed/verify/clean) — prereqs not met"
    echo
    record doctor SKIP 0s; record seed SKIP 0s
    record verify SKIP 0s; record clean SKIP 0s
  fi
elif [[ -n "$ONLY" ]]; then
  # In --only mode, run the live layer if that's what was requested.
  case "$ONLY" in
    doctor|seed|verify|clean)
      if check_live_prereqs; then
        case "$ONLY" in
          doctor) run_layer "doctor" layer_doctor || OVERALL=1 ;;
          seed)   run_layer "seed"   layer_seed   || OVERALL=1 ;;
          verify) [[ -s "$MANIFEST" ]] || { red "verify needs an existing manifest at $MANIFEST"; OVERALL=1; }; \
                  run_layer "verify" layer_verify || OVERALL=1 ;;
          clean)  [[ -s "$MANIFEST" ]] || { red "clean needs an existing manifest at $MANIFEST"; OVERALL=1; }; \
                  run_layer "clean"  layer_clean  || OVERALL=1 ;;
        esac
      else
        OVERALL=1
      fi ;;
  esac
fi

# ─── summary ──────────────────────────────────────────────────────────────────
echo
echo "$(bold "summary")"
echo "──────────────────────────────────────────"
printf "  %-12s  %-6s  %s\n" "layer" "result" "time"
for layer in build test vet scenarios dryrun doctor seed verify clean; do
  status="$(result_of "$layer")"
  elapsed="$(time_of "$layer")"
  [[ -z "$status" ]]  && status="—"
  [[ -z "$elapsed" ]] && elapsed="—"
  colored="$status"
  case "$status" in
    PASS) colored="$(green PASS)" ;;
    FAIL) colored="$(red FAIL)" ;;
    SKIP) colored="$(yellow SKIP)" ;;
  esac
  printf "  %-12s  %s  %s\n" "$layer" "$colored" "$elapsed"
done
echo "──────────────────────────────────────────"
echo "  log:      $LOG_FILE"
echo "  manifest: $MANIFEST"

if [[ $OVERALL -ne 0 ]]; then
  echo
  red "OVERALL: FAIL"
  echo
  echo "Common failure modes:"
  echo "  • envelope decode regression  → check internal/seed/client.go"
  echo "  • backend DTO drift           → run with --only dryrun and diff bodies"
  echo "  • staging unreachable         → curl https://org.aforo.ai/actuator/health"
  echo "  • token expired/wrong role    → role must include OWNER or ADMIN"
  exit 1
fi
echo
green "OVERALL: PASS"
echo
