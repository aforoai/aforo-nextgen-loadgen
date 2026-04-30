package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// LocalRunner spawns `aforo-loadgen run` as a child process per
// trigger. The child writes a canonical run.json under its OutputDir;
// LocalRunner reads it on completion to extract metrics + assertions.
//
// The runner re-execs the SAME binary as the server itself — that
// guarantees scenario catalogs and run engine versions match exactly,
// which is critical for reproducibility.
type LocalRunner struct {
	BinaryPath  string
	WorkDir     string
	ScenariosBuiltIn []string // names accepted directly without --scenario path
}

// NewLocalRunner picks the binary path (defaults to os.Args[0]) and
// validates the work directory exists.
func NewLocalRunner(workDir, binaryPath string, builtins []string) (*LocalRunner, error) {
	if workDir == "" {
		return nil, errors.New("work directory required")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	if binaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		binaryPath = exe
	}
	return &LocalRunner{BinaryPath: binaryPath, WorkDir: workDir, ScenariosBuiltIn: builtins}, nil
}

// Spawn launches the worker subprocess. The returned ProcHandle gives
// the caller a way to wait for completion and request cancellation.
//
// Cancellation: invoking cancel() sends SIGINT to the process; the
// run engine's signal handler drains in-flight work and writes a
// partial manifest before exiting. The Wait() call returns when the
// child has exited — partial output is already on disk.
func (l *LocalRunner) Spawn(_ context.Context, runID string, req TriggerRequest, manifest string) (*ProcHandle, error) {
	if !validRunID(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	outDir := filepath.Join(l.WorkDir, runID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir run dir: %w", err)
	}

	scenarioFlag := req.Scenario
	if !isBuiltinName(l.ScenariosBuiltIn, scenarioFlag) {
		// Caller is referencing a path on the server filesystem — the
		// server explicitly does NOT accept arbitrary scenario YAML
		// over the wire (CLI flag --allow-arbitrary-scenarios=false
		// by default). Reject so we never read /etc/passwd as YAML.
		return nil, fmt.Errorf("scenario %q is not a built-in; server does not accept ad-hoc scenarios over HTTP", req.Scenario)
	}

	args := []string{"run",
		"--scenario", scenarioFlag,
		"--target", req.Target,
		"--manifest", manifest,
		"--out", outDir,
		"--metrics-addr", "", // child doesn't expose metrics; the server already does
	}
	if req.DurationSecs > 0 {
		args = append(args, "--duration", fmt.Sprintf("%ds", req.DurationSecs))
	}
	if req.Workers > 0 {
		args = append(args, "--workers", fmt.Sprintf("%d", req.Workers))
	}

	// Detached context so the child outlives the HTTP request that
	// triggered it. Cancellation is via SIGINT below, not context.
	bgCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(bgCtx, l.BinaryPath, args...) //nolint:gosec // binary path is server-controlled

	logFile, err := os.OpenFile(filepath.Join(outDir, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		cancel()
		return nil, fmt.Errorf("start worker: %w", err)
	}

	h := &ProcHandle{
		runID:   runID,
		outDir:  outDir,
		cmd:     cmd,
		log:     logFile,
		ctxStop: cancel,
		done:    make(chan error, 1),
	}
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		h.done <- err
		close(h.done)
	}()
	return h, nil
}

// ProcHandle is a control + observation handle for a spawned worker.
type ProcHandle struct {
	runID   string
	outDir  string
	cmd     *exec.Cmd
	log     *os.File
	ctxStop context.CancelFunc

	doneOnce sync.Once
	done     chan error
}

// OutputDir returns the run's artifact directory (where run.json lives).
func (p *ProcHandle) OutputDir() string { return p.outDir }

// Wait blocks until the worker exits and returns the exit error (nil
// on graceful completion, non-nil on crash or non-zero exit).
func (p *ProcHandle) Wait() error { return <-p.done }

// Cancel sends SIGINT to the worker. The run engine's signal handler
// then drains, writes partial output, and exits 0 within a few
// seconds. After Cancel, callers should still Wait() for clean
// teardown.
func (p *ProcHandle) Cancel() error {
	if p.cmd.Process == nil {
		return errors.New("worker has not started")
	}
	// Best-effort — the process may have exited between our Wait
	// goroutine's read and this call.
	_ = p.cmd.Process.Signal(os.Interrupt)
	return nil
}

// Kill is a hard SIGKILL — only used when graceful Cancel times out.
// The doneOnce guard means a second Kill is a no-op even if Cancel
// already nudged the worker.
func (p *ProcHandle) Kill() {
	p.doneOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		p.ctxStop()
	})
}

// ReadRunJSON reads the canonical run.json artifact written by the
// run engine. The shape is whatever cmd/aforo-loadgen/run produces;
// the server treats it as opaque JSON to forward to the index.
func (p *ProcHandle) ReadRunJSON() (RunArtifact, error) {
	path := filepath.Join(p.outDir, "run.json")
	f, err := os.Open(path)
	if err != nil {
		return RunArtifact{}, err
	}
	defer f.Close()
	var art RunArtifact
	if err := json.NewDecoder(f).Decode(&art); err != nil {
		return RunArtifact{}, err
	}
	return art, nil
}

// CopyRunJSON streams run.json to dst. Used to push manifest into
// the manifest store after completion.
func (p *ProcHandle) CopyRunJSON(dst io.Writer) error {
	path := filepath.Join(p.outDir, "run.json")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}

// RunArtifact is the subset of run.json the server consumes to
// populate the index row. The runner emits many more fields; this
// struct intentionally takes only what the operator UI reads, so
// new run-engine fields don't require a server release.
type RunArtifact struct {
	EventsGenerated int64                  `json:"events_generated"`
	EventsSubmitted int64                  `json:"events_submitted"`
	EventsSucceeded int64                  `json:"events_succeeded"`
	LatencyP99ms    float64                `json:"latency_p99_ms"`
	LatencyP90ms    float64                `json:"latency_p90_ms"`
	LatencyP50ms    float64                `json:"latency_p50_ms"`
	NegativePathCounts map[string]int64    `json:"negative_path_counts"`
	PerArchetype       map[string]any      `json:"per_archetype_stats"`
	Assertions         []ArtifactAssertion `json:"assertions"`
	Outcome            string              `json:"overall_outcome"`
}

// ArtifactAssertion is the per-check entry in run.json.
type ArtifactAssertion struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

func isBuiltinName(builtins []string, name string) bool {
	for _, b := range builtins {
		if b == name {
			return true
		}
	}
	return false
}

// genRunID generates a server-assigned run id with format
// <scenario>-<unix-ts>-<8-char-hex>. The hex tail prevents collisions
// when multiple operators trigger the same scenario in the same second.
func genRunID(scenario string) string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// extremely unlikely; fall through with an empty tail rather
		// than panic on the request hot path.
		return fmt.Sprintf("%s-%d", sanitizeScenario(scenario), time.Now().Unix())
	}
	return fmt.Sprintf("%s-%d-%s", sanitizeScenario(scenario), time.Now().Unix(), hex.EncodeToString(buf))
}

// sanitizeScenario lowercases + strips characters disallowed by
// validRunID. Scenario names in the catalog are already safe but we
// guard defensively in case a future scenario name introduces an
// uppercase or punctuation character.
func sanitizeScenario(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-' || c == '_':
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "run"
	}
	return string(out)
}
