package chaos

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Executor abstracts the side-effect surface of chaos scenarios. The
// production executor shells out to local CLI tools (aws, kubectl,
// redis-cli, tc, iptables) on the perf-cluster bastion. Tests inject a
// Recorder for deterministic, side-effect-free assertions.
//
// Run takes a "label" so outcomes can be tagged with a stable string
// regardless of the underlying command — useful for redacting the actual
// command from logs while still recording that something happened.
type Executor interface {
	// Run executes a command and returns its combined stdout/stderr.
	// Implementations may apply a default timeout to prevent a hung
	// recovery from stalling the scheduler.
	Run(ctx context.Context, label string, name string, args ...string) (string, error)
}

// ShellExecutor runs commands on the local OS via os/exec. Honors ctx
// cancellation; applies DefaultTimeout when ctx has no deadline.
type ShellExecutor struct {
	// DefaultTimeout caps any single command. 0 → 60s.
	DefaultTimeout time.Duration

	// DryRun, when true, logs the command but does not execute. Used by
	// the ops-runbook "preview" mode and by `chaos --dry-run`. Always
	// returns "" and nil error.
	DryRun bool

	// Logger receives one line per command (with the resolved label).
	// nil → discard.
	Logger func(format string, args ...any)
}

// Run implements Executor.
func (e *ShellExecutor) Run(ctx context.Context, label, name string, args ...string) (string, error) {
	if e.Logger != nil {
		e.Logger("chaos: exec %s: %s %s", label, name, strings.Join(args, " "))
	}
	if e.DryRun {
		return "", nil
	}
	timeout := e.DefaultTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("exec %s: %w (output: %s)", label, err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// Recorder is a test executor that captures every Run call without
// executing anything. Use Returns(label, output, err) to script
// per-label outputs — useful for testing chaos types whose Plan or
// Inject calls inspect command output (e.g. parsing aws ssm responses).
type Recorder struct {
	mu      sync.Mutex
	calls   []RecordedCall
	scripts map[string]scriptedReturn
}

// RecordedCall is one captured invocation.
type RecordedCall struct {
	Label string
	Name  string
	Args  []string
}

type scriptedReturn struct {
	output string
	err    error
}

// NewRecorder constructs an empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{scripts: map[string]scriptedReturn{}}
}

// Returns scripts a return value for the given label. The next Run with
// that label receives output and err.
func (r *Recorder) Returns(label, output string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts[label] = scriptedReturn{output: output, err: err}
}

// Calls returns a defensive copy of the captured calls.
func (r *Recorder) Calls() []RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// LabelsCalled returns the unique labels seen, in order of first appearance.
func (r *Recorder) LabelsCalled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]struct{}{}
	var out []string
	for _, c := range r.calls {
		if _, ok := seen[c.Label]; ok {
			continue
		}
		seen[c.Label] = struct{}{}
		out = append(out, c.Label)
	}
	return out
}

// Run implements Executor.
func (r *Recorder) Run(ctx context.Context, label, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, RecordedCall{Label: label, Name: name, Args: append([]string(nil), args...)})
	scr, ok := r.scripts[label]
	r.mu.Unlock()
	if !ok {
		return "", nil
	}
	return scr.output, scr.err
}
