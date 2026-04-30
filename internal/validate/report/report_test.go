package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

func sampleRun() *runner.RunResult {
	return &runner.RunResult{
		RunID:         "test-run",
		ScenarioName:  "ci-smoke",
		Target:        "local",
		StartedAt:     time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		StoppedAt:     time.Date(2026, 4, 30, 10, 1, 0, 0, time.UTC),
		Duration:      time.Minute,
		TargetTPS:     50,
		EventsGenerated: 100,
		EventsSucceeded: 100,
		PerArchetype:    map[string]int64{"ar-A": 80, "ar-B": 20},
		PerTenant:       map[string]int64{"t-A": 80, "t-B": 20},
		LatencyP50ms:    1.5,
		LatencyP90ms:    3.0,
		LatencyP99ms:    7.5,
	}
}

func TestRender_WritesSelfContainedHTML(t *testing.T) {
	dir := t.TempDir()
	rep := &validate.ValidationReport{
		ReportVersion: validate.ReportVersion,
		RunID:         "test-run",
		Scenario:      "ci-smoke",
		Target:        "local",
		Checks: []*validate.CheckResult{
			validate.NewCheckResult(validate.CheckEventCount).Pass(),
			validate.NewCheckResult(validate.CheckInvariants).Pass(),
		},
	}
	rep.Finalize()

	path, err := Render(dir, sampleRun(), rep)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) // #nosec G304 — test temp file
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	// Self-contained: no external CDN references
	for _, banned := range []string{"https://fonts.googleapis.com", "https://cdn.", "https://cdnjs."} {
		if strings.Contains(html, banned) {
			t.Fatalf("HTML must be offline-safe; found banned reference: %s", banned)
		}
	}
	// CSS embedded inline
	if !strings.Contains(html, "<style>") {
		t.Fatal("expected inline <style> block")
	}
	// Headline numbers present
	for _, want := range []string{"ci-smoke", "test-run", "events generated", "1.5"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML missing expected fragment %q", want)
		}
	}
}

func TestRender_NilValidation_StillRenders(t *testing.T) {
	dir := t.TempDir()
	path, err := Render(dir, sampleRun(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestRender_NilRun_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(dir, nil, nil); err == nil {
		t.Fatal("expected error on nil RunResult")
	}
}

func TestRender_DeterministicOutput(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	run := sampleRun()
	rep := &validate.ValidationReport{Checks: []*validate.CheckResult{
		validate.NewCheckResult(validate.CheckEventCount).Pass(),
	}}
	rep.Finalize()

	p1, err := Render(dir1, run, rep)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Render(dir2, run, rep)
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := os.ReadFile(p1)
	d2, _ := os.ReadFile(p2)

	// Strip the GeneratedAt timestamp so comparisons are stable. The
	// view writes it once at the top + footer; replace both via a known
	// marker prefix.
	out1 := stripTimestamps(string(d1))
	out2 := stripTimestamps(string(d2))

	if out1 != out2 {
		t.Fatal("renderer not deterministic across calls with the same inputs")
	}
	_ = filepath.Base(p1) // keep path used
}

// stripTimestamps removes the volatile GeneratedAt rendering so two-runs
// equality checks the structural output, not the wall-clock.
func stripTimestamps(s string) string {
	const marker = "generated"
	out := s
	for {
		i := strings.Index(out, marker)
		if i < 0 {
			break
		}
		// crude: skip the next 32 chars which include the timestamp
		end := i + len(marker)
		if end+32 < len(out) {
			out = out[:i] + marker + "[STRIPPED]" + out[end+32:]
		} else {
			break
		}
	}
	return out
}
