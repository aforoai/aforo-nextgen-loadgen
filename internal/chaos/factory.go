package chaos

import (
	"fmt"
)

// BuildScenario constructs a chaos.Scenario from the YAML "type" string
// and the free-form params map. Unknown types return an error so a typo
// in the scenario YAML fails at scheduler construction rather than at
// the moment the event would have fired.
//
// The supported types map 1:1 with the .go files in this package:
//
//	"kafka_kill"    → KafkaKill
//	"redis_flush"   → RedisFlush
//	"ch_slowdown"   → CHSlowdown
//	"net_partition" → NetPartition
//
// New chaos types: add a case here AND a constructor that reads from
// params. Keep the param keys in snake_case to match YAML conventions.
func BuildScenario(typ string, params map[string]any) (Scenario, error) {
	switch typ {
	case "kafka_kill":
		return &KafkaKill{
			ClusterName:     stringParam(params, "cluster_name"),
			InstanceID:      stringParam(params, "instance_id"),
			SSMDocumentName: stringParam(params, "ssm_document_name"),
		}, nil
	case "redis_flush":
		return &RedisFlush{
			BastionInstanceID: stringParam(params, "bastion_instance_id"),
			CacheEndpoint:     stringParam(params, "cache_endpoint"),
			SSMDocumentName:   stringParam(params, "ssm_document_name"),
		}, nil
	case "ch_slowdown":
		return &CHSlowdown{
			InstanceID:      stringParam(params, "instance_id"),
			LatencyMs:       intParam(params, "latency_ms"),
			Iface:           stringParam(params, "iface"),
			SSMDocumentName: stringParam(params, "ssm_document_name"),
		}, nil
	case "net_partition":
		return &NetPartition{
			SourceInstanceID:  stringParam(params, "source_instance_id"),
			DestIP:            stringParam(params, "dest_ip"),
			SourceServiceName: stringParam(params, "source_service_name"),
			DestServiceName:   stringParam(params, "dest_service_name"),
			SSMDocumentName:   stringParam(params, "ssm_document_name"),
		}, nil
	default:
		return nil, fmt.Errorf("chaos: unknown type %q (supported: kafka_kill, redis_flush, ch_slowdown, net_partition)", typ)
	}
}

// SupportedTypes returns the canonical list of chaos type strings. Used
// by the scenario validator so a typo surfaces with file:line:col.
func SupportedTypes() []string {
	return []string{"kafka_kill", "redis_flush", "ch_slowdown", "net_partition"}
}

// IsSupportedType reports whether typ is recognized.
func IsSupportedType(typ string) bool {
	for _, t := range SupportedTypes() {
		if t == typ {
			return true
		}
	}
	return false
}

// stringParam reads a string from the YAML-decoded params map. Missing
// or wrong-type entries return "".
func stringParam(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	v, ok := p[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// intParam reads an int from the YAML-decoded params map. yaml.v3 decodes
// integers as int (or int64 on 32-bit platforms) and floats as float64;
// we accept either silently.
func intParam(p map[string]any, key string) int {
	if p == nil {
		return 0
	}
	v, ok := p[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return 0
	}
}
