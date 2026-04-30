package chaos

import (
	"context"
	"errors"
	"fmt"
)

// NetPartition installs an iptables DROP rule between two service hosts
// for a fixed duration, then removes it on Recovery.
//
// What it tests: cross-service circuit breakers (Resilience4j), retry
// logic in RestTemplate clients, and the platform's degraded-mode
// behavior when one upstream is unreachable. Specifically, when
// billing-platform cannot reach customer-service, billing operations
// that need fresh customer data should fail closed with a 503 rather
// than serve stale data or 500.
//
// Implementation: on the SOURCE host, `iptables -A OUTPUT -d <dst-ip>
// -j DROP`; on Recovery, `iptables -D OUTPUT -d <dst-ip> -j DROP`. We
// install on the source side rather than the destination so the dst
// host's logs show no traffic at all (clean failure mode); installing
// on the destination would let the source still see ACKed connections
// drop after the rule.
//
// We tag the iptables rule with a custom "-m comment" so multiple
// concurrent net_partition events don't step on each other's cleanup.
type NetPartition struct {
	// SourceInstanceID is the EC2 instance to install the iptables rule
	// on. Required.
	SourceInstanceID string

	// DestIP is the IP address to drop traffic to. Required.
	DestIP string

	// SourceServiceName is informational — appended to the iptables rule
	// comment so post-mortems can find which chaos event installed which
	// rule.
	SourceServiceName string

	// DestServiceName is informational, same purpose.
	DestServiceName string

	// SSMDocumentName overrides the SSM document. 0 → AWS-RunShellScript.
	SSMDocumentName string
}

// Type implements Scenario.
func (n *NetPartition) Type() string { return "net_partition" }

// Plan validates required fields.
func (n *NetPartition) Plan(ctx context.Context, exec Executor) error {
	if n.SourceInstanceID == "" {
		return errors.New("net_partition: source_instance_id is required")
	}
	if n.DestIP == "" {
		return errors.New("net_partition: dest_ip is required")
	}
	if _, err := exec.Run(ctx, "net_partition.plan", "aws", "ssm", "describe-instance-information",
		"--filters", fmt.Sprintf("Key=InstanceIds,Values=%s", n.SourceInstanceID),
		"--output", "json"); err != nil {
		return fmt.Errorf("net_partition: ssm reachability: %w", err)
	}
	return nil
}

// Inject installs the iptables DROP rule; Recovery removes it.
func (n *NetPartition) Inject(ctx context.Context, exec Executor) (Recovery, error) {
	doc := n.SSMDocumentName
	if doc == "" {
		doc = "AWS-RunShellScript"
	}
	tag := fmt.Sprintf("aforo-loadgen-chaos-%s-to-%s", n.SourceServiceName, n.DestServiceName)
	addCmd := fmt.Sprintf(`commands=["sudo iptables -I OUTPUT -d %s -m comment --comment '%s' -j DROP"]`, n.DestIP, tag)
	if _, err := exec.Run(ctx, "net_partition.inject", "aws",
		"ssm", "send-command",
		"--document-name", doc,
		"--instance-ids", n.SourceInstanceID,
		"--parameters", addCmd,
		"--comment", fmt.Sprintf("aforo-loadgen chaos net_partition %s→%s", n.SourceServiceName, n.DestServiceName),
	); err != nil {
		return nil, fmt.Errorf("net_partition: iptables add: %w", err)
	}
	return func(ctx context.Context, exec Executor) error {
		// Match the rule by tag for safe cleanup. Use `iptables -D` with
		// the same args we used to add. Fallback to listing rules and
		// deleting any matching the tag in case the args drifted.
		delCmd := fmt.Sprintf(
			`commands=["sudo iptables -D OUTPUT -d %s -m comment --comment '%s' -j DROP || (sudo iptables -L OUTPUT --line-numbers -n | grep '%s' | awk '{print $1}' | tac | while read n; do sudo iptables -D OUTPUT $n; done)"]`,
			n.DestIP, tag, tag,
		)
		if _, err := exec.Run(ctx, "net_partition.recover", "aws",
			"ssm", "send-command",
			"--document-name", doc,
			"--instance-ids", n.SourceInstanceID,
			"--parameters", delCmd,
			"--comment", fmt.Sprintf("aforo-loadgen chaos net_partition recovery %s→%s", n.SourceServiceName, n.DestServiceName),
		); err != nil {
			return fmt.Errorf("net_partition: iptables del: %w", err)
		}
		return nil
	}, nil
}
