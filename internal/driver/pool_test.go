package driver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// fakeDriver records calls and returns canned results.
type fakeDriver struct {
	name     string
	mu       sync.Mutex
	calls    int
	resultFn func(*generator.Event) Result
}

func (d *fakeDriver) Name() string { return d.name }
func (d *fakeDriver) Submit(_ context.Context, e *generator.Event) Result {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.resultFn == nil {
		return Result{Event: e, Status: 200}
	}
	return d.resultFn(e)
}
func (d *fakeDriver) Close() error { return nil }
func (d *fakeDriver) Calls() int   { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

// TestPoolDispatchesAllEvents — feed N events, expect N submits, all
// successful.
func TestPoolDispatchesAllEvents(t *testing.T) {
	drv := &fakeDriver{name: "fake"}
	pool, err := NewPool(PoolConfig{Driver: drv, Workers: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ch := make(chan *generator.Event, 100)
	for i := 0; i < 100; i++ {
		ch <- &generator.Event{
			Envelope: generator.Envelope{
				ProductType: "API",
				},
		}
	}
	close(ch)
	pool.Run(context.Background(), ch)
	if got := drv.Calls(); got != 100 {
		t.Errorf("dispatched %d events, want 100", got)
	}
	if got := pool.Stats().Succeeded.Load(); got != 100 {
		t.Errorf("succeeded=%d, want 100", got)
	}
}

// TestPoolSkipsWhenCircuitOpen — open breaker → no driver calls.
func TestPoolSkipsWhenCircuitOpen(t *testing.T) {
	drv := &fakeDriver{name: "fake"}
	cb := NewCircuitBreaker(CircuitBreakerConfig{Now: time.Now})
	cb.ForceOpen()
	pool, err := NewPool(PoolConfig{Driver: drv, Workers: 2, CircuitBreaker: cb})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ch := make(chan *generator.Event, 50)
	for i := 0; i < 50; i++ {
		ch <- &generator.Event{Envelope: generator.Envelope{}}
	}
	close(ch)
	pool.Run(context.Background(), ch)
	if drv.Calls() > 0 {
		t.Errorf("driver should not be called while circuit open; got %d", drv.Calls())
	}
	if got := pool.Stats().CircuitOpenSkipped.Load(); got != 50 {
		t.Errorf("CircuitOpenSkipped=%d, want 50", got)
	}
}

// TestPoolExpectedFailuresDoNotTripBreaker — negative-path-induced 4xx
// counted as expected, NOT toward breaker error rate.
func TestPoolExpectedFailuresDoNotTripBreaker(t *testing.T) {
	drv := &fakeDriver{name: "fake"}
	drv.resultFn = func(e *generator.Event) Result {
		// future_event → 4xx response, expected
		return Result{Event: e, Status: 400}
	}
	now := time.Unix(1000, 0)
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         5,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		Now:                func() time.Time { return now },
	})
	pool, err := NewPool(PoolConfig{Driver: drv, Workers: 1, CircuitBreaker: cb})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ch := make(chan *generator.Event, 20)
	for i := 0; i < 20; i++ {
		ch <- &generator.Event{
			Envelope:     generator.Envelope{},
			NegativePath: generator.NPFuture,
		}
	}
	close(ch)
	pool.Run(context.Background(), ch)
	if cb.State() != StateClosed {
		t.Errorf("breaker should stay closed; expected_failures shouldn't trip; got %s", cb.State())
	}
	if got := pool.Stats().ExpectedFailures.Load(); got != 20 {
		t.Errorf("ExpectedFailures=%d, want 20", got)
	}
}

// TestPoolBackpressureTrips — repeated 5xx engages backpressure.
func TestPoolBackpressureTrips(t *testing.T) {
	drv := &fakeDriver{name: "fake"}
	drv.resultFn = func(e *generator.Event) Result {
		return Result{Event: e, Status: 503}
	}
	now := time.Unix(1000, 0)
	bp := NewBackpressure(BackpressureConfig{
		MinSamples:         10,
		ErrorRateThreshold: 0.05,
		Window:             time.Minute,
		Now:                func() time.Time { return now },
	})
	pool, err := NewPool(PoolConfig{Driver: drv, Workers: 1, Backpressure: bp})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ch := make(chan *generator.Event, 50)
	for i := 0; i < 50; i++ {
		ch <- &generator.Event{Envelope: generator.Envelope{}}
	}
	close(ch)
	pool.Run(context.Background(), ch)
	if !bp.Active() {
		t.Errorf("backpressure should engage on all-503 traffic")
	}
	if got := pool.Stats().ServerErrors.Load(); got != 50 {
		t.Errorf("ServerErrors=%d, want 50", got)
	}
}

// TestPoolOnResultCallback — every Submit yields one OnResult.
func TestPoolOnResultCallback(t *testing.T) {
	var called atomic.Int64
	drv := &fakeDriver{name: "fake"}
	pool, err := NewPool(PoolConfig{
		Driver:   drv,
		Workers:  4,
		OnResult: func(_ Result) { called.Add(1) },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ch := make(chan *generator.Event, 100)
	for i := 0; i < 100; i++ {
		ch <- &generator.Event{Envelope: generator.Envelope{}}
	}
	close(ch)
	pool.Run(context.Background(), ch)
	if got := called.Load(); got != 100 {
		t.Errorf("OnResult called %d times, want 100", got)
	}
}
