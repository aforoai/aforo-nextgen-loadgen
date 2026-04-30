package coord

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// WorkerClient is the coordinator's per-worker HTTP/2 + mTLS client.
// One client per worker — the *http.Client is reused for the lifetime
// of the run so HTTP/2 connection pooling works.
type WorkerClient struct {
	addr  string
	http  *http.Client
}

// NewWorkerClient constructs a client that POSTs/GETs to the worker at
// addr. addr is "host:port" — the client adds the https:// scheme.
func NewWorkerClient(addr string, mtls MTLSConfig, requestTimeout time.Duration) (*WorkerClient, error) {
	tlsCfg, err := mtls.NewClientTLSConfig()
	if err != nil {
		return nil, err
	}
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	transport := &http.Transport{
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      4,
		IdleConnTimeout:   60 * time.Second,
	}
	return &WorkerClient{
		addr: addr,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
		},
	}, nil
}

// Assign POSTs an Assignment. Returns the worker's Acceptance, or an
// error if the call fails or the worker rejects (Acceptance.Accepted=false
// surfaces as a non-nil error so the orchestrator can branch on it).
func (c *WorkerClient) Assign(ctx context.Context, a *Assignment) (Acceptance, error) {
	var rsp Acceptance
	if err := c.do(ctx, http.MethodPost, PathAssign, a, &rsp); err != nil {
		return rsp, err
	}
	if !rsp.Accepted {
		return rsp, fmt.Errorf("worker %s rejected assignment: %s", c.addr, rsp.Reason)
	}
	return rsp, nil
}

// Heartbeat polls the worker's liveness endpoint.
func (c *WorkerClient) Heartbeat(ctx context.Context) (Heartbeat, error) {
	var hb Heartbeat
	err := c.do(ctx, http.MethodGet, PathHeartbeat, nil, &hb)
	return hb, err
}

// FetchReport fetches the worker's final report. Returns an
// (Report, false) if the worker has not yet completed (HTTP 404).
func (c *WorkerClient) FetchReport(ctx context.Context) (*Report, bool, error) {
	var rep Report
	err := c.do(ctx, http.MethodGet, PathReport, nil, &rep)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &rep, true, nil
}

// Abort sends an abort request. Returns the worker's response.
func (c *WorkerClient) Abort(ctx context.Context, runID, reason string) (AbortResponse, error) {
	var rsp AbortResponse
	err := c.do(ctx, http.MethodPost, PathAbort, AbortRequest{RunID: runID, Reason: reason}, &rsp)
	return rsp, err
}

// Close releases idle connections in the underlying transport.
func (c *WorkerClient) Close() {
	if t, ok := c.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// errNotFound is returned by do when the worker responds 404. Used by
// FetchReport to signal "still running" without an error.
var errNotFound = errors.New("not found")

// do is the shared HTTP call: marshal body → POST/GET → unmarshal response.
func (c *WorkerClient) do(ctx context.Context, method, path string, body, into any) error {
	var bodyR io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %T: %w", body, err)
		}
		bodyR = bytes.NewReader(buf)
	}
	url := "https://" + c.addr + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyR)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rsp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("worker %s %s: %w", c.addr, path, err)
	}
	defer rsp.Body.Close()

	if rsp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if rsp.StatusCode >= 400 && rsp.StatusCode != http.StatusConflict {
		// Read the body for diagnostic context but cap to keep
		// pathological bodies from blowing memory.
		buf, _ := io.ReadAll(io.LimitReader(rsp.Body, 8*1024))
		return fmt.Errorf("worker %s %s: HTTP %d: %s", c.addr, path, rsp.StatusCode, string(buf))
	}
	if into == nil {
		return nil
	}
	dec := json.NewDecoder(rsp.Body)
	dec.DisallowUnknownFields() // catch protocol mismatches during dev
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// pingTLS dials the worker addr with the given client TLS config and
// returns nil if the TCP+TLS handshake succeeds. Used by the
// coordinator's pre-flight check to confirm every worker is reachable
// before assigning work.
//
// Times out at 5s — production runs should never wait longer for a
// healthy worker.
func pingTLS(addr string, mtls MTLSConfig) error {
	tlsCfg, err := mtls.NewClientTLSConfig()
	if err != nil {
		return err
	}
	dialer := &tls.Dialer{Config: tlsCfg}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
