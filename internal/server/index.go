package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunsIndex is the durable list/detail store for runs. Two impls:
//
//   - SupabaseIndex: PostgREST against the loadgen_runs table.
//     Production default.
//   - MemoryIndex: in-memory; default when --supabase-url is empty
//     (local dev) and used by tests.
//
// Operations:
//   - Insert is called when the worker subprocess is spawned; status=queued
//   - Update is called on every status transition (queued→running→...)
//   - Get/List back the read endpoints
type RunsIndex interface {
	Insert(ctx context.Context, r Run) error
	Update(ctx context.Context, r Run) error
	Get(ctx context.Context, id string) (*Run, error)
	List(ctx context.Context, q ListQuery) (*ListResponse, error)
}

// ListQuery filters and paginates a List call. Zero values are
// permissive (no filter). PerPage is clamped server-side.
type ListQuery struct {
	Status   string
	Scenario string
	Page     int
	PerPage  int
}

// ─────────────────────── Memory ───────────────────────

// MemoryIndex is a thread-safe in-memory RunsIndex. Used for dev,
// tests, and when --supabase-url is empty.
type MemoryIndex struct {
	mu   sync.RWMutex
	rows map[string]Run
}

// NewMemoryIndex builds an empty memory index.
func NewMemoryIndex() *MemoryIndex { return &MemoryIndex{rows: make(map[string]Run)} }

func (m *MemoryIndex) Insert(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[r.RunID]; exists {
		return fmt.Errorf("run already exists: %s", r.RunID)
	}
	m.rows[r.RunID] = r
	return nil
}

func (m *MemoryIndex) Update(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[r.RunID]; !exists {
		return fmt.Errorf("run not found: %s", r.RunID)
	}
	m.rows[r.RunID] = r
	return nil
}

func (m *MemoryIndex) Get(_ context.Context, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &r, nil
}

func (m *MemoryIndex) List(_ context.Context, q ListQuery) (*ListResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Run, 0, len(m.rows))
	for _, r := range m.rows {
		if q.Status != "" && string(r.Status) != q.Status {
			continue
		}
		if q.Scenario != "" && r.Scenario != q.Scenario {
			continue
		}
		out = append(out, r)
	}
	// Newest first by started_at.
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	total := len(out)
	page, perPage := normalizePaging(q.Page, q.PerPage)
	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return &ListResponse{Runs: out[start:end], Total: total, Page: page, PerPage: perPage}, nil
}

// ErrNotFound is returned by Get/List when the run does not exist.
var ErrNotFound = errors.New("run not found")

func normalizePaging(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	switch {
	case perPage <= 0:
		perPage = 25
	case perPage > 100:
		perPage = 100
	}
	return page, perPage
}

// ─────────────────────── Supabase ───────────────────────

// SupabaseIndex is the PostgREST implementation. All writes go through
// the service-role key so RLS doesn't intercept them.
type SupabaseIndex struct {
	URL            string
	ServiceRoleKey string
	HTTPClient     *http.Client
}

// NewSupabaseIndex builds a configured PostgREST client. Empty url
// returns nil so callers can opt into MemoryIndex transparently.
func NewSupabaseIndex(supabaseURL, serviceRoleKey string) (*SupabaseIndex, error) {
	if supabaseURL == "" {
		return nil, errors.New("supabase url required")
	}
	if serviceRoleKey == "" {
		return nil, errors.New("service role key required")
	}
	return &SupabaseIndex{
		URL:            strings.TrimRight(supabaseURL, "/"),
		ServiceRoleKey: serviceRoleKey,
		HTTPClient:     &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// loadgen_runs row shape — JSON tags match the column names so PostgREST
// upserts work without a translation layer.
type runRow struct {
	RunID                string         `json:"run_id"`
	Scenario             string         `json:"scenario"`
	Target               string         `json:"target"`
	Status               string         `json:"status"`
	TriggeredBy          *string        `json:"triggered_by,omitempty"`
	StartedAt            time.Time      `json:"started_at"`
	EndedAt              *time.Time     `json:"ended_at,omitempty"`
	P99Ms                *int           `json:"p99_ms,omitempty"`
	EventsSent           *int64         `json:"events_sent,omitempty"`
	EventsSucceeded      *int64         `json:"events_succeeded,omitempty"`
	OverallOutcome       *string        `json:"overall_outcome,omitempty"`
	ManifestS3Path       *string        `json:"manifest_s3_path,omitempty"`
	GrafanaURL           *string        `json:"grafana_url,omitempty"`
	PerArchetypeSummary  map[string]any `json:"per_archetype_summary,omitempty"`
	PerNegativePathStats map[string]any `json:"per_negative_path_stats,omitempty"`
	Assertions           []Assertion    `json:"assertions,omitempty"`
}

func toRow(r Run) runRow {
	row := runRow{
		RunID:                r.RunID,
		Scenario:             r.Scenario,
		Target:               r.Target,
		Status:               string(r.Status),
		StartedAt:            r.StartedAt,
		EndedAt:              r.EndedAt,
		PerArchetypeSummary:  r.PerArchetypeSummary,
		PerNegativePathStats: r.PerNegativePathStats,
		Assertions:           r.Assertions,
	}
	if r.TriggeredBy != "" {
		t := r.TriggeredBy
		row.TriggeredBy = &t
	}
	if r.P99Ms > 0 {
		v := r.P99Ms
		row.P99Ms = &v
	}
	if r.EventsSent > 0 {
		v := r.EventsSent
		row.EventsSent = &v
	}
	if r.EventsSucceeded > 0 {
		v := r.EventsSucceeded
		row.EventsSucceeded = &v
	}
	if r.OverallOutcome != "" {
		v := string(r.OverallOutcome)
		row.OverallOutcome = &v
	}
	if r.ManifestS3Path != "" {
		v := r.ManifestS3Path
		row.ManifestS3Path = &v
	}
	if r.GrafanaURL != "" {
		v := r.GrafanaURL
		row.GrafanaURL = &v
	}
	return row
}

func fromRow(row runRow) Run {
	out := Run{
		RunID:                row.RunID,
		Scenario:             row.Scenario,
		Target:               row.Target,
		Status:               RunStatus(row.Status),
		StartedAt:            row.StartedAt,
		EndedAt:              row.EndedAt,
		PerArchetypeSummary:  row.PerArchetypeSummary,
		PerNegativePathStats: row.PerNegativePathStats,
		Assertions:           row.Assertions,
	}
	if row.TriggeredBy != nil {
		out.TriggeredBy = *row.TriggeredBy
	}
	if row.P99Ms != nil {
		out.P99Ms = *row.P99Ms
	}
	if row.EventsSent != nil {
		out.EventsSent = *row.EventsSent
	}
	if row.EventsSucceeded != nil {
		out.EventsSucceeded = *row.EventsSucceeded
	}
	if row.OverallOutcome != nil {
		out.OverallOutcome = RunOutcome(*row.OverallOutcome)
	}
	if row.ManifestS3Path != nil {
		out.ManifestS3Path = *row.ManifestS3Path
	}
	if row.GrafanaURL != nil {
		out.GrafanaURL = *row.GrafanaURL
	}
	return out
}

func (s *SupabaseIndex) Insert(ctx context.Context, r Run) error {
	body, err := json.Marshal(toRow(r))
	if err != nil {
		return err
	}
	return s.do(ctx, http.MethodPost, "/rest/v1/loadgen_runs", body, "Prefer", "return=minimal", nil)
}

func (s *SupabaseIndex) Update(ctx context.Context, r Run) error {
	body, err := json.Marshal(toRow(r))
	if err != nil {
		return err
	}
	q := url.Values{"run_id": {"eq." + r.RunID}}
	return s.do(ctx, http.MethodPatch, "/rest/v1/loadgen_runs?"+q.Encode(), body, "Prefer", "return=minimal", nil)
}

func (s *SupabaseIndex) Get(ctx context.Context, id string) (*Run, error) {
	q := url.Values{"run_id": {"eq." + id}, "select": {"*"}}
	var rows []runRow
	if err := s.do(ctx, http.MethodGet, "/rest/v1/loadgen_runs?"+q.Encode(), nil, "", "", &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	out := fromRow(rows[0])
	return &out, nil
}

func (s *SupabaseIndex) List(ctx context.Context, q ListQuery) (*ListResponse, error) {
	page, perPage := normalizePaging(q.Page, q.PerPage)
	rangeStart := (page - 1) * perPage
	rangeEnd := rangeStart + perPage - 1

	v := url.Values{"select": {"*"}, "order": {"started_at.desc"}}
	if q.Status != "" {
		v.Set("status", "eq."+q.Status)
	}
	if q.Scenario != "" {
		v.Set("scenario", "eq."+q.Scenario)
	}
	urlStr := "/rest/v1/loadgen_runs?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", s.ServiceRoleKey)
	req.Header.Set("Authorization", "Bearer "+s.ServiceRoleKey)
	req.Header.Set("Range-Unit", "items")
	req.Header.Set("Range", fmt.Sprintf("%d-%d", rangeStart, rangeEnd))
	req.Header.Set("Prefer", "count=exact")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("postgrest %d: %s", resp.StatusCode, string(body))
	}

	var rows []runRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	out := &ListResponse{Runs: make([]Run, 0, len(rows)), Page: page, PerPage: perPage}
	for _, row := range rows {
		out.Runs = append(out.Runs, fromRow(row))
	}
	out.Total = parseContentRangeTotal(resp.Header.Get("Content-Range"), len(rows))
	return out, nil
}

// parseContentRangeTotal extracts the trailing count from a PostgREST
// "0-24/123" Content-Range header. Returns fallback when missing or
// malformed (e.g. "*" — count not requested).
func parseContentRangeTotal(header string, fallback int) int {
	idx := strings.LastIndex(header, "/")
	if idx < 0 || idx == len(header)-1 {
		return fallback
	}
	tail := header[idx+1:]
	n := 0
	for _, c := range tail {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func (s *SupabaseIndex) do(ctx context.Context, method, path string, body []byte, hdrKey, hdrVal string, out any) error {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", s.ServiceRoleKey)
	req.Header.Set("Authorization", "Bearer "+s.ServiceRoleKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdrKey != "" {
		req.Header.Set(hdrKey, hdrVal)
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("postgrest %d %s: %s", resp.StatusCode, path, string(body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}
	return nil
}
