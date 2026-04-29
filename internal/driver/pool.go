package driver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// PoolConfig configures the worker pool that consumes events from a channel
// and dispatches them through a Driver.
type PoolConfig struct {
	Driver         Driver
	Workers        int
	Backpressure   *BackpressureController
	CircuitBreaker *CircuitBreaker
	// OnResult is invoked once per Submit. The runner uses it to record
	// HDR latencies, per-tenant counts, and metric counters. Called from
	// worker goroutines — implementations must be concurrency-safe.
	OnResult func(Result)
	// AcceptOnly is an optional filter — if non-nil, the pool will skip
	// events for which AcceptOnly returns false. Used by tests; normal
	// runs leave it nil.
	AcceptOnly func(*generator.Event) bool
	// MaxQueueDepth is the buffered depth between the events channel and
	// the worker fanout. Default 1024.
	MaxQueueDepth int
}

// PoolStats reports counters for the metrics layer.
type PoolStats struct {
	Submitted          atomic.Int64
	Succeeded          atomic.Int64
	ClientErrors       atomic.Int64
	ServerErrors       atomic.Int64
	TransportFailures  atomic.Int64
	CircuitOpenSkipped atomic.Int64
	ExpectedFailures   atomic.Int64
}

// Pool runs N worker goroutines consuming events from the input channel.
// One Pool services exactly one Driver. The runner builds one pool per
// distinct ingestion path it routes traffic to (Session 4: just rest_direct).
type Pool struct {
	cfg       PoolConfig
	stats     *PoolStats
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewPool constructs but does not start the pool. Workers spin up on Run.
func NewPool(cfg PoolConfig) (*Pool, error) {
	if cfg.Driver == nil {
		return nil, errors.New("pool: Driver is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	if cfg.MaxQueueDepth <= 0 {
		cfg.MaxQueueDepth = 1024
	}
	return &Pool{cfg: cfg, stats: &PoolStats{}}, nil
}

// Stats returns the pool's counters.
func (p *Pool) Stats() *PoolStats { return p.stats }

// Run dispatches events from the input channel until it's closed AND all
// workers have drained. Returns when both conditions hold.
func (p *Pool) Run(ctx context.Context, events <-chan *generator.Event) {
	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx, events)
	}
	p.wg.Wait()
}

func (p *Pool) worker(ctx context.Context, events <-chan *generator.Event) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain semantics: when ctx cancels, we still try to clear what's
			// already in the channel so partial output is consistent.
			p.drainOnExit(events)
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e == nil {
				continue
			}
			if p.cfg.AcceptOnly != nil && !p.cfg.AcceptOnly(e) {
				continue
			}
			p.dispatch(ctx, e)
		}
	}
}

// drainOnExit clears any events buffered in the channel without sending —
// used when ctx cancels mid-run. Each drained event is reported as a
// "circuit_open_skipped" so the runner sees them in stats.
func (p *Pool) drainOnExit(events <-chan *generator.Event) {
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			if e != nil {
				p.stats.CircuitOpenSkipped.Add(1)
				if p.cfg.OnResult != nil {
					p.cfg.OnResult(Result{
						Event:        e,
						TransportErr: ErrPaused,
					})
				}
			}
		default:
			return
		}
	}
}

// dispatch runs the breaker check, calls Driver.Submit, classifies the
// result, and reports it to the runner via OnResult.
func (p *Pool) dispatch(ctx context.Context, e *generator.Event) {
	if p.cfg.CircuitBreaker != nil && !p.cfg.CircuitBreaker.Allow() {
		p.stats.CircuitOpenSkipped.Add(1)
		if p.cfg.OnResult != nil {
			p.cfg.OnResult(Result{
				Event:        e,
				TransportErr: ErrCircuitOpen,
			})
		}
		return
	}

	res := p.cfg.Driver.Submit(ctx, e)
	p.stats.Submitted.Add(1)

	// Classify outcome for metrics + breakers.
	switch {
	case res.IsSuccess():
		p.stats.Succeeded.Add(1)
	case res.IsClientError():
		p.stats.ClientErrors.Add(1)
	case res.IsServerError():
		p.stats.ServerErrors.Add(1)
	case res.IsTransport():
		p.stats.TransportFailures.Add(1)
	}

	expected := res.IsExpectedFailure()
	if expected {
		p.stats.ExpectedFailures.Add(1)
	}

	// Backpressure + circuit breaker decisions: count an event as a "real"
	// failure only if it's not an expected negative-path outcome. Otherwise
	// a scenario with high oversize_pct would needlessly trip the breaker.
	switch {
	case res.IsSuccess():
		if p.cfg.Backpressure != nil {
			p.cfg.Backpressure.Record(true)
		}
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Record(true)
		}
	case expected:
		// Treat as success for health signals — the platform did the
		// right thing.
		if p.cfg.Backpressure != nil {
			p.cfg.Backpressure.Record(true)
		}
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Record(true)
		}
	default:
		if p.cfg.Backpressure != nil {
			p.cfg.Backpressure.Record(false)
		}
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Record(false)
		}
	}

	if p.cfg.OnResult != nil {
		p.cfg.OnResult(res)
	}
}

// Close releases driver resources. Safe to call multiple times.
func (p *Pool) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.cfg.Driver != nil {
			err = p.cfg.Driver.Close()
		}
	})
	return err
}

// SleepWithContext is a small helper used by tests + the runner to wait a
// duration with context cancellation. Defined here so the test/integration
// layer doesn't import time.NewTimer everywhere.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
