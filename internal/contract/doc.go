// Package contract codifies the loadgen ↔ backend wire-format contract.
//
// The test in contract_test.go reflects over loadgen's request / response
// Go structs and, for each `json:"..."` tag, asserts the field name is
// declared in the corresponding backend OpenAPI schema. Snapshots live at
// openapi/<service>.json and are refreshed via `make sync-openapi`.
//
// What this catches:
//
//   - A loadgen struct sends/reads `json:"customer_id"` but backend
//     serializes the field as `customerId`. Backend would silently drop
//     the value on writes and return zero on reads; the contract test
//     fails the PR.
//   - A backend rename (`type` → `productType`) without a matching loadgen
//     update. The first PR that touches the backend forces a snapshot
//     refresh, which fails this test until the loadgen struct catches up.
//   - A loadgen struct claims a field the schema doesn't declare. Often
//     intentional (forward-compat phantom field, e.g. externalId), so the
//     test uses an allowlist via the IntentionalPhantom set.
//
// What this does NOT catch:
//
//   - Required-field validation drift (backend marks a field @NotBlank but
//     loadgen omits it). The contract test only validates field NAMES; a
//     missing @NotBlank field on the request body still passes here. Catch
//     these in the integration / e2e test flow instead.
//   - Type-shape mismatches (backend LocalDate vs loadgen time.Time RFC3339).
//     Contract test reads OpenAPI schema field names only, not types. Type
//     drift surfaces as a 400 deserialization failure at run time.
//   - Endpoint path drift. The endpoints constants live in
//     internal/aforo/endpoints.go; a separate test (see paths_test.go in
//     this package) probes each path against the OpenAPI snapshot's paths
//     map.
//
// How to add coverage for a new loadgen struct:
//
//  1. Add the (struct, schemaName) pair to the appropriate Services entry
//     in service_map.go.
//  2. Run `go test ./internal/contract/...` — failures are field tags
//     that don't appear in the schema. Either update the loadgen struct,
//     or refresh the snapshot via `make sync-openapi` if backend changed.
package contract
