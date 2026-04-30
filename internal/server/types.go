package server

import "time"

// RunStatus values mirror loadgen_runs.status — keep in sync with the
// Supabase migration when adding new states.
type RunStatus string

const (
	RunStatusQueued    RunStatus = "queued"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

// RunOutcome is the operator-facing assertion verdict — distinct from
// status because a run can complete (status=completed) but still fail
// its assertions (overall_outcome=failed).
type RunOutcome string

const (
	OutcomePass RunOutcome = "pass"
	OutcomeFail RunOutcome = "fail"
	OutcomeSkip RunOutcome = "skip"
)

// TriggerRequest is the body of POST /api/v1/runs. Fields not set fall
// back to scenario defaults; --duration overrides the scenario YAML.
//
// ManifestPath is intentionally NOT exposed over the HTTP API — the
// loadgen server uses its server-side --manifest-path flag (or the
// default `manifest.json` relative to the work-dir). Allowing the
// client to pass an arbitrary filesystem path would let a
// platform_admin point the worker at /etc/passwd or similar and
// have the run engine try to parse it. Manifests are a deploy-time
// config item, not a per-request choice.
type TriggerRequest struct {
	Scenario     string `json:"scenario"`
	Target       string `json:"target"`
	DurationSecs int    `json:"duration_secs,omitempty"`
	Workers      int    `json:"workers,omitempty"`
	Acknowledge  bool   `json:"acknowledge_high_tps,omitempty"`
}

// TriggerResponse is the 202 body — operators poll detail endpoint.
type TriggerResponse struct {
	RunID  string `json:"run_id"`
	Status RunStatus `json:"status"`
}

// Run is the index row + summary used by list and detail endpoints.
// JSONB blobs are typed as map[string]any for flexibility; the
// Control Tower view types narrow them to the shapes the CLI emits.
type Run struct {
	RunID                string         `json:"run_id"`
	Scenario             string         `json:"scenario"`
	Target               string         `json:"target"`
	Status               RunStatus      `json:"status"`
	TriggeredBy          string         `json:"triggered_by,omitempty"`
	StartedAt            time.Time      `json:"started_at"`
	EndedAt              *time.Time     `json:"ended_at,omitempty"`
	P99Ms                int            `json:"p99_ms,omitempty"`
	EventsSent           int64          `json:"events_sent,omitempty"`
	EventsSucceeded      int64          `json:"events_succeeded,omitempty"`
	OverallOutcome       RunOutcome     `json:"overall_outcome,omitempty"`
	ManifestS3Path       string         `json:"manifest_s3_path,omitempty"`
	GrafanaURL           string         `json:"grafana_url,omitempty"`
	PerArchetypeSummary  map[string]any `json:"per_archetype_summary,omitempty"`
	PerNegativePathStats map[string]any `json:"per_negative_path_stats,omitempty"`
	Assertions           []Assertion    `json:"assertions,omitempty"`
}

// Assertion mirrors the validate-oracle output for a single check.
type Assertion struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"` // pass | fail | skip
	Message string `json:"message,omitempty"`
}

// ListResponse paginates Run rows for GET /api/v1/runs.
type ListResponse struct {
	Runs    []Run `json:"runs"`
	Total   int   `json:"total"`
	Page    int   `json:"page"`
	PerPage int   `json:"per_page"`
}

// ScenarioInfo is the lightweight catalog row returned by GET /scenarios.
type ScenarioInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TargetTPS   int    `json:"target_tps"`
	DurationSecs int   `json:"duration_secs"`
	Tenants     int    `json:"tenants"`
	HighTPS     bool   `json:"high_tps"` // true when target_tps > 1000 — UI shows confirmation modal
}

// HealthResponse is the GET /health body.
type HealthResponse struct {
	Status     string            `json:"status"`
	Version    string            `json:"version,omitempty"`
	ActiveRuns int               `json:"active_runs"`
	Storage    string            `json:"storage"` // "local-fs" | "s3"
	Index      string            `json:"index"`   // "supabase" | "memory"
	Components map[string]string `json:"components,omitempty"`
}

// HighTPSThreshold is the cutoff above which the trigger UI prompts
// for an explicit acknowledgement. Anything above this can saturate
// staging and risk a noisy-neighbor incident on shared infra.
const HighTPSThreshold = 1000
