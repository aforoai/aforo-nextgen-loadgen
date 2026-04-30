//go:build e2e
// +build e2e

// Package e2e is the tag-gated end-to-end test for `aforo-loadgen e2e`.
// It is excluded from the default `go test ./...` build by the //go:build
// e2e tag — only `go test -tags=e2e ./test/e2e/...` (or `make e2e-test`)
// will compile and run it.
//
// What this asserts:
//
//   1. The binary builds.
//   2. doctor exits 0 against the local docker stack.
//   3. e2e --include-billing --include-lifecycle finishes inside its
//      time budget AND emits an e2e.json with overall=PASS.
//
// What this does NOT assert:
//
//   - Specific event counts or billing math: those are validate's job,
//     and the e2e flow already runs validate. We only assert the
//     orchestrator's overall verdict.
//   - That every individual gateway/SDK ingestion path is healthy:
//     the crawl-e2e scenario exercises only rest_direct + sdk_node by
//     design (Session 7 deliverable).
//
// Pre-conditions (all environment-driven):
//
//   AFORO_ADMIN_TOKEN  — required. Skip with t.Skip() if absent.
//   AFORO_E2E_TARGET   — optional, default "local". Override to point
//                        at a non-local target (review env, staging).
//   AFORO_E2E_TIMEOUT  — optional, default "12m". Hard timeout.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findRepoRoot walks up from the test file's location until it finds
// go.mod, then returns that directory. We need this because `go test`
// runs in the test package's directory and the binary build needs the
// module root.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod walking up from %s", dir)
		}
		dir = parent
	}
}

func requireToken(t *testing.T) string {
	t.Helper()
	tok := os.Getenv("AFORO_ADMIN_TOKEN")
	if tok == "" {
		t.Skip("AFORO_ADMIN_TOKEN not set — skipping e2e test (assumes local Docker stack + admin token)")
	}
	return tok
}

func target() string {
	if v := os.Getenv("AFORO_E2E_TARGET"); v != "" {
		return v
	}
	return "local"
}

func e2eTimeout() time.Duration {
	if v := os.Getenv("AFORO_E2E_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 12 * time.Minute
}

// buildBinary compiles the CLI into a t.TempDir() so each test starts
// from a known-good binary. Reuses the existing make target's flags so
// the tested binary matches what `make build` produces.
func buildBinary(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aforo-loadgen")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/aforo-loadgen")
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestE2E_DoctorPassesAgainstLocalStack(t *testing.T) {
	requireToken(t)
	repoRoot := findRepoRoot(t)
	bin := buildBinary(t, repoRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "doctor", "--target", target())
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed (assumes Docker stack is up):\n%s\nerror: %v", out, err)
	}
	if !strings.Contains(string(out), "summary:") {
		t.Errorf("doctor output missing 'summary:' line:\n%s", out)
	}
}

func TestE2E_CrawlScenarioCompletesPass(t *testing.T) {
	requireToken(t)
	repoRoot := findRepoRoot(t)
	bin := buildBinary(t, repoRoot)

	outDir := filepath.Join(t.TempDir(), fmt.Sprintf("e2e-test-%d", time.Now().Unix()))
	timeout := e2eTimeout()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{
		"e2e",
		"--scenario", "scenarios/crawl-e2e.yaml",
		"--target", target(),
		"--out", outDir,
		"--include-billing",
		"--include-lifecycle",
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	t.Logf("e2e completed in %s\nstdout/err:\n%s", elapsed, out)

	// Even on failure, e2e.json must exist — orchestrator writes it on
	// the way out via deferred Save.
	e2eJSON := filepath.Join(outDir, "e2e.json")
	data, readErr := os.ReadFile(e2eJSON)
	if readErr != nil {
		t.Fatalf("e2e.json missing — orchestrator must write it even on failure: %v\nstdout:\n%s", readErr, out)
	}
	var summary struct {
		Overall string `json:"overall"`
		Stages  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Err    string `json:"error,omitempty"`
		} `json:"stages"`
		Elapsed time.Duration `json:"elapsed_ms"`
	}
	if jerr := json.Unmarshal(data, &summary); jerr != nil {
		t.Fatalf("e2e.json malformed: %v\n%s", jerr, data)
	}

	if err != nil {
		// Surface every failed stage's error so CI logs are actionable.
		var failures []string
		for _, s := range summary.Stages {
			if s.Status == "FAIL" {
				failures = append(failures, fmt.Sprintf("%s: %s", s.Name, s.Err))
			}
		}
		t.Fatalf("e2e exited non-zero: %v\nfailed stages:\n%s\nfull stdout:\n%s",
			err, strings.Join(failures, "\n"), out)
	}

	if summary.Overall != "PASS" {
		t.Fatalf("e2e summary overall = %q, want PASS\nstages: %#v", summary.Overall, summary.Stages)
	}
	// Session 7 budget: < 10 minutes on a healthy local stack.
	const budget = 10 * time.Minute
	if elapsed > budget {
		t.Errorf("e2e took %s (budget: %s); investigate slowdown", elapsed.Round(time.Second), budget)
	}
}
