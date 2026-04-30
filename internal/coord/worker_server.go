package coord

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerHandler is the worker-side application logic. The HTTP server
// in this package handles routing + JSON marshaling + mTLS; the
// implementation of the actual run lives behind this interface so the
// transport layer stays testable.
//
// One WorkerHandler instance per worker process. Methods are called
// from arbitrary goroutines — implementations must be thread-safe.
type WorkerHandler interface {
	// Accept is called when an Assignment arrives. The handler validates
	// the assignment, returns an Acceptance, and (on accept) spawns a
	// goroutine that runs the scenario. Acceptance.Accepted=false means
	// the worker refuses the assignment; the coordinator will surface
	// the reason and may try a different worker.
	Accept(ctx context.Context, a *Assignment) Acceptance

	// Heartbeat returns the worker's current liveness + progress. Called
	// frequently — must be O(1).
	Heartbeat() Heartbeat

	// Abort cancels the in-progress run with the given reason. Drains
	// in-flight events and emits a final Report. Idempotent: a second
	// abort is a no-op.
	Abort(ctx context.Context, reason string) AbortResponse

	// LastReport returns the final Report once the run has completed,
	// nil while still running. The coordinator polls this after a
	// worker reports state="done" to retrieve the final stats. (Workers
	// also POST to /v1/report; this is the fallback / re-fetch path.)
	LastReport() *Report
}

// WorkerServerConfig is the construction-time bag.
type WorkerServerConfig struct {
	// ListenAddr is the host:port the server binds. Use ":7070" to
	// bind all interfaces.
	ListenAddr string

	// MTLS is the server's TLS material. Required for production;
	// nil + AllowInsecure for local dev.
	MTLS MTLSConfig

	// Handler is the worker-side application logic.
	Handler WorkerHandler

	// Logger receives one line per request. nil → discard.
	Logger func(format string, args ...any)
}

// WorkerServer hosts the four /v1/* endpoints over HTTP/2 + mTLS. One
// instance per worker process.
type WorkerServer struct {
	cfg    WorkerServerConfig
	server *http.Server
	mu     sync.Mutex
	addr   string
	closed atomic.Bool
}

// NewWorkerServer constructs a server. Validates the MTLS material
// up-front so a misconfigured worker fails to start rather than fail
// at first request.
func NewWorkerServer(cfg WorkerServerConfig) (*WorkerServer, error) {
	if cfg.Handler == nil {
		return nil, errors.New("worker server: Handler is required")
	}
	if _, err := ParseListenAddr(cfg.ListenAddr); err != nil {
		return nil, err
	}
	tlsCfg, err := cfg.MTLS.NewServerTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("worker server: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}

	w := &WorkerServer{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc(PathAssign, w.handleAssign)
	mux.HandleFunc(PathHeartbeat, w.handleHeartbeat)
	mux.HandleFunc(PathReport, w.handleReportFetch) // GET-style read of LastReport
	mux.HandleFunc(PathAbort, w.handleAbort)

	w.server = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		// Long body reads are expected on /v1/assign (full scenario +
		// manifest). 30s is generous; misuse usually surfaces as a
		// connection that never sends a full body.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return w, nil
}

// Start binds the listening socket and serves until ctx is cancelled or
// Close is called. Returns the bound addr (useful when binding port 0
// in tests).
//
// Blocks until the server stops; spawn it in a goroutine.
func (w *WorkerServer) Start(ctx context.Context) error {
	listener, err := tlsListen(w.cfg.ListenAddr, w.server.TLSConfig)
	if err != nil {
		return fmt.Errorf("worker server: listen: %w", err)
	}
	w.mu.Lock()
	w.addr = listener.Addr().String()
	w.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = w.Close()
	}()

	if err := w.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("worker server: serve: %w", err)
	}
	return nil
}

// Close stops the server with a 5s drain timeout.
func (w *WorkerServer) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.server.Shutdown(ctx)
}

// Addr returns the bound address. Empty until Start has bound the listener.
func (w *WorkerServer) Addr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.addr
}

// --- handlers -----------------------------------------------------------

func (w *WorkerServer) handleAssign(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 256*1024*1024))
	if err != nil {
		http.Error(rw, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var a Assignment
	if err := json.Unmarshal(body, &a); err != nil {
		http.Error(rw, "decode assignment: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.cfg.Logger("worker: received assignment runID=%s tenants=%d", a.RunID, len(a.TenantIDs))

	rsp := w.cfg.Handler.Accept(r.Context(), &a)
	rw.Header().Set(HeaderRunID, a.RunID)
	rw.Header().Set(HeaderWorker, rsp.WorkerID)
	rw.Header().Set("Content-Type", "application/json")
	if !rsp.Accepted {
		rw.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(rw).Encode(rsp)
}

func (w *WorkerServer) handleHeartbeat(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hb := w.cfg.Handler.Heartbeat()
	rw.Header().Set(HeaderRunID, hb.RunID)
	rw.Header().Set(HeaderWorker, hb.WorkerID)
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(hb)
}

func (w *WorkerServer) handleReportFetch(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rep := w.cfg.Handler.LastReport()
	if rep == nil {
		http.Error(rw, "report not yet available", http.StatusNotFound)
		return
	}
	rw.Header().Set(HeaderRunID, rep.RunID)
	rw.Header().Set(HeaderWorker, rep.WorkerID)
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(rep)
}

func (w *WorkerServer) handleAbort(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 32*1024))
	if err != nil {
		http.Error(rw, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req AbortRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(rw, "decode abort: "+err.Error(), http.StatusBadRequest)
		return
	}
	rsp := w.cfg.Handler.Abort(r.Context(), req.Reason)
	rw.Header().Set(HeaderRunID, req.RunID)
	rw.Header().Set(HeaderWorker, rsp.WorkerID)
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(rsp)
}

// tlsListen wraps net.Listen + tls.NewListener so we can override the
// addr in tests (binding port 0).
func tlsListen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	rawListen, err := netListen(addr)
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		return rawListen, nil
	}
	return tls.NewListener(rawListen, tlsCfg), nil
}

// netListen is a thin indirection so tests can substitute. Production
// uses net.Listen("tcp", addr).
var netListen = func(addr string) (net.Listener, error) {
	return defaultListenTCP(addr)
}
