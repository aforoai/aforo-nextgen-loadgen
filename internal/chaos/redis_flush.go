package chaos

import (
	"context"
	"errors"
	"fmt"
)

// RedisFlush flushes the Redis ElastiCache cluster mid-run to test
// cold-cache resilience.
//
// What it tests: the BillingHierarchyEnricher's ability to rebuild the
// API-key → tenant lookup cache from PostgreSQL after a flush; entitlement
// cache rebuild via EntitlementCacheSyncJob; Kong rate-limit policy
// re-warm via RateLimitPolicyCacheWarmer.
//
// What it does NOT test: the platform's safety against an attacker
// flushing prod Redis. That is handled by the security team, not by this
// fault-injector.
//
// Implementation: redis-cli FLUSHALL via SSM on the bastion. We do not
// connect to Redis directly from this binary because production Redis is
// in a private subnet. Recovery is a no-op — there is no "unflush"; the
// running services rebuild from primary stores.
//
// Safety: refuses to fire if the host pattern looks like prod (contains
// "prod" or "production"). Defense in depth alongside the scheduler's
// target-allowlist gate.
type RedisFlush struct {
	// BastionInstanceID is the EC2 instance with redis-cli installed and
	// network access to the cache cluster. Required.
	BastionInstanceID string

	// CacheEndpoint is the Redis primary endpoint (host:port). Required.
	CacheEndpoint string

	// SSMDocumentName overrides the SSM document. 0 → AWS-RunShellScript.
	SSMDocumentName string
}

// Type implements Scenario.
func (r *RedisFlush) Type() string { return "redis_flush" }

// Plan validates params and refuses to fire when CacheEndpoint contains
// "prod" — defensive belt-and-braces check.
func (r *RedisFlush) Plan(ctx context.Context, exec Executor) error {
	if r.BastionInstanceID == "" {
		return errors.New("redis_flush: bastion_instance_id is required")
	}
	if r.CacheEndpoint == "" {
		return errors.New("redis_flush: cache_endpoint is required")
	}
	if containsAny(r.CacheEndpoint, "prod", "production") {
		return fmt.Errorf("redis_flush: cache_endpoint %q looks like production — refusing to schedule", r.CacheEndpoint)
	}
	return nil
}

// Inject sends `redis-cli -h <endpoint> FLUSHALL` via SSM. Recovery is a
// no-op — services rebuild from PostgreSQL and Kong's policy table.
func (r *RedisFlush) Inject(ctx context.Context, exec Executor) (Recovery, error) {
	doc := r.SSMDocumentName
	if doc == "" {
		doc = "AWS-RunShellScript"
	}
	cmd := fmt.Sprintf(`commands=["redis-cli -h %s FLUSHALL"]`, r.CacheEndpoint)
	args := []string{
		"ssm", "send-command",
		"--document-name", doc,
		"--instance-ids", r.BastionInstanceID,
		"--parameters", cmd,
		"--comment", "aforo-loadgen chaos redis_flush",
	}
	if _, err := exec.Run(ctx, "redis_flush.inject", "aws", args...); err != nil {
		return nil, fmt.Errorf("redis_flush: %w", err)
	}
	// Recovery is a no-op; services rebuild from primary.
	return func(ctx context.Context, exec Executor) error { return nil }, nil
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if indexFold(s, n) >= 0 {
			return true
		}
	}
	return false
}

// indexFold is a case-insensitive strings.Index. Inlined to avoid pulling
// strings.ToLower (allocates) for one comparison.
func indexFold(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if foldChar(s[i+j]) != foldChar(sub[j]) {
				continue outer
			}
		}
		return i
	}
	return -1
}

func foldChar(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}
