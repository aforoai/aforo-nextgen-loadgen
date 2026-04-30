package chaos

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a minimal mock clock for the scheduler's Now func.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeScenario is a Scenario that records its lifecycle for assertions.
type fakeScenario struct {
	name        string
	planErr     error
	injectErr   error
	recoverErr  error
	injectCount int
	recovered   bool
	mu          sync.Mutex
}

func (f *fakeScenario) Type() string { return f.name }
func (f *fakeScenario) Plan(ctx context.Context, exec Executor) error {
	return f.planErr
}
func (f *fakeScenario) Inject(ctx context.Context, exec Executor) (Recovery, error) {
	f.mu.Lock()
	f.injectCount++
	f.mu.Unlock()
	if f.injectErr != nil {
		return nil, f.injectErr
	}
	return func(ctx context.Context, exec Executor) error {
		f.mu.Lock()
		f.recovered = true
		f.mu.Unlock()
		return f.recoverErr
	}, nil
}

func (f *fakeScenario) injects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injectCount
}

func (f *fakeScenario) wasRecovered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recovered
}

func TestSchedulerRefusesNonAllowedTarget(t *testing.T) {
	_, err := NewScheduler(SchedulerConfig{
		Executor:   NewRecorder(),
		TargetName: "prod",
	})
	if err == nil {
		t.Fatal("scheduler must refuse to construct on non-allowed target")
	}
	if !strings.Contains(err.Error(), "not in allow list") {
		t.Fatalf("error must explain allow list; got %q", err)
	}
}

func TestSchedulerAcceptsCustomAllowedTarget(t *testing.T) {
	_, err := NewScheduler(SchedulerConfig{
		Executor:       NewRecorder(),
		TargetName:     "perf-custom",
		AllowedTargets: []string{"perf-custom"},
	})
	if err != nil {
		t.Fatalf("custom target should be accepted: %v", err)
	}
}

func TestSchedulerFiresAtPlannedOffset(t *testing.T) {
	clk := &fakeClock{}
	clk.Set(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	rec := NewRecorder()
	sc := &fakeScenario{name: "fakeA"}
	s, err := NewScheduler(SchedulerConfig{
		Events: []Event{
			{At: 1 * time.Hour, Duration: 30 * time.Second, Scenario: sc},
		},
		Executor:   rec,
		Now:        clk.Now,
		TargetName: "perf-aws",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.Start(clk.Now())
	// Tick before the offset → no fire.
	s.Tick(ctx)
	if sc.injects() != 0 {
		t.Fatalf("scenario fired before its offset; injects=%d", sc.injects())
	}
	// Advance to exactly the offset → fire.
	clk.Advance(1 * time.Hour)
	s.Tick(ctx)
	if sc.injects() != 1 {
		t.Fatalf("scenario should have fired at offset; injects=%d", sc.injects())
	}
	// Tick before duration elapses → no recovery.
	clk.Advance(15 * time.Second)
	s.Tick(ctx)
	if sc.wasRecovered() {
		t.Fatal("recovered before duration elapsed")
	}
	// Tick past the duration deadline → recovery.
	clk.Advance(20 * time.Second)
	s.Tick(ctx)
	if !sc.wasRecovered() {
		t.Fatal("recovery did not fire after duration elapsed")
	}
	// Idempotent on re-tick.
	clk.Advance(30 * time.Second)
	s.Tick(ctx)
	if sc.injects() != 1 {
		t.Fatalf("re-tick re-injected; injects=%d", sc.injects())
	}
}

func TestSchedulerJitterTolerance(t *testing.T) {
	clk := &fakeClock{}
	clk.Set(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	rec := NewRecorder()
	sc := &fakeScenario{name: "fakeJitter"}
	s, _ := NewScheduler(SchedulerConfig{
		Events:          []Event{{At: 1 * time.Second, Duration: 1 * time.Second, Scenario: sc}},
		Executor:        rec,
		Now:             clk.Now,
		TargetName:      "perf-aws",
		JitterTolerance: 200 * time.Millisecond,
	})
	s.Start(clk.Now())

	// 800ms — outside jitter window (need ≥ 800ms because tolerance shifts the
	// fire window earlier). 800ms + 200ms tolerance = 1000ms = exactly at,
	// which fires.
	clk.Advance(800 * time.Millisecond)
	s.Tick(context.Background())
	if sc.injects() != 1 {
		t.Fatalf("should fire within jitter window; injects=%d", sc.injects())
	}
}

func TestSchedulerCloseRecoversActiveFaults(t *testing.T) {
	clk := &fakeClock{}
	clk.Set(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	sc := &fakeScenario{name: "fakeLong"}
	s, _ := NewScheduler(SchedulerConfig{
		Events: []Event{
			{At: 1 * time.Second, Duration: 1 * time.Hour, Scenario: sc},
		},
		Executor:   NewRecorder(),
		Now:        clk.Now,
		TargetName: "perf-aws",
	})
	s.Start(clk.Now())
	clk.Advance(2 * time.Second) // past offset, but well within duration
	s.Tick(context.Background())
	if !sc.wasRecovered() == false {
		// flipped to "wasRecovered must be false"
	}
	if sc.wasRecovered() {
		t.Fatal("recovered prematurely")
	}
	// Run is closing — Close must recover even though the duration has not
	// elapsed.
	s.Close(context.Background())
	if !sc.wasRecovered() {
		t.Fatal("Close did not invoke recovery for active fault")
	}
	// Close is idempotent.
	s.Close(context.Background())
}

func TestSchedulerInjectErrorIsRecorded(t *testing.T) {
	clk := &fakeClock{}
	clk.Set(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	sc := &fakeScenario{name: "fakeBoom", injectErr: errors.New("boom")}
	s, _ := NewScheduler(SchedulerConfig{
		Events:     []Event{{At: 1 * time.Second, Duration: 30 * time.Second, Scenario: sc}},
		Executor:   NewRecorder(),
		Now:        clk.Now,
		TargetName: "perf-aws",
	})
	s.Start(clk.Now())
	clk.Advance(2 * time.Second)
	s.Tick(context.Background())
	outs := s.Outcomes()
	if len(outs) != 1 {
		t.Fatalf("expected one outcome, got %d", len(outs))
	}
	if outs[0].InjectError == "" {
		t.Fatal("inject error must be recorded")
	}
	// Recovery must NOT fire when inject failed (no recovery returned).
	if sc.wasRecovered() {
		t.Fatal("recovery fired despite inject failure")
	}
}

func TestSchedulerZeroDurationFiresAndRecoversInOneTick(t *testing.T) {
	clk := &fakeClock{}
	clk.Set(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	sc := &fakeScenario{name: "fakeFlush"}
	s, _ := NewScheduler(SchedulerConfig{
		Events:     []Event{{At: 1 * time.Second, Duration: 0, Scenario: sc}},
		Executor:   NewRecorder(),
		Now:        clk.Now,
		TargetName: "perf-aws",
	})
	s.Start(clk.Now())
	clk.Advance(2 * time.Second)
	s.Tick(context.Background())
	if !sc.wasRecovered() {
		t.Fatal("zero-duration event should fire-and-recover in same tick")
	}
}

func TestSchedulerPlanErrorPropagates(t *testing.T) {
	sc := &fakeScenario{name: "fakeBadPlan", planErr: errors.New("invalid")}
	s, _ := NewScheduler(SchedulerConfig{
		Events:     []Event{{At: 1 * time.Second, Duration: 30 * time.Second, Scenario: sc}},
		Executor:   NewRecorder(),
		TargetName: "perf-aws",
	})
	if err := s.Plan(context.Background()); err == nil {
		t.Fatal("plan error should propagate")
	}
}

func TestBuildScenarioRoundTrip(t *testing.T) {
	cases := []struct {
		typ    string
		params map[string]any
	}{
		{"kafka_kill", map[string]any{"instance_id": "i-aaa", "cluster_name": "msk-1"}},
		{"redis_flush", map[string]any{"bastion_instance_id": "i-bbb", "cache_endpoint": "perf-redis.amazonaws.com:6379"}},
		{"ch_slowdown", map[string]any{"instance_id": "i-ccc", "latency_ms": 500}},
		{"net_partition", map[string]any{"source_instance_id": "i-ddd", "dest_ip": "10.0.0.1"}},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			s, err := BuildScenario(c.typ, c.params)
			if err != nil {
				t.Fatal(err)
			}
			if s.Type() != c.typ {
				t.Fatalf("type %q ≠ %q", s.Type(), c.typ)
			}
		})
	}
}

func TestBuildScenarioUnknownType(t *testing.T) {
	_, err := BuildScenario("not_a_real_type", nil)
	if err == nil {
		t.Fatal("unknown type must return error")
	}
}

func TestRedisFlushRefusesProductionEndpoint(t *testing.T) {
	r := &RedisFlush{
		BastionInstanceID: "i-bastion",
		CacheEndpoint:     "redis-prod-cluster.cache.amazonaws.com:6379",
	}
	if err := r.Plan(context.Background(), NewRecorder()); err == nil {
		t.Fatal("redis_flush must refuse production-looking endpoint")
	}
}

func TestNetPartitionRecoveryUsesIptablesD(t *testing.T) {
	rec := NewRecorder()
	n := &NetPartition{
		SourceInstanceID: "i-source",
		DestIP:           "10.0.0.1",
	}
	rec.Returns("net_partition.inject", "ok", nil)
	rec.Returns("net_partition.recover", "ok", nil)
	recovery, err := n.Inject(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	calls := rec.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (inject + recover), got %d", len(calls))
	}
	// Recovery must include "iptables -D" — we verify the constructed
	// SSM parameters string includes the right cleanup command.
	recoverArgs := calls[1].Args
	found := false
	for _, a := range recoverArgs {
		if strings.Contains(a, "iptables -D OUTPUT") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("recovery args do not include iptables -D: %v", recoverArgs)
	}
}

func TestKafkaKillEmptyInstancePlanFails(t *testing.T) {
	k := &KafkaKill{}
	if err := k.Plan(context.Background(), NewRecorder()); err == nil {
		t.Fatal("empty instance_id must fail Plan")
	}
}

func TestCHSlowdownRequiresInstanceID(t *testing.T) {
	c := &CHSlowdown{LatencyMs: 500}
	if err := c.Plan(context.Background(), NewRecorder()); err == nil {
		t.Fatal("empty instance_id must fail Plan")
	}
}
