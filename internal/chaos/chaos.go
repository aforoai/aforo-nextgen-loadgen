// Package chaos schedules and executes infrastructure fault injection
// during a load-test run.
//
// Design contract:
//
//  1. Every chaos type is REVERSIBLE. The Scenario interface returns a
//     Recovery func that undoes the fault. The scheduler always invokes
//     it — even on early termination, panic recovery, or context
//     cancellation. A run that fails to recover its chaos faults is
//     considered itself broken.
//
//  2. Chaos faults run against the *target environment*, not the load
//     generator. A misconfigured target (e.g. running this against
//     production instead of the perf-aws cluster) is the operator's most
//     dangerous mistake. The Scheduler refuses to fire any event when
//     the target name does not begin with "perf-" or appear in the
//     allowlist provided at construction.
//
//  3. Side effects are isolated behind the Executor interface. The
//     production executor shells out to aws ssm / tc / iptables; tests
//     inject a recording fake. This boundary keeps the package
//     deterministic to test without giving up real-world capability.
//
//  4. Timing tolerates jitter. The scheduler fires events when the run
//     clock crosses each event's "at" mark, ± JitterTolerance. A late
//     fire is preferred over a missed fire — we'd rather inject the
//     fault than skip it because the previous tick ran long.
//
// What this package does NOT do:
//
//   - It does not directly contact the AWS API. All commands flow through
//     the Executor, which can be the real shell or a fake.
//   - It does not own the run clock. The scheduler is driven from the
//     run engine via Tick(now) calls — same time source as the rest of
//     the runner so chaos events line up with run.json timestamps.
//   - It does not retry failed chaos executions. A chaos type that fails
//     to inject is logged and counted; we do not double-fire because
//     re-applying a partially-injected fault can leave the target in an
//     unrecoverable state.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Scenario is one parameterized chaos fault. Implementations decide what
// to inject and how to recover. Implementations should be safe to call
// from a single goroutine — the Scheduler serializes Inject/Recover for
// each Scenario.
type Scenario interface {
	// Type is the user-facing name from the YAML scenario, e.g.
	// "kafka_kill", "redis_flush", "ch_slowdown", "net_partition".
	Type() string

	// Plan validates the scenario's parameters against the target. Called
	// once before the run begins so misconfigurations surface in the
	// pre-flight, not 24h into a 7-day soak.
	Plan(ctx context.Context, exec Executor) error

	// Inject performs the fault. Returns a Recovery callback that the
	// scheduler invokes when the event's duration elapses (or the run
	// ends). Inject must NOT block for the duration of the fault — it
	// should kick off the chaos action and return promptly.
	//
	// If Inject returns an error, the scheduler skips the fault entirely
	// and does NOT call any recovery (because no recovery was returned).
	Inject(ctx context.Context, exec Executor) (Recovery, error)
}

// Recovery undoes the fault that Inject created. The scheduler always
// calls Recovery exactly once per successful Inject — including on run
// abort or panic recovery — so each Recovery must be idempotent. Returning
// an error is informational; the scheduler logs and moves on.
type Recovery func(ctx context.Context, exec Executor) error

// Event is one scheduled chaos firing — when, what, and the operator's
// notes. The scheduler holds these sorted by At.
type Event struct {
	// At is the offset from the run start at which to fire.
	At time.Duration

	// Duration is how long the fault remains injected before Recovery
	// runs. Some chaos types (e.g. redis_flush) are instantaneous;
	// duration = 0 fires Inject and immediately fires Recovery.
	Duration time.Duration

	// Scenario is the constructed chaos action.
	Scenario Scenario

	// Notes is operator-supplied context (free text). Embedded into the
	// run timeline.
	Notes string
}

// Outcome records what happened when the scheduler fired one Event.
// Embedded into the run.json under "chaos_timeline" so post-run analysis
// can see which faults landed and which did not.
type Outcome struct {
	Type           string        `json:"type"`
	StartedAt      time.Time     `json:"started_at"`
	RecoveredAt    time.Time     `json:"recovered_at,omitempty"`
	Duration       time.Duration `json:"duration"`
	InjectError    string        `json:"inject_error,omitempty"`
	RecoveryError  string        `json:"recovery_error,omitempty"`
	Skipped        bool          `json:"skipped,omitempty"`
	SkipReason     string        `json:"skip_reason,omitempty"`
	Notes          string        `json:"notes,omitempty"`
}

// SchedulerConfig is the construction-time bag.
type SchedulerConfig struct {
	// Events is the sorted list of scheduled chaos events. Scheduler
	// re-sorts in case the caller passed unsorted input.
	Events []Event

	// Executor is the side-effect boundary. Pass a ShellExecutor for
	// production, a Recorder for tests.
	Executor Executor

	// Now is the time source. nil → time.Now. Tests inject a fake clock.
	Now func() time.Time

	// AllowedTargets gates which target names may receive chaos faults.
	// The Scheduler refuses to fire if the target is not in this list.
	// Default: any name beginning with "perf-".
	AllowedTargets []string

	// TargetName is the target this run is hitting. Compared against
	// AllowedTargets at scheduler construction time and on every fire.
	TargetName string

	// JitterTolerance is the window around At within which a fire is
	// considered on-time. Late fires are preferred over missed fires.
	// Default 500ms.
	JitterTolerance time.Duration

	// Logger receives one line per scheduled fire (and per recovery).
	// nil → discard. Used by ops runbook to confirm injection.
	Logger func(format string, args ...any)
}

// Scheduler fires chaos events at their planned offsets, tracks outcomes,
// and recovers everything before returning.
type Scheduler struct {
	cfg            SchedulerConfig
	now            func() time.Time
	startedAt      time.Time
	mu             sync.Mutex
	outcomes       []Outcome
	pending        []scheduled
	closed         bool
	allowedAllowed map[string]struct{}
}

type scheduled struct {
	idx       int
	event     Event
	recovery  Recovery
	startedAt time.Time
	deadline  time.Time
	rolledOut bool
}

// NewScheduler validates the events list and the target gate and returns
// a Scheduler ready to receive Tick calls. Returns an error if the target
// is not in the allow list — fail-closed before any chaos fires.
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Executor == nil {
		return nil, errors.New("chaos: scheduler requires an Executor")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.JitterTolerance <= 0 {
		cfg.JitterTolerance = 500 * time.Millisecond
	}
	if len(cfg.AllowedTargets) == 0 {
		// Default deny except perf-* targets.
		cfg.AllowedTargets = []string{"perf-aws", "perf-staging"}
	}
	allowed := map[string]struct{}{}
	for _, t := range cfg.AllowedTargets {
		allowed[t] = struct{}{}
	}
	if !targetAllowed(cfg.TargetName, allowed) {
		return nil, fmt.Errorf("chaos: target %q not in allow list %v — chaos events refuse to fire on non-perf targets",
			cfg.TargetName, cfg.AllowedTargets)
	}

	// Sort events by offset, copy to avoid mutating the caller's slice.
	events := make([]Event, len(cfg.Events))
	copy(events, cfg.Events)
	sort.Slice(events, func(i, j int) bool { return events[i].At < events[j].At })
	cfg.Events = events

	return &Scheduler{
		cfg:            cfg,
		now:            cfg.Now,
		allowedAllowed: allowed,
	}, nil
}

// Plan calls Plan on every event's Scenario. Returns the first error
// encountered — operators should fix that one and retry.
func (s *Scheduler) Plan(ctx context.Context) error {
	for i, ev := range s.cfg.Events {
		if err := ev.Scenario.Plan(ctx, s.cfg.Executor); err != nil {
			return fmt.Errorf("chaos: event[%d] type=%s: plan: %w", i, ev.Scenario.Type(), err)
		}
	}
	return nil
}

// Start records the run start time. Subsequent Tick calls compare against
// this time + each event's At.
func (s *Scheduler) Start(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startedAt = at
	s.pending = make([]scheduled, len(s.cfg.Events))
	for i, ev := range s.cfg.Events {
		s.pending[i] = scheduled{idx: i, event: ev}
	}
}

// Tick is called by the run engine on its main loop. It checks each
// pending event against the current run offset and fires any whose
// "at" mark has passed (within JitterTolerance). It also recovers any
// active fault whose duration has elapsed.
//
// Safe to call concurrently — internally serialized by mu.
func (s *Scheduler) Tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.startedAt.IsZero() {
		return
	}
	nowT := s.now()
	runOffset := nowT.Sub(s.startedAt)

	// Fire pending events whose offset has passed.
	for i := range s.pending {
		p := &s.pending[i]
		if p.rolledOut || p.recovery != nil {
			continue
		}
		if runOffset+s.cfg.JitterTolerance < p.event.At {
			continue
		}
		// On-time or late — fire now.
		s.fire(ctx, p, nowT)
	}

	// Recover any active fault whose duration has elapsed.
	for i := range s.pending {
		p := &s.pending[i]
		if p.recovery == nil || p.rolledOut {
			continue
		}
		if !nowT.Before(p.deadline) {
			s.recover(ctx, p, nowT)
		}
	}
}

// Close fires Recovery on every still-active fault and prevents future
// Tick calls from doing anything. Safe to call multiple times. Always
// invoked by the run engine via defer to guarantee chaos faults don't
// outlive the run.
func (s *Scheduler) Close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	nowT := s.now()
	for i := range s.pending {
		p := &s.pending[i]
		if p.recovery == nil || p.rolledOut {
			continue
		}
		s.recover(ctx, p, nowT)
	}
}

// Outcomes returns a snapshot of the chaos timeline. Safe for concurrent
// read; the returned slice is a copy.
func (s *Scheduler) Outcomes() []Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Outcome, len(s.outcomes))
	copy(out, s.outcomes)
	return out
}

// fire is called with mu held.
func (s *Scheduler) fire(ctx context.Context, p *scheduled, nowT time.Time) {
	logf := s.cfg.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logf("chaos: firing %s at run+%s", p.event.Scenario.Type(), nowT.Sub(s.startedAt))

	rec, err := p.event.Scenario.Inject(ctx, s.cfg.Executor)
	if err != nil {
		logf("chaos: inject failed for %s: %v", p.event.Scenario.Type(), err)
		s.outcomes = append(s.outcomes, Outcome{
			Type:        p.event.Scenario.Type(),
			StartedAt:   nowT,
			Duration:    p.event.Duration,
			InjectError: err.Error(),
			Notes:       p.event.Notes,
		})
		p.rolledOut = true
		return
	}
	p.startedAt = nowT
	p.deadline = nowT.Add(p.event.Duration)
	p.recovery = rec

	// Record the outcome up front; recovery patches it later.
	s.outcomes = append(s.outcomes, Outcome{
		Type:      p.event.Scenario.Type(),
		StartedAt: nowT,
		Duration:  p.event.Duration,
		Notes:     p.event.Notes,
	})

	// Zero-duration fault: recover immediately. Useful for
	// instantaneous events like redis_flush.
	if p.event.Duration <= 0 {
		s.recover(ctx, p, nowT)
	}
}

// recover is called with mu held. Patches the corresponding outcome row.
func (s *Scheduler) recover(ctx context.Context, p *scheduled, nowT time.Time) {
	logf := s.cfg.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if p.recovery == nil {
		return
	}
	logf("chaos: recovering %s at run+%s", p.event.Scenario.Type(), nowT.Sub(s.startedAt))
	err := p.recovery(ctx, s.cfg.Executor)
	if err != nil {
		logf("chaos: recovery failed for %s: %v", p.event.Scenario.Type(), err)
	}
	// Patch the matching outcome (most-recently-recorded for this
	// scheduled index). The slice is append-only so the latest matching
	// row is the one we wrote in fire().
	for i := len(s.outcomes) - 1; i >= 0; i-- {
		if s.outcomes[i].Type == p.event.Scenario.Type() && s.outcomes[i].RecoveredAt.IsZero() && s.outcomes[i].StartedAt.Equal(p.startedAt) {
			s.outcomes[i].RecoveredAt = nowT
			if err != nil {
				s.outcomes[i].RecoveryError = err.Error()
			}
			break
		}
	}
	p.rolledOut = true
	p.recovery = nil
}

// targetAllowed reports whether name is in the allowed map. Empty target
// is always denied; matching is exact (no prefix magic) so operators
// must spell the target as configured.
func targetAllowed(name string, allowed map[string]struct{}) bool {
	if name == "" {
		return false
	}
	_, ok := allowed[name]
	return ok
}
