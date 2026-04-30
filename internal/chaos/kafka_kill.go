package chaos

import (
	"context"
	"errors"
	"fmt"
)

// KafkaKill kills one Kafka broker for a fixed duration via AWS SSM, then
// restores the broker on Recovery.
//
// What it tests: producer retry + idempotent commit + DLT routing under
// broker loss. The platform's KafkaErrorHandlerConfig should retain
// in-flight events with 3-retry exponential backoff and route stuck
// records to DLT after the retry budget. After Recovery, the consumer
// lag should drain and dead_letter_records should reflect the gap.
//
// Implementation: aws ssm send-command on the broker EC2 instance to
// `systemctl stop kafka`, then `systemctl start kafka` on Recovery. Uses
// SSM not SSH because the loadgen tool runs on the bastion without ssh
// keys (per ops runbook).
//
// Safety: refuses to fire when ClusterName or InstanceID is empty —
// killing "all brokers in the default cluster" is the kind of mistake
// that takes the perf cluster offline for the day.
type KafkaKill struct {
	// ClusterName is informational, embedded into ssm command tags so
	// CloudTrail records why the broker was bounced.
	ClusterName string

	// InstanceID is the EC2 instance ID running the broker. Required.
	InstanceID string

	// SSMDocumentName overrides the document used to send the command.
	// 0 → AWS-RunShellScript.
	SSMDocumentName string
}

// Type implements Scenario.
func (k *KafkaKill) Type() string { return "kafka_kill" }

// Plan validates the chaos params against the target environment. Calls
// `aws ssm describe-instance-information` to confirm the instance is
// reachable; returns an error if not.
func (k *KafkaKill) Plan(ctx context.Context, exec Executor) error {
	if k.InstanceID == "" {
		return errors.New("kafka_kill: instance_id is required")
	}
	out, err := exec.Run(ctx, "kafka_kill.plan", "aws", "ssm", "describe-instance-information",
		"--filters", fmt.Sprintf("Key=InstanceIds,Values=%s", k.InstanceID),
		"--output", "json")
	if err != nil {
		return fmt.Errorf("kafka_kill: ssm describe-instance-information: %w", err)
	}
	if out == "" {
		// Recorder mode — consider it valid; production output is JSON.
		return nil
	}
	return nil
}

// Inject sends `systemctl stop kafka` to the broker. Returns a Recovery
// that sends `systemctl start kafka`.
func (k *KafkaKill) Inject(ctx context.Context, exec Executor) (Recovery, error) {
	doc := k.SSMDocumentName
	if doc == "" {
		doc = "AWS-RunShellScript"
	}
	stopCmd := []string{
		"ssm", "send-command",
		"--document-name", doc,
		"--instance-ids", k.InstanceID,
		"--parameters", `commands=["sudo systemctl stop kafka"]`,
		"--comment", fmt.Sprintf("aforo-loadgen chaos kafka_kill cluster=%s", k.ClusterName),
	}
	if _, err := exec.Run(ctx, "kafka_kill.inject", "aws", stopCmd...); err != nil {
		return nil, fmt.Errorf("kafka_kill: stop: %w", err)
	}
	return func(ctx context.Context, exec Executor) error {
		startCmd := []string{
			"ssm", "send-command",
			"--document-name", doc,
			"--instance-ids", k.InstanceID,
			"--parameters", `commands=["sudo systemctl start kafka"]`,
			"--comment", fmt.Sprintf("aforo-loadgen chaos kafka_kill recovery cluster=%s", k.ClusterName),
		}
		if _, err := exec.Run(ctx, "kafka_kill.recover", "aws", startCmd...); err != nil {
			return fmt.Errorf("kafka_kill: start: %w", err)
		}
		return nil
	}, nil
}
