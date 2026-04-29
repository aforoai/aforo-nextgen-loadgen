// Package metrics exposes the Prometheus + pprof endpoints the runner
// surfaces while a load test is in flight.
//
// All counters/gauges/histograms are constructed once at server start and
// shared across goroutines — every Prometheus metric is goroutine-safe by
// design. The runner threads the *Registry into the generator + driver +
// pool callbacks so they can increment counters without coupling those
// packages to the prometheus client library.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric the load run exposes. One Registry is built
// per `aforo-loadgen run` invocation; the metrics server registers the
// underlying *prometheus.Registry and serves /metrics on the chosen port.
type Registry struct {
	prom *prometheus.Registry

	EventsSent          *prometheus.CounterVec
	EventsFailed        *prometheus.CounterVec
	NegativePathTotal   *prometheus.CounterVec
	IngestLatency       *prometheus.HistogramVec
	BackpressureActive  prometheus.Gauge
	CircuitBreakerState *prometheus.GaugeVec
	TenantsActive       prometheus.Gauge
}

// NewRegistry constructs a fresh registry with all metrics declared.
func NewRegistry() *Registry {
	r := prometheus.NewRegistry()
	reg := &Registry{prom: r}

	reg.EventsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "events_sent_total",
		Help: "Events successfully accepted by the platform (2xx).",
	}, []string{"product_type", "archetype", "ingestion_path"})

	reg.EventsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "events_failed_total",
		Help: "Events the platform failed (or could not receive). error_class is one of: client, server, transport, circuit_open, expected.",
	}, []string{"product_type", "archetype", "error_class"})

	reg.NegativePathTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "events_negative_path_total",
		Help: "Events generated with intentional fault injection.",
	}, []string{"negative_path"})

	reg.IngestLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ingest_latency_seconds",
		Help:    "Distribution of round-trip latencies for /v1/ingest POSTs.",
		Buckets: prometheus.ExponentialBucketsRange(0.001, 30.0, 16),
	}, []string{"product_type", "archetype"})

	reg.BackpressureActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "backpressure_active",
		Help: "1 when backpressure is throttling the generator, 0 otherwise.",
	})

	reg.CircuitBreakerState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "circuit_breaker_state",
		Help: "Driver circuit breaker state: 0=closed, 1=half_open, 2=open.",
	}, []string{"driver"})

	reg.TenantsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tenants_active",
		Help: "Distinct tenants the generator can drive traffic to.",
	})

	r.MustRegister(
		reg.EventsSent,
		reg.EventsFailed,
		reg.NegativePathTotal,
		reg.IngestLatency,
		reg.BackpressureActive,
		reg.CircuitBreakerState,
		reg.TenantsActive,
	)
	return reg
}

// PromRegistry exposes the underlying *prometheus.Registry.
// Tests use it to gather snapshots; runner uses it to wrap the HTTP handler.
func (r *Registry) PromRegistry() *prometheus.Registry { return r.prom }

// Server runs an HTTP server exposing /metrics and (if enabled) /debug/pprof.
type Server struct {
	listener net.Listener
	srv      *http.Server
	addr     string
	mu       sync.Mutex
	closed   bool
}

// ServerConfig configures the metrics server.
type ServerConfig struct {
	Registry     *Registry
	Addr         string // e.g. ":9095" or "127.0.0.1:9095"
	EnablePprof  bool
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// NewServer creates and binds the listener but does not start serving.
// Returns an error if the address can't be bound.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Registry == nil {
		return nil, errors.New("metrics: Registry is required")
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 5 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(cfg.Registry.PromRegistry(), promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if cfg.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: bind %s: %w", cfg.Addr, err)
	}
	return &Server{
		listener: ln,
		srv:      srv,
		addr:     ln.Addr().String(),
	}, nil
}

// Addr returns the bound address (useful when Addr is ":0").
func (s *Server) Addr() string { return s.addr }

// Start serves /metrics on the bound listener until ctx is cancelled or
// Close is called. Blocks; spawn in a goroutine.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	if err := s.srv.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops the server immediately. Safe to call multiple times.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
