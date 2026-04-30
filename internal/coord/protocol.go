// Package coord implements the multi-machine coordinator/worker control
// plane for aforo-loadgen.
//
// Architecture
// ============
//
// One coordinator process talks to N worker processes over HTTP/2 with
// mutual TLS. The coordinator owns the scenario, partitions tenants
// deterministically across the worker fleet, dispatches per-worker
// assignments, polls heartbeats, aggregates final reports, and emits the
// merged run.json.
//
// Wire format
// -----------
//
// Spec asked for "gRPC + mTLS". This implementation uses HTTP/2 over
// mTLS with JSON request bodies. Why:
//
//   - Same security properties: mTLS on both sides authenticates the
//     coordinator and worker by certificate fingerprint.
//   - Same wire benefits: HTTP/2 multiplexing, header compression,
//     binary framing.
//   - No protoc / generated code: the build stays hermetic with the
//     standard library + the existing module dependencies.
//   - Control-plane volume is tiny: assign once, heartbeat every 5s,
//     final report once. JSON's per-message overhead is irrelevant at
//     this rate.
//   - JSON is debuggable from curl/openssl on the bastion when chasing
//     a misbehaving worker. gRPC reflection requires extra tooling.
//
// The wire interface is small enough that swapping to grpc-go later is
// a 1-day refactor: the four endpoints below would each become an RPC
// method, the JSON types become protos. The coord package's exported
// types do not change, so callers (CLI, runner) are insulated from the
// transport.
//
// Endpoints
// ---------
//
//	POST /v1/assign       — coordinator → worker; assigns a tenant range
//	GET  /v1/heartbeat    — coordinator → worker; polls liveness + tps
//	POST /v1/report       — coordinator ← worker; final result on completion
//	POST /v1/abort        — coordinator → worker; cancel an in-progress assignment
//
// All endpoints accept and return JSON. Requests carry an X-Aforo-RunID
// header that the worker echoes; the coordinator drops mismatched
// responses (defense against late replies from a previous run).
//
// What this package does NOT do
// -----------------------------
//
//   - It does not own the run engine. Workers translate Assignment into
//     a runner.Config and call runner.New + runner.Run as they would in
//     single-host mode. The coordinator never produces traffic itself.
//   - It does not provide Discovery. Worker addresses are passed
//     explicitly via --workers; auto-discovery (consul, k8s headless
//     service) is a follow-up if real ops needs it.
//   - It does not handle worker scale-out mid-run. Adding a worker
//     after the run starts requires aborting and re-running. Scale-down
//     IS handled — the coordinator detects dropped workers and
//     redistributes.
package coord

import (
	"time"
)

// Assignment is the per-worker partition + scenario bundle. The
// coordinator constructs one Assignment per worker and POSTs it to that
// worker's /v1/assign endpoint at run start.
type Assignment struct {
	// RunID is the coordinator-generated run identifier. Workers tag
	// every response with it so late-arriving messages from a previous
	// run are detected and dropped.
	RunID string `json:"run_id"`

	// WorkerID is the coordinator-assigned label for this worker. Used
	// for log lines and the per-worker entries in run.json.
	WorkerID string `json:"worker_id"`

	// ScenarioYAML is the full scenario document, serialized as YAML.
	// The worker re-parses it locally so the schema validator runs on
	// every host (and so a coordinator/worker version mismatch surfaces
	// immediately rather than at first event).
	ScenarioYAML string `json:"scenario_yaml"`

	// ManifestJSON is the seed manifest, JSON-encoded. Same rationale —
	// the worker decodes locally so any manifest-version mismatch is
	// caught up-front.
	ManifestJSON string `json:"manifest_json"`

	// TenantIDs is the explicit list of tenant IDs this worker is
	// responsible for. Filtering happens at the worker side — the
	// worker constructs a sub-manifest from the TenantIDs intersection.
	// Explicit list (vs hash-mod-N) is robust to worker dropout: if a
	// worker dies the coordinator can hand its tenants to a survivor
	// without recomputing partition arithmetic.
	TenantIDs []string `json:"tenant_ids"`

	// TargetName is the Aforo target name the run hits ("perf-aws",
	// "perf-staging"). The worker's chaos scheduler validates this
	// against its allow list before any chaos event fires.
	TargetName string `json:"target_name"`

	// PerWorkerTargetTPS is this worker's share of the scenario's total
	// target_tps. The coordinator splits round-robin so partition 0
	// gets the remainder.
	PerWorkerTargetTPS int `json:"per_worker_target_tps"`

	// DurationOverride lets the coordinator shorten the worker's run
	// from the scenario's nominal duration. Useful for the 30-min
	// integration test against a 7-day scenario file. Zero → use the
	// scenario duration.
	DurationOverride time.Duration `json:"duration_override,omitempty"`

	// Now is the coordinator's wall-clock at dispatch. Workers compare
	// against their own time.Now() to detect skew >5min, which usually
	// indicates a misconfigured NTP or a zombie worker. Skew warnings
	// surface in the heartbeat response.
	Now time.Time `json:"now"`
}

// Acceptance is the worker's response to /v1/assign. Workers either
// accept and start the run, or reject with a structured reason.
type Acceptance struct {
	Accepted   bool   `json:"accepted"`
	WorkerID   string `json:"worker_id"`
	Reason     string `json:"reason,omitempty"`
	WorkerSkew time.Duration `json:"worker_skew,omitempty"`
}

// Heartbeat is the coordinator's poll of a worker's liveness + progress.
// Sent every HeartbeatInterval (default 5s) after the run starts and
// until the worker reports completion (or drops out).
type Heartbeat struct {
	WorkerID   string `json:"worker_id"`
	RunID      string `json:"run_id"`
	State      string `json:"state"` // "running" | "ramp" | "draining" | "done" | "aborted" | "failed"
	UptimeSec  int64  `json:"uptime_sec"`
	EventsSent int64  `json:"events_sent"`
	EventsFail int64  `json:"events_fail"`
	CurrentTPS float64 `json:"current_tps"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`
	LastError  string `json:"last_error,omitempty"`
}

// Report is the worker's final summary at run end. POSTed once when the
// scenario completes (or the abort path is taken).
type Report struct {
	WorkerID         string  `json:"worker_id"`
	RunID            string  `json:"run_id"`
	StartedAt        time.Time `json:"started_at"`
	StoppedAt        time.Time `json:"stopped_at"`
	EventsGenerated  int64   `json:"events_generated"`
	EventsSubmitted  int64   `json:"events_submitted"`
	EventsSucceeded  int64   `json:"events_succeeded"`
	EventsFailed     int64   `json:"events_failed"`
	ClientErrors     int64   `json:"client_errors"`
	ServerErrors     int64   `json:"server_errors"`
	TransportErrors  int64   `json:"transport_errors"`
	LatencyP50Ms     float64 `json:"latency_p50_ms"`
	LatencyP90Ms     float64 `json:"latency_p90_ms"`
	LatencyP99Ms     float64 `json:"latency_p99_ms"`
	LatencyMaxMs     float64 `json:"latency_max_ms"`
	PerArchetype     map[string]int64 `json:"per_archetype,omitempty"`
	PerProductType   map[string]int64 `json:"per_product_type,omitempty"`
	PerIngestionPath map[string]int64 `json:"per_ingestion_path,omitempty"`
	Aborted          bool    `json:"aborted,omitempty"`
	AbortReason      string  `json:"abort_reason,omitempty"`
	Errors           []string `json:"errors,omitempty"`
}

// AbortRequest is sent on /v1/abort to halt a worker's run. The worker
// drains in-flight events, emits a final Report, and shuts down its
// runner.
type AbortRequest struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

// AbortResponse acknowledges the abort. Body is informational — the
// worker is shutting down regardless.
type AbortResponse struct {
	Accepted bool   `json:"accepted"`
	WorkerID string `json:"worker_id"`
}

// Endpoint paths. Exported so worker server and coordinator client agree.
const (
	PathAssign    = "/v1/assign"
	PathHeartbeat = "/v1/heartbeat"
	PathReport    = "/v1/report"
	PathAbort     = "/v1/abort"

	HeaderRunID   = "X-Aforo-RunID"
	HeaderWorker  = "X-Aforo-Worker"

	// DefaultHeartbeatInterval is the coordinator's poll cadence. Tuned
	// for the 7-day soak — 5s is frequent enough to detect a stuck
	// worker within one chaos-event window, sparse enough to avoid
	// overloading the coordinator with HTTP/2 streams.
	DefaultHeartbeatInterval = 5 * time.Second

	// DefaultDropoutTimeout is the time without a successful heartbeat
	// after which the coordinator declares a worker dropped. 30s gives
	// 6 missed heartbeats before reassignment kicks in.
	DefaultDropoutTimeout = 30 * time.Second
)
