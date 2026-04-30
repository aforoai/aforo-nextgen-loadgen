// Package server implements the loadgen control-plane HTTP API.
//
// The server provides the operator-facing layer that Control Tower's
// /admin/loadgen pages talk to. The CLI remains the workhorse for 95%
// of usage; this package adds REST endpoints over the same scenario
// catalog and run engine so non-CLI users can trigger and inspect runs
// from a browser.
//
// Endpoints (all under /api/v1):
//
//	POST /runs              — trigger a run; returns 202 with run_id
//	GET  /runs              — paginated run list (status, scenario, target filters)
//	GET  /runs/{id}         — run detail with assertions + breakdowns
//	POST /runs/{id}/cancel  — graceful cancel; worker drains and writes partial manifest
//	GET  /scenarios         — list built-in scenarios
//	GET  /health            — liveness + dependency probe
//
// Auth: every endpoint except /health requires a Supabase JWT in the
// Authorization header. Validation round-trips to Supabase's
// /auth/v1/user — handles HS256 (legacy) and RS256 (new projects)
// transparently. RBAC is enforced via the platform_admin internal role,
// resolved from the internal_roles table the same way Control Tower's
// API routes do.
//
// Storage:
//   - Manifests: local filesystem by default; optionally pushed to S3
//     via the `aws` CLI when --s3-bucket is set. Manifest path stored
//     as either file:///abs/path or s3://bucket/key.
//   - Index: Supabase loadgen_runs table written via PostgREST (HTTP);
//     no DB driver required. When --supabase-url is empty the server
//     runs index-less (in-memory only) for local development.
//
// Concurrency:
//   - One run state object per run_id is kept in memory while the run
//     is active; the worker subprocess writes the canonical run.json
//     and the server tails it for status transitions.
//   - SIGINT/SIGTERM cancels every active run gracefully before the
//     server exits.
package server
