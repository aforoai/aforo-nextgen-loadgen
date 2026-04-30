package chaos

import (
	"context"
	"errors"
	"fmt"
)

// CHSlowdown adds artificial network latency to the ClickHouse instance
// for a fixed duration via Linux tc/netem, then removes it on Recovery.
//
// What it tests: analytics-service writer back-pressure, the
// Resilience4j circuit breaker around ClickHouse writes, and the
// AnalyticsQueryRouter's PostgreSQL fallback path. With +500ms added at
// the network level, ClickHouseWriter buffer fills, circuit breaker
// trips, reads fall back to PostgreSQL until the breaker half-opens.
//
// Implementation: ssm send-command runs `tc qdisc add dev eth0 root
// netem delay 500ms` on Recovery, `tc qdisc del dev eth0 root` clears
// it. Both are idempotent in netem 4.x — re-adding replaces the
// existing qdisc.
//
// Limitation: this targets ONE ClickHouse instance. Multi-replica setups
// need one event per node; the scenario YAML can declare multiple
// ch_slowdown events with different instance_id values.
type CHSlowdown struct {
	// InstanceID is the EC2 instance hosting ClickHouse. Required.
	InstanceID string

	// LatencyMs is the additional one-way latency in milliseconds.
	// Default 500.
	LatencyMs int

	// Iface is the network interface to apply the qdisc to. Default
	// "eth0". Some instance types use "ens5".
	Iface string

	// SSMDocumentName overrides the SSM document. 0 → AWS-RunShellScript.
	SSMDocumentName string
}

// Type implements Scenario.
func (c *CHSlowdown) Type() string { return "ch_slowdown" }

// Plan checks instance reachability via SSM.
func (c *CHSlowdown) Plan(ctx context.Context, exec Executor) error {
	if c.InstanceID == "" {
		return errors.New("ch_slowdown: instance_id is required")
	}
	if c.LatencyMs < 0 {
		return errors.New("ch_slowdown: latency_ms must be >= 0")
	}
	if _, err := exec.Run(ctx, "ch_slowdown.plan", "aws", "ssm", "describe-instance-information",
		"--filters", fmt.Sprintf("Key=InstanceIds,Values=%s", c.InstanceID),
		"--output", "json"); err != nil {
		return fmt.Errorf("ch_slowdown: ssm reachability: %w", err)
	}
	return nil
}

// Inject adds the netem qdisc; Recovery removes it.
func (c *CHSlowdown) Inject(ctx context.Context, exec Executor) (Recovery, error) {
	doc := c.SSMDocumentName
	if doc == "" {
		doc = "AWS-RunShellScript"
	}
	iface := c.Iface
	if iface == "" {
		iface = "eth0"
	}
	latency := c.LatencyMs
	if latency <= 0 {
		latency = 500
	}
	addCmd := fmt.Sprintf(`commands=["sudo tc qdisc replace dev %s root netem delay %dms"]`, iface, latency)
	if _, err := exec.Run(ctx, "ch_slowdown.inject", "aws",
		"ssm", "send-command",
		"--document-name", doc,
		"--instance-ids", c.InstanceID,
		"--parameters", addCmd,
		"--comment", "aforo-loadgen chaos ch_slowdown",
	); err != nil {
		return nil, fmt.Errorf("ch_slowdown: tc add: %w", err)
	}
	return func(ctx context.Context, exec Executor) error {
		delCmd := fmt.Sprintf(`commands=["sudo tc qdisc del dev %s root || true"]`, iface)
		if _, err := exec.Run(ctx, "ch_slowdown.recover", "aws",
			"ssm", "send-command",
			"--document-name", doc,
			"--instance-ids", c.InstanceID,
			"--parameters", delCmd,
			"--comment", "aforo-loadgen chaos ch_slowdown recovery",
		); err != nil {
			return fmt.Errorf("ch_slowdown: tc del: %w", err)
		}
		return nil
	}, nil
}
