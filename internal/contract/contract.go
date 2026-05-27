package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

// SnapshotDir is the conventional path to the committed OpenAPI snapshots
// relative to the loadgen repo root. Tests resolve this by walking up from
// the calling file's location, so the contract test runs identically from
// any package depth.
const SnapshotDir = "openapi"

// Spec is the subset of the OpenAPI 3.x JSON document we read. We
// deliberately don't import an OpenAPI library because we only need three
// fields and adding kin-openapi or libopenapi pulls in a heavy dep tree for
// no gain — drift catches happen at the field-name level, not the type
// level.
type Spec struct {
	OpenAPI string                  `json:"openapi"`
	Info    SpecInfo                `json:"info"`
	Paths   map[string]any          `json:"paths"`
	Comps   map[string]SchemaBucket `json:"components"`
}

// SpecInfo is metadata included for diagnostics on test failures.
type SpecInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

// SchemaBucket holds the `components.schemas` map; we only ever read the
// "schemas" bucket but the type is flexible enough to cover the others
// (responses, parameters, etc.) when we extend coverage.
type SchemaBucket map[string]Schema

// Schema is the subset of an OpenAPI 3 Schema Object we consume.
//
// Properties: the field-name → sub-schema map. Field-name drift checks
// look up names here.
//
// AllOf: composition. Spring DTOs that extend a parent class are emitted
// with allOf:[$ref, {properties: ...}]. We flatten transitively in
// Resolve so the caller sees one unified Properties map.
//
// Ref: $ref pointer. When a property is itself a $ref to another schema
// (very common — e.g. CreateSubscriptionRequest.startDate is a ref to
// LocalDate), we currently don't recurse into it because the field-name
// check is satisfied by the property's presence on the parent. Type-shape
// drift is out of scope for the contract test (see doc.go).
//
// Required: list of @NotNull/@NotBlank fields. Not currently asserted —
// the contract test is a field-name presence check, not a field-required
// check. Recorded for future tightening.
type Schema struct {
	Type       string            `json:"type"`
	Format     string            `json:"format"`
	Ref        string            `json:"$ref"`
	Properties map[string]Schema `json:"properties"`
	AllOf      []Schema          `json:"allOf"`
	Required   []string          `json:"required"`
}

// Direction names which side of the wire a struct represents. The contract
// test treats request and response differently: a request struct sending a
// phantom field is acceptable (silently dropped server-side) but a
// response struct reading a phantom field is a bug (field is always
// empty). The expectation enum lets the contract test report the right
// diagnosis.
type Direction string

const (
	DirRequest  Direction = "request"
	DirResponse Direction = "response"
)

// Expectation tells the contract test how strict to be on a per-entry
// basis. PerfectMatch is the default; MissingFromSchemaIsOK lets the entry
// document fields that intentionally don't appear in the schema (see the
// IntentionalPhantom set in the seed package's contract_test.go).
type Expectation int

const (
	// PerfectMatch — every JSON tag in the Go struct must be present in
	// the schema's Properties (transitively across allOf). A missing tag
	// fails the test.
	PerfectMatch Expectation = iota
	// AllowPhantomRequest — the struct is a request body; tags missing
	// from the schema are tolerated (they get silently dropped by Jackson
	// server-side, often intentionally for forward-compat). Tags PRESENT
	// in the schema that the struct doesn't carry are NOT checked
	// (a request can legitimately omit optional fields).
	AllowPhantomRequest
)

// Entry is one (Go struct ↔ OpenAPI schema) pair the contract test
// enforces. Service + SchemaName identify which OpenAPI snapshot to load
// and which named schema to compare against.
type Entry struct {
	Service     string      // e.g. "customer", matches openapi/<service>.json
	SchemaName  string      // e.g. "CustomerResponse", appears in components.schemas
	StructType  reflect.Type // reflect.TypeOf(yourStruct{})
	Direction   Direction
	Expectation Expectation
	// Skip — when non-empty, the test reports the entry as skipped with
	// this reason. Use for entries whose backend OpenAPI doesn't yet
	// emit the schema (e.g. internal-only endpoints) but where the loadgen
	// struct still merits a registration row so it can be flipped on
	// later without rediscovery.
	Skip string
}

// LoadSpec reads openapi/<service>.json and parses it. The repoRoot
// parameter is normally the result of FindRepoRoot called from the test
// file's directory.
func LoadSpec(repoRoot, service string) (*Spec, error) {
	p := filepath.Join(repoRoot, SnapshotDir, service+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &spec, nil
}

// FindRepoRoot walks up from the calling source file's directory looking
// for go.mod. Tests use this so they can resolve openapi/<service>.json
// regardless of which package depth go test runs them from.
func FindRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// resolveSchema returns the named schema's fully-merged Properties map,
// flattening allOf compositions transitively. Returns nil + false if the
// schema is missing from the spec.
func (s *Spec) resolveSchema(name string) (map[string]bool, bool) {
	bucket, ok := s.Comps["schemas"]
	if !ok {
		return nil, false
	}
	root, ok := bucket[name]
	if !ok {
		return nil, false
	}
	props := map[string]bool{}
	s.mergeProperties(root, props, map[string]bool{name: true})
	return props, true
}

// mergeProperties walks Schema.AllOf transitively and merges every
// Properties map seen into out. The cycle guard prevents infinite
// recursion on a self-referential schema (rare but legal in OpenAPI).
func (s *Spec) mergeProperties(node Schema, out map[string]bool, seen map[string]bool) {
	for name := range node.Properties {
		out[name] = true
	}
	for _, sub := range node.AllOf {
		if sub.Ref != "" {
			refName := strings.TrimPrefix(sub.Ref, "#/components/schemas/")
			if seen[refName] {
				continue
			}
			seen[refName] = true
			bucket, ok := s.Comps["schemas"]
			if !ok {
				continue
			}
			refSchema, ok := bucket[refName]
			if !ok {
				continue
			}
			s.mergeProperties(refSchema, out, seen)
			continue
		}
		s.mergeProperties(sub, out, seen)
	}
}

// VerifyResult is returned per Entry. Mismatches is the list of (json tag,
// reason) pairs the test fails on; SkippedFields lists tags intentionally
// omitted (e.g. omitempty-only tags with no JSON tag).
type VerifyResult struct {
	Entry         Entry
	Mismatches    []string
	SkippedFields []string
	OK            bool
}

// Verify enforces one Entry's contract. It does NOT call t.Errorf —
// returns a VerifyResult so callers can aggregate, sort, and report all
// drift in one batch (instead of bailing on the first mismatch).
func Verify(spec *Spec, e Entry) VerifyResult {
	result := VerifyResult{Entry: e}
	if e.Skip != "" {
		result.OK = true
		return result
	}

	schemaProps, ok := spec.resolveSchema(e.SchemaName)
	if !ok {
		result.Mismatches = append(result.Mismatches,
			fmt.Sprintf("schema %q not found in openapi/%s.json", e.SchemaName, e.Service))
		return result
	}

	for _, jsonName := range extractJSONFieldNames(e.StructType) {
		if jsonName == "" {
			continue // unexported or tagged "-"
		}
		if !schemaProps[jsonName] {
			switch e.Expectation {
			case AllowPhantomRequest:
				// Silently allowed — phantom request field, server drops it.
				result.SkippedFields = append(result.SkippedFields, jsonName)
			default:
				result.Mismatches = append(result.Mismatches,
					fmt.Sprintf("field %q present on Go struct but missing from schema %s.%s",
						jsonName, e.Service, e.SchemaName))
			}
		}
	}
	sort.Strings(result.Mismatches)
	result.OK = len(result.Mismatches) == 0
	return result
}

// extractJSONFieldNames walks struct fields and returns their JSON names
// (stripped of ",omitempty" + ",string" + similar tag options). Anonymous
// embedded structs are recursed into.
func extractJSONFieldNames(t reflect.Type) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			out = append(out, extractJSONFieldNames(f.Type)...)
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			// e.g. `json:",omitempty"` — fall back to Go field name lowercased.
			// We DON'T do that here because we want to surface this as a
			// drift signal — a struct that relies on the field name as the
			// JSON name should declare it explicitly.
			continue
		}
		out = append(out, name)
	}
	return out
}
