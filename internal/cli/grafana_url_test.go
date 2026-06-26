package cli

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestBuildGrafanaURLFn_EmptyBaseReturnsNil(t *testing.T) {
	if fn := buildGrafanaURLFn(""); fn != nil {
		t.Fatalf("empty base must yield a nil function, got non-nil")
	}
	if fn := buildGrafanaURLFn("   "); fn != nil {
		t.Fatalf("blank base must yield a nil function (TrimRight only trims slashes; blank stays blank), got non-nil")
	}
}

func TestBuildGrafanaURLFn_LiveRunTracksToNow(t *testing.T) {
	fn := buildGrafanaURLFn("https://grafana.aforo.ai/")
	if fn == nil {
		t.Fatal("non-empty base must yield a function")
	}
	start := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)

	u := mustParse(t, fn("ci-smoke-1718-abcd", start, nil))
	q := u.Query()

	if got := q.Get("var-runId"); got != "ci-smoke-1718-abcd" {
		t.Errorf("var-runId = %q, want the runID", got)
	}
	wantFrom := start.Add(-grafanaWindowMargin).UnixMilli()
	if got := q.Get("from"); got != strconv.FormatInt(wantFrom, 10) {
		t.Errorf("from = %q, want %d (start - margin, epoch ms)", got, wantFrom)
	}
	if got := q.Get("to"); got != "now" {
		t.Errorf("to = %q, want \"now\" for an in-flight run", got)
	}
	// Trailing slash on base must be trimmed (no double slash).
	if u.Path != "/d/loadgen-run/loadgen-run" {
		t.Errorf("path = %q, want /d/loadgen-run/loadgen-run", u.Path)
	}
}

func TestBuildGrafanaURLFn_CompletedRunLocksWindow(t *testing.T) {
	fn := buildGrafanaURLFn("https://grafana.aforo.ai")
	start := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	u := mustParse(t, fn("run-x", start, &end))
	q := u.Query()

	wantFrom := start.Add(-grafanaWindowMargin).UnixMilli()
	wantTo := end.Add(grafanaWindowMargin).UnixMilli()
	if got := q.Get("from"); got != strconv.FormatInt(wantFrom, 10) {
		t.Errorf("from = %q, want %d", got, wantFrom)
	}
	if got := q.Get("to"); got != strconv.FormatInt(wantTo, 10) {
		t.Errorf("to = %q, want %d (locked end + margin), not \"now\"", got, wantTo)
	}
}

func TestBuildGrafanaURLFn_ZeroStartOmitsFrom(t *testing.T) {
	fn := buildGrafanaURLFn("https://grafana.aforo.ai")
	u := mustParse(t, fn("run-y", time.Time{}, nil))
	q := u.Query()
	if q.Has("from") {
		t.Errorf("zero start time must omit the from param, got %q", q.Get("from"))
	}
	if got := q.Get("to"); got != "now" {
		t.Errorf("to = %q, want \"now\"", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
