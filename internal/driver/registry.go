package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// RegistryConfig describes the per-target plumbing all drivers share.
type RegistryConfig struct {
	Target              aforo.Target
	AdminToken          string
	RequestTimeout      time.Duration
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration

	// WebhookSources is the per-tenant webhook source map. When non-nil,
	// the webhook driver uses these for routing+signing; when nil, it
	// falls back to a synthetic stub (which 404s — useful for shape-only
	// load tests).
	WebhookSources map[string]WebhookSource

	// CSVBatchSize tunes the csv_upload driver's per-tenant buffer.
	// Default 100.
	CSVBatchSize int
}

// Registry resolves an ingestion-path name (e.g. "sdk_node", "gateway_kong")
// to a Driver. Constructed once per run; lazy-initializes drivers on first
// use so a scenario that never hits gateway_envoy doesn't pay for its
// HTTP client.
//
// The runner queries the registry per worker pool. For a single worker
// pool that fans across many ingestion paths, the runner builds one pool
// per driver and routes events via a Multiplexer (see multiplex.go).
type Registry struct {
	cfg     RegistryConfig
	mu      sync.Mutex
	cache   map[string]Driver
	closers []func() error
}

// NewRegistry constructs a registry. Validates the target up front so that
// configuration errors surface before the run starts.
func NewRegistry(cfg RegistryConfig) (*Registry, error) {
	if cfg.Target.Name == "" {
		return nil, fmt.Errorf("registry: target is required")
	}
	if _, err := cfg.Target.URL(aforo.ServiceUsageIngestor); err != nil {
		return nil, fmt.Errorf("registry: target %s lacks usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &Registry{cfg: cfg, cache: map[string]Driver{}}, nil
}

// Get returns the driver for a given ingestion-path name, constructing it
// on first call. Unknown names return ErrUnknownIngestionPath so the runner
// can reject the scenario before any events are emitted.
func (r *Registry) Get(name string) (Driver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.cache[name]; ok {
		return d, nil
	}
	d, err := r.construct(name)
	if err != nil {
		return nil, err
	}
	r.cache[name] = d
	r.closers = append(r.closers, d.Close)
	return d, nil
}

// AllNames returns the canonical list of supported ingestion-path names.
// Used by the validator + tests + docs generation.
func AllNames() []string {
	return []string{
		"rest_direct",
		"sdk_node", "sdk_python", "sdk_java", "sdk_go",
		"gateway_kong", "gateway_apigee", "gateway_aws",
		"gateway_azure", "gateway_mulesoft",
		"gateway_apisix", "gateway_tyk", "gateway_gravitee", "gateway_envoy",
		"webhook_receiver", "csv_upload",
	}
}

// IsKnown reports whether name is a recognized ingestion path.
func IsKnown(name string) bool {
	for _, n := range AllNames() {
		if n == name {
			return true
		}
	}
	return false
}

// Close closes every driver constructed by this registry. Idempotent —
// subsequent calls are no-ops.
func (r *Registry) Close() error {
	r.mu.Lock()
	closers := r.closers
	r.closers = nil
	r.mu.Unlock()
	var firstErr error
	for _, c := range closers {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// construct dispatches to the right NewXxx for the given name.
// Caller holds r.mu.
func (r *Registry) construct(name string) (Driver, error) {
	hb := HTTPBaseConfig{
		Target:              r.cfg.Target,
		AdminToken:          r.cfg.AdminToken,
		RequestTimeout:      r.cfg.RequestTimeout,
		MaxIdleConnsPerHost: r.cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     r.cfg.IdleConnTimeout,
	}
	switch name {
	case "rest_direct":
		return NewRESTDirect(RESTDirectConfig{
			Target:              hb.Target,
			HTTPClient:          hb.HTTPClient,
			RequestTimeout:      hb.RequestTimeout,
			MaxIdleConnsPerHost: hb.MaxIdleConnsPerHost,
			IdleConnTimeout:     hb.IdleConnTimeout,
			AdminToken:          hb.AdminToken,
		})
	case "sdk_node":
		return NewSDKNode(hb)
	case "sdk_python":
		return NewSDKPython(hb)
	case "sdk_java":
		return NewSDKJava(hb)
	case "sdk_go":
		return NewSDKGo(hb)
	case "gateway_kong":
		return NewGatewayKong(hb)
	case "gateway_apigee":
		return NewGatewayApigee(hb)
	case "gateway_aws":
		return NewGatewayAWS(hb)
	case "gateway_azure":
		return NewGatewayAzure(hb)
	case "gateway_mulesoft":
		return NewGatewayMuleSoft(hb)
	case "gateway_apisix":
		return NewGatewayAPISIX(hb)
	case "gateway_tyk":
		return NewGatewayTyk(hb)
	case "gateway_gravitee":
		return NewGatewayGravitee(hb)
	case "gateway_envoy":
		return NewGatewayEnvoy(hb)
	case "webhook_receiver":
		return NewWebhook(WebhookConfig{
			HTTPBaseConfig: hb,
			Sources:        r.cfg.WebhookSources,
		})
	case "csv_upload":
		return NewCSVUpload(CSVUploadConfig{
			HTTPBaseConfig: hb,
			BatchSize:      r.cfg.CSVBatchSize,
		})
	}
	return nil, fmt.Errorf("driver registry: unknown ingestion path %q", name)
}

// Multiplex wraps a registry as a single Driver, dispatching each event
// to the per-event ingestion-path's underlying driver. The runner uses
// this to keep a single pool while fanning to many drivers — a natural
// fit for tenant-fairness scheduling because the pool's worker pool is
// shared across paths.
//
// All sub-drivers are looked up lazily; a malformed scenario referencing
// an unknown path yields a transport-class failure on the affected event,
// not a panic.
type Multiplex struct {
	reg *Registry
}

// NewMultiplex returns a Multiplex.
func NewMultiplex(reg *Registry) *Multiplex { return &Multiplex{reg: reg} }

// Name returns "multiplex" — used by the metrics circuit-breaker label.
// Per-driver outcomes are still recorded with the right ingestion-path
// label via Result.Event.IngestionPath.
func (m *Multiplex) Name() string { return "multiplex" }

// Submit dispatches to the per-event driver.
func (m *Multiplex) Submit(ctx context.Context, e *generator.Event) Result {
	if e == nil {
		return Result{}
	}
	path := e.IngestionPath
	if path == "" {
		path = "rest_direct"
	}
	d, err := m.reg.Get(path)
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	return d.Submit(ctx, e)
}

// Close closes the underlying registry.
func (m *Multiplex) Close() error { return m.reg.Close() }
