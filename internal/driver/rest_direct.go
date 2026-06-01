package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// RESTDirectConfig configures the REST-direct driver.
type RESTDirectConfig struct {
	Target              aforo.Target
	HTTPClient          *http.Client
	RequestTimeout      time.Duration
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	// AdminToken, if non-empty, is used as a default Authorization header
	// when an event has no per-key token (e.g. when a fabricated key was
	// injected, the driver always uses the per-event Auth.Token).
	AdminToken string
}

// RESTDirect is the Session 4 reference driver. POSTs each event to
// /v1/ingest with bearer auth. Uses per-event Auth.Token so every request
// is correctly attributable to the synthetic customer's key.
type RESTDirect struct {
	cfg    RESTDirectConfig
	client *http.Client
	url    string
}

// NewRESTDirect constructs a REST-direct driver. Returns an error if the
// target doesn't have a usage-ingestor URL configured.
func NewRESTDirect(cfg RESTDirectConfig) (*RESTDirect, error) {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = 100
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = 90 * time.Second
	}
	if cfg.HTTPClient == nil {
		// Per spec: net/http with custom Transport, MaxIdleConnsPerHost: 100,
		// IdleConnTimeout: 90s, no auto-redirect.
		transport := &http.Transport{
			MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
			MaxIdleConns:          cfg.MaxIdleConnsPerHost * 4,
			IdleConnTimeout:       cfg.IdleConnTimeout,
			ResponseHeaderTimeout: cfg.RequestTimeout,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
		}
		cfg.HTTPClient = &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				// Disable auto-redirect — load tests hit the ingest endpoint
				// directly; redirects suggest a misconfiguration we want
				// to surface, not silently follow.
				return http.ErrUseLastResponse
			},
		}
	}

	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return nil, fmt.Errorf("rest_direct: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	return &RESTDirect{cfg: cfg, client: cfg.HTTPClient, url: url}, nil
}

// Name reports the driver's identifier — used in metrics labels.
func (d *RESTDirect) Name() string { return "rest_direct" }

// Submit serializes the event and POSTs it to /v1/ingest.
//
// Body shape: the generator's Envelope struct, marshaled to JSON. For
// negative_path=malformed events, Event.RawBody is sent as-is (already
// corrupt by design).
//
// Headers:
//
//	Authorization: Bearer <key.Secret>
//	X-Tenant-Id:   <event.TenantID>
//	X-Customer-Id: <event.CustomerID>   (defense-in-depth for some routes)
//	Content-Type:  application/json
func (d *RESTDirect) Submit(ctx context.Context, e *generator.Event) Result {
	res := Result{Event: e}
	startedAt := time.Now()

	var bodyBytes []byte
	if len(e.RawBody) > 0 {
		bodyBytes = e.RawBody
	} else {
		b, err := json.Marshal(e.Envelope)
		if err != nil {
			res.TransportErr = fmt.Errorf("marshal envelope: %w", err)
			res.Latency = time.Since(startedAt)
			return res
		}
		bodyBytes = b
	}
	res.BytesSent = len(bodyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(bodyBytes))
	if err != nil {
		res.TransportErr = fmt.Errorf("build request: %w", err)
		res.Latency = time.Since(startedAt)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.Auth.Token != "" {
		// Some Aforo deployments accept "Bearer <client_id>:<client_secret>"
		// for CLIENT_CREDENTIALS keys; for now we always send Bearer <token>.
		// SDKs normalize this same way.
		req.Header.Set("Authorization", "Bearer "+e.Auth.Token)
	} else if d.cfg.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.AdminToken)
	}
	// TenantID and EventID moved off Envelope (they're not part of the
	// backend's IngestUsageEventRequest contract) — read from the
	// in-memory Event instead. Drift-fix 2026-06-01.
	if e.TenantID != "" {
		req.Header.Set("X-Tenant-Id", e.TenantID)
	}
	if e.Envelope.CustomerID != "" {
		req.Header.Set("X-Customer-Id", e.Envelope.CustomerID)
	}
	if e.Auth.ClientID != "" {
		req.Header.Set("X-Client-Id", e.Auth.ClientID)
	}
	// Tag the request so server-side logs can correlate with run.json.
	if e.EventID != "" {
		req.Header.Set("X-Loadgen-Event-Id", e.EventID)
	}
	if e.NegativePath != "" {
		req.Header.Set("X-Loadgen-Negative-Path", string(e.NegativePath))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// Treat redirect-disabled errors and explicit ctx cancellations as
		// transport failures so the circuit breaker can react.
		res.TransportErr = err
		res.Latency = time.Since(startedAt)
		return res
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	// Drain a small prefix to count bytes; we don't need the response body.
	const maxRead = 4 << 10
	limited := io.LimitReader(resp.Body, maxRead)
	body, _ := io.ReadAll(limited)
	res.BytesRecv = len(body)
	res.Latency = time.Since(startedAt)

	// Coerce a 401/403 from the platform's validator output to consistent
	// shape if the server responded with a JSON body that includes an
	// "error" key — only used for runs that want to tag retry semantics.
	_ = body
	return res
}

// Close releases idle connections.
func (d *RESTDirect) Close() error {
	if t, ok := d.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}
