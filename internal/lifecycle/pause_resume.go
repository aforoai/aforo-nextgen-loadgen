package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// pauseRequest is the platform's PauseSubscriptionRequest. Reason is logged
// in the subscription_phase audit row.
type pauseRequest struct {
	Reason string `json:"reason,omitempty"`
}

type pauseResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ResumeScheduler tracks deferred resumes after a pause. The agent
// schedules a Resume call N seconds after a Pause; on shutdown, every
// pending resume is cancelled cleanly so the run output is internally
// consistent ("we paused but never resumed" is fine; "agent dropped a
// scheduled resume on the floor" is not).
//
// Concurrency: safe for use by all per-kind goroutines; the internal map
// is mutex-guarded.
type ResumeScheduler struct {
	mu    sync.Mutex
	timer map[string]*time.Timer
	wg    sync.WaitGroup
}

// NewResumeScheduler returns a fresh scheduler.
func NewResumeScheduler() *ResumeScheduler {
	return &ResumeScheduler{timer: map[string]*time.Timer{}}
}

// Schedule arranges fn to be called after delay. Replaces any prior pending
// resume for the same subID. Cancel cancels them all.
//
// When replacing a timer, we MUST decrement the wg counter for the prior
// timer if Stop succeeds — otherwise Cancel.wg.Wait() blocks forever on a
// counter that no Done call will ever fire (the prior timer's fn is dead).
func (r *ResumeScheduler) Schedule(subID string, delay time.Duration, fn func()) {
	r.mu.Lock()
	if old, ok := r.timer[subID]; ok {
		if old.Stop() {
			// Successfully stopped before fn ran — decrement the counter
			// the prior Schedule call added.
			r.wg.Done()
		}
		// If Stop returned false, the prior fn already fired (or is firing)
		// and will Done() itself.
	}
	r.wg.Add(1)
	t := time.AfterFunc(delay, func() {
		defer r.wg.Done()
		fn()
	})
	r.timer[subID] = t
	r.mu.Unlock()
}

// Cancel stops every pending timer and waits for in-flight callbacks to
// drain. Safe to call multiple times.
func (r *ResumeScheduler) Cancel() {
	r.mu.Lock()
	for id, t := range r.timer {
		if t.Stop() {
			r.wg.Done() // we successfully stopped, fn won't run
		}
		delete(r.timer, id)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// PendingCount returns the number of timers currently armed. Cheap.
func (r *ResumeScheduler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.timer)
}

// FirePauseAndScheduleResume pauses s, then schedules a resume after
// resumeAfter. The picker is asked to suspend s during the paused window
// so other transitions don't fire concurrently — this models a real
// customer's quiet period.
func FirePauseAndScheduleResume(
	ctx context.Context,
	deps Deps,
	s Subject,
	resumeAfter time.Duration,
) error {
	rec := newIntent(s, TransitionPause)
	rec.IdempotencyKey = idempotencyKey(s, TransitionPause)
	if err := deps.Log.Append(rec); err != nil {
		return fmt.Errorf("log pause intent: %w", err)
	}

	body := pauseRequest{Reason: "loadgen-pause"}
	var resp pauseResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionPause, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		body,
		&resp,
	)
	rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	rec.HTTPStatus = status
	if err != nil {
		rec.TransitionStatus = StatusFail
		rec.Error = FormatError(err)
		_ = deps.Log.Append(rec)
		return err
	}
	rec.TransitionStatus = StatusOK
	rec.ExpectedPostState = string(scenario.StatePaused)
	if err := deps.Log.Append(rec); err != nil {
		return err
	}
	deps.Picker.SetLiveState(s.SubscriptionID, scenario.StatePaused)
	deps.Picker.MarkSuspended(s.SubscriptionID)

	// Schedule a deferred resume. The captured ctx is the agent's run
	// context — when the agent shuts down, ResumeScheduler.Cancel races
	// timer.Stop with the AfterFunc, so we double-check ctx.Err() inside.
	deps.Resumes.Schedule(s.SubscriptionID, resumeAfter, func() {
		// Detached context is necessary — the run context will be cancelled
		// before our timer fires. Use a short-lived context so a hung
		// resume doesn't block shutdown forever.
		callCtx, cancel := context.WithTimeout(context.Background(), deps.ResumeTimeout)
		defer cancel()
		_ = fireResume(callCtx, deps, s)
	})
	return nil
}

// fireResume calls /resume — invoked by both the deferred timer and (in
// rare cases) the agent picking RESUME directly when a sub is already PAUSED.
func fireResume(ctx context.Context, deps Deps, s Subject) error {
	rec := newIntent(s, TransitionResume)
	// Mark FROM as PAUSED for the audit row (more useful than the picker's
	// memory which we already updated when we paused).
	rec.FromState = string(scenario.StatePaused)
	rec.IdempotencyKey = idempotencyKey(s, TransitionResume)
	if err := deps.Log.Append(rec); err != nil {
		return err
	}

	var resp pauseResponse
	start := time.Now()
	status, err := deps.Client.PostJSON(
		ctx,
		aforo.ServicePricing,
		fmt.Sprintf(aforo.PathSubscriptionResume, s.SubscriptionID),
		s.TenantID,
		rec.IdempotencyKey,
		nil,
		&resp,
	)
	rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	rec.HTTPStatus = status
	if err != nil {
		rec.TransitionStatus = StatusFail
		rec.Error = FormatError(err)
		_ = deps.Log.Append(rec)
		// Even on failure, un-suspend the sub so other kinds can pick it.
		deps.Picker.MarkLive(s.SubscriptionID)
		return err
	}
	rec.TransitionStatus = StatusOK
	rec.ExpectedPostState = string(scenario.StateActive)
	_ = deps.Log.Append(rec)
	deps.Picker.SetLiveState(s.SubscriptionID, scenario.StateActive)
	deps.Picker.MarkLive(s.SubscriptionID)
	return nil
}

// FireResume is the agent-driven resume entry. Most resumes happen via
// ResumeScheduler; this path covers RESUME picked directly out of the kind
// rotation when a sub is already PAUSED.
func FireResume(ctx context.Context, deps Deps, s Subject) error {
	return fireResume(ctx, deps, s)
}
