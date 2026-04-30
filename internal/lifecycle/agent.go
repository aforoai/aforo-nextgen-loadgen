package lifecycle

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// AgentConfig configures the orchestrator. Must populate everything except
// the optional fields with sane defaults applied.
type AgentConfig struct {
	Scenario *scenario.Scenario
	Manifest *seed.Manifest
	Log      *TransitionLog
	Client   *Client
	Picker   *Picker
	RunID    string

	// MinTickerInterval guards against runaway tick rates when a transition
	// has tiny eligible-population × pct combos. Default: 1s.
	MinTickerInterval time.Duration

	// MaxTickerInterval caps idle tickers — when no eligible subs remain
	// for a kind, the ticker still polls at this rate to pick up newly-
	// eligible subs (e.g. PAST_DUE created mid-run). Default: 30s.
	MaxTickerInterval time.Duration

	// PauseResumeDelay is how long a paused sub stays paused before the
	// scheduled resume fires. Real customer pauses last hours/days; we
	// compress this so a 4h run produces both pause + resume signal.
	// Default: 30s.
	PauseResumeDelay time.Duration

	// ResumeTimeout is the per-resume detached-context budget. Default 30s.
	ResumeTimeout time.Duration

	// DunningConfig overrides the platform's max-attempts. Default = platform default.
	DunningConfig DunningConfig

	// Logger receives one-line agent events (start, stop, ticker config).
	// nil → discard.
	Logger io.Writer

	// Now is for tests. nil → time.Now.
	Now func() time.Time
}

// Agent is the orchestrator. One Agent per `lifecycle` invocation; runs
// alongside the run engine. Run blocks until ctx is cancelled, then drains
// pending resumes and returns.
type Agent struct {
	cfg     AgentConfig
	deps    Deps
	resumes *ResumeScheduler
	dunning *DunningWalker
	tickers []*kindTicker
	now     func() time.Time
	log     io.Writer
}

// NewAgent constructs and validates an Agent.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.Scenario == nil {
		return nil, fmt.Errorf("lifecycle: agent: scenario is required")
	}
	if cfg.Manifest == nil {
		return nil, fmt.Errorf("lifecycle: agent: manifest is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("lifecycle: agent: transition log is required")
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("lifecycle: agent: client is required")
	}
	if cfg.Picker == nil {
		return nil, fmt.Errorf("lifecycle: agent: picker is required")
	}
	if cfg.MinTickerInterval <= 0 {
		cfg.MinTickerInterval = 1 * time.Second
	}
	if cfg.MaxTickerInterval <= 0 {
		cfg.MaxTickerInterval = 30 * time.Second
	}
	if cfg.PauseResumeDelay <= 0 {
		cfg.PauseResumeDelay = 30 * time.Second
	}
	if cfg.ResumeTimeout <= 0 {
		cfg.ResumeTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = io.Discard
	}

	resumes := NewResumeScheduler()
	dunning := NewDunningWalker(cfg.DunningConfig)
	deps := Deps{
		Client:        cfg.Client,
		Log:           cfg.Log,
		Picker:        cfg.Picker,
		Resumes:       resumes,
		Dunning:       dunning,
		RunID:         cfg.RunID,
		ResumeTimeout: cfg.ResumeTimeout,
	}
	return &Agent{
		cfg:     cfg,
		deps:    deps,
		resumes: resumes,
		dunning: dunning,
		now:     cfg.Now,
		log:     cfg.Logger,
	}, nil
}

// Run blocks until ctx is cancelled. Spawns one goroutine per enabled
// transition kind; each ticker fires at a per-kind rate derived from
// scenario.lifecycle.*_per_hour_pct × eligible-sub-count.
func (a *Agent) Run(ctx context.Context) error {
	lc := a.cfg.Scenario.Lifecycle
	if !lc.Enabled {
		fmt.Fprintln(a.log, "lifecycle: disabled by scenario — agent will idle and exit on ctx done")
		<-ctx.Done()
		return nil
	}

	a.tickers = a.buildTickers(lc)
	if len(a.tickers) == 0 {
		fmt.Fprintln(a.log, "lifecycle: no transitions enabled — agent will idle")
		<-ctx.Done()
		return nil
	}

	fmt.Fprintf(a.log, "lifecycle: starting %d kind tickers\n", len(a.tickers))

	wg := sync.WaitGroup{}
	for _, t := range a.tickers {
		wg.Add(1)
		t := t
		go func() {
			defer wg.Done()
			t.run(ctx, a)
		}()
	}

	<-ctx.Done()
	fmt.Fprintf(a.log, "lifecycle: ctx done; cancelling %d pending resumes\n", a.resumes.PendingCount())
	a.resumes.Cancel()
	wg.Wait()
	fmt.Fprintln(a.log, "lifecycle: all kind tickers exited")
	return nil
}

// kindTicker drives one TransitionKind's firing schedule.
type kindTicker struct {
	kind      TransitionKind
	hourlyPct float64
	fire      func(context.Context, Deps, Subject) error
	postFire  func(*kindTicker, error)
}

func (t *kindTicker) run(ctx context.Context, a *Agent) {
	for {
		// Compute the next fire interval based on the current eligible count.
		// Empty eligibility → wait MaxTickerInterval (bucket may refill).
		eligible := a.deps.Picker.EligibleCount(t.kind)
		var interval time.Duration
		if eligible == 0 || t.hourlyPct <= 0 {
			interval = a.cfg.MaxTickerInterval
		} else {
			perHour := t.hourlyPct * float64(eligible)
			if perHour <= 0 {
				interval = a.cfg.MaxTickerInterval
			} else {
				secondsPerEvent := 3600.0 / perHour
				interval = time.Duration(secondsPerEvent * float64(time.Second))
				if interval < a.cfg.MinTickerInterval {
					interval = a.cfg.MinTickerInterval
				}
				if interval > a.cfg.MaxTickerInterval {
					interval = a.cfg.MaxTickerInterval
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		s, ok := a.deps.Picker.PickFor(t.kind)
		if !ok {
			continue
		}
		err := t.fire(ctx, a.deps, s)
		if t.postFire != nil {
			t.postFire(t, err)
		}
	}
}

// buildTickers translates scenario.LifecycleProfile fields into per-kind
// tickers. Each ticker captures its fire function so the run loop is data-
// driven — adding a new transition is "add a kindTicker entry".
func (a *Agent) buildTickers(lc scenario.LifecycleProfile) []*kindTicker {
	tickers := []*kindTicker{}
	add := func(k TransitionKind, pct float64, fire func(context.Context, Deps, Subject) error) {
		if pct <= 0 || math.IsNaN(pct) {
			return
		}
		tickers = append(tickers, &kindTicker{kind: k, hourlyPct: pct, fire: fire})
	}

	add(TransitionUpgrade, lc.UpgradesPerHourPct, FireUpgrade)
	add(TransitionDowngrade, lc.DowngradesPerHourPct, FireDowngrade)
	// Pause+Resume share a single ticker — the pause step schedules its
	// own resume after PauseResumeDelay.
	add(TransitionPause, lc.PauseResumePerHourPct, func(ctx context.Context, d Deps, s Subject) error {
		return FirePauseAndScheduleResume(ctx, d, s, a.cfg.PauseResumeDelay)
	})
	add(TransitionTrialConversion, lc.TrialConversionPerHourPct, FireTrialConversion)
	add(TransitionTrialCancel, lc.TrialCancelPerHourPct, FireTrialCancel)
	add(TransitionMigrate, lc.MigratePerHourPct, FireMigrate)
	// Retry-payment ticker drives the dunning walker — every retry-payment
	// pick goes through the walker so escalation is tracked per sub.
	add(TransitionRetryPayment, lc.RetryPaymentPerHourPct, func(ctx context.Context, d Deps, s Subject) error {
		return a.dunning.Step(ctx, d, s)
	})
	return tickers
}

// LogSnapshot returns the transition-log roll-up for the agent's run.
// Used by the CLI to print a shutdown summary.
func (a *Agent) LogSnapshot() Snapshot {
	if a.cfg.Log == nil {
		return Snapshot{}
	}
	return a.cfg.Log.Snapshot()
}

// PendingResumeCount surfaces the resume scheduler's depth — useful for
// integration tests that want to assert the scheduler drained on shutdown.
func (a *Agent) PendingResumeCount() int { return a.resumes.PendingCount() }
