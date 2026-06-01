package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// Default transport tuning.
const (
	defaultMaxConcurrency = 50
	defaultMinInterval    = 200 * time.Millisecond
	defaultMaxRetries     = 3
	defaultBaseBackoff    = 250 * time.Millisecond
	defaultRequestTimeout = 30 * time.Second
	defaultBodyTruncate   = 4 << 10 // 4 KiB — caps APIError.Body length
)

// ClientConfig configures HTTP transport. Zero values fall back to the
// default constants above so callers only set what they need to override.
type ClientConfig struct {
	Target         aforo.Target
	BearerToken    string
	HTTPClient     *http.Client  // nil → default with reasonable timeouts
	MaxConcurrency int           // 0 → 50
	MinInterval    time.Duration // 0 → 200ms
	MaxRetries     int           // 0 → 3
	BaseBackoff    time.Duration // 0 → 250ms
	RequestTimeout time.Duration // 0 → 30s
	DryRun         bool          // true → log requests but never send them
	Logger         *log.Logger   // nil → log.Default()
	Now            func() time.Time
}

// Client wraps an HTTP transport with auth, rate limiting, retry, and
// idempotency. One Client serves the entire seed run; methods are safe for
// concurrent use.
//
// The rate limiter is a single-token bucket that refills every MinInterval —
// callers wait for a token before issuing a request. The semaphore caps
// in-flight requests at MaxConcurrency. Together they prevent the seed
// harness from DDoS'ing the admin API while still maintaining steady
// throughput.
type Client struct {
	cfg        ClientConfig
	httpClient *http.Client
	limiter    *rateLimiter
	sem        chan struct{}
	logger     *log.Logger
	now        func() time.Time

	// dryRunCounter is incremented on every would-be request in dry-run mode.
	// Tests assert against it to verify call shape without networking.
	dryRunMu      sync.Mutex
	dryRunRecords []DryRunRecord
}

// DryRunRecord captures a single request the client would have sent. Tests
// pull these out via DryRunRecords() to assert call sequences and bodies.
type DryRunRecord struct {
	Method  string
	URL     string
	Headers http.Header
	Body    json.RawMessage
}

// NewClient constructs a Client. Returns ErrAuthMissing if BearerToken is
// blank and DryRun is false — fail fast rather than 401 mid-run.
func NewClient(cfg ClientConfig) (*Client, error) {
	if !cfg.DryRun && cfg.BearerToken == "" {
		return nil, aforo.ErrAuthMissing
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = defaultMinInterval
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBaseBackoff
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.RequestTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		cfg:        cfg,
		httpClient: hc,
		limiter:    newRateLimiter(cfg.MinInterval),
		sem:        make(chan struct{}, cfg.MaxConcurrency),
		logger:     logger,
		now:        now,
	}, nil
}

// Close releases the rate-limiter goroutine. Safe to call multiple times.
func (c *Client) Close() error {
	c.limiter.Close()
	return nil
}

// Target returns the resolved Target so callers can build URLs.
func (c *Client) Target() aforo.Target { return c.cfg.Target }

// DryRun reports whether this client is in dry-run mode (no network calls).
func (c *Client) DryRun() bool { return c.cfg.DryRun }

// DryRunRecords returns the captured request records — only populated when
// DryRun is true. Callers may safely iterate the returned slice; it's a copy.
func (c *Client) DryRunRecords() []DryRunRecord {
	c.dryRunMu.Lock()
	defer c.dryRunMu.Unlock()
	out := make([]DryRunRecord, len(c.dryRunRecords))
	copy(out, c.dryRunRecords)
	return out
}

// RequestOptions tweak per-request behavior.
type RequestOptions struct {
	// TenantID, if non-empty, is sent as X-Tenant-Id. Internal admin endpoints
	// (organization-service /internal/tenants) typically don't require this.
	TenantID string
	// OrgID, if non-empty, is sent as X-Organization-Id (billing-platform).
	OrgID string
	// UserID, if non-empty, is sent as X-User-Id for audit attribution.
	UserID string
	// Idempotency, if non-empty, is sent as Idempotency-Key header. Aforo's
	// services honor this on POSTs to /subscriptions, /invoices, etc.
	Idempotency string
	// Query parameters appended to the URL.
	Query url.Values
}

// Do issues an HTTP request and decodes the JSON response into out (if non-nil).
// Honors the rate limiter, semaphore, retries, and dry-run flag.
//
// On non-2xx, returns *aforo.APIError so callers can inspect status via the
// IsNotFound / IsConflict / IsRetryable helpers in the aforo package.
func (c *Client) Do(ctx context.Context, method string, fullURL string, body any, out any, opts RequestOptions) error {
	// Marshal body up front so the same bytes are reused across retries.
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body for %s %s: %w", method, fullURL, err)
		}
		bodyBytes = b
	}

	if opts.Query != nil {
		fullURL = appendQuery(fullURL, opts.Query)
	}

	if c.cfg.DryRun {
		return c.recordDryRun(method, fullURL, bodyBytes, opts, out)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.backoffDuration(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		if err := c.acquireSlot(ctx); err != nil {
			return err
		}
		err := c.doOnce(ctx, method, fullURL, bodyBytes, out, opts)
		c.releaseSlot()

		if err == nil {
			return nil
		}
		lastErr = err
		if !aforo.IsRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

func (c *Client) backoffDuration(attempt int) time.Duration {
	// Exponential: base * 2^(attempt-1), with a cap so a 5xx storm doesn't
	// pause the seed harness for minutes.
	base := c.cfg.BaseBackoff
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > 8*time.Second {
			d = 8 * time.Second
			break
		}
	}
	return d
}

func (c *Client) acquireSlot(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.limiter.Wait(ctx); err != nil {
		<-c.sem
		return err
	}
	return nil
}

func (c *Client) releaseSlot() { <-c.sem }

func (c *Client) doOnce(ctx context.Context, method, fullURL string, body []byte, out any, opts RequestOptions) error {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}
	if opts.TenantID != "" {
		req.Header.Set("X-Tenant-Id", opts.TenantID)
	}
	if opts.OrgID != "" {
		req.Header.Set("X-Organization-Id", opts.OrgID)
	}
	if opts.UserID != "" {
		req.Header.Set("X-User-Id", opts.UserID)
	}
	if opts.Idempotency != "" {
		req.Header.Set("Idempotency-Key", opts.Idempotency)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &aforo.APIError{Method: method, URL: fullURL, UnderlyingErr: err}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, defaultBodyTruncate*4))
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return &aforo.APIError{Method: method, URL: fullURL, Status: resp.StatusCode, UnderlyingErr: readErr}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := string(respBody)
		if len(body) > defaultBodyTruncate {
			body = body[:defaultBodyTruncate] + "…(truncated)"
		}
		return &aforo.APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: body}
	}

	if out != nil && len(respBody) > 0 {
		if err := unmarshalAforoResponse(respBody, out); err != nil {
			return &aforo.APIError{Method: method, URL: fullURL, Status: resp.StatusCode, Body: string(respBody), UnderlyingErr: fmt.Errorf("unmarshal: %w", err)}
		}
	}
	return nil
}

// unmarshalAforoResponse decodes a backend response into out. Aforo services
// return one of these shapes:
//
//  1. ApiResponseAdvice envelope: {"success":true,"data":<inner>,"meta":...}
//     — most controllers, including everything that goes through the standard
//     ResponseEntity advice.
//  2. Plain entity / plain list  — a handful of internal admin endpoints
//     (e.g. LoadgenInternalTenantController) writeJson the body directly
//     and bypass the envelope. List responses sometimes carry their own
//     {"data":[...]} key without the success/meta fields.
//
// History note (do not regress): a prior version tried `json.Unmarshal(body,
// out)` first and only fell through to the envelope path on error. That
// silently dropped data on every enveloped response — Go's json package
// ignores unknown fields, so unmarshalling {"success":true,"data":{...}}
// into a typed entity struct like tenantResponse{ID,Name,...} succeeded with
// err==nil and every field zero-valued. Loadgen would then record blank IDs
// in the manifest, log "created" success, and the backend either had no row
// or had a row the lookup couldn't find again. See README + this file's
// commit history for the symptoms (developer report 2026-06-01).
//
// New behavior: probe for the standard envelope by checking both `success`
// and `data` keys. If present, unmarshal data into out. Otherwise fall back
// to a direct unmarshal so unwrapped/list shapes still work.
func unmarshalAforoResponse(respBody []byte, out any) error {
	if isEnvelopeResponse(respBody) {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(respBody, &env); err != nil {
			// Body claimed to be an envelope but is malformed — surface the
			// error rather than silently zeroing out.
			return err
		}
		if len(env.Data) == 0 || string(env.Data) == "null" {
			// Envelope present but data is empty/null. Leave out untouched
			// (its zero value); a missing payload is not an error.
			return nil
		}
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(respBody, out)
}

// isEnvelopeResponse reports whether respBody looks like Aforo's standard
// {success, data, meta} envelope. We require BOTH `success` and `data` to
// be present at the top level — checking only `data` is risky because some
// entities legitimately carry a `data` field on the wire (e.g. webhook
// payload bodies). The combination is unique enough to be unambiguous.
func isEnvelopeResponse(respBody []byte) bool {
	// Cheap shape filter: envelopes are JSON objects, not arrays/strings.
	trimmed := bytes.TrimLeft(respBody, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &probe); err != nil {
		return false
	}
	_, hasSuccess := probe["success"]
	_, hasData := probe["data"]
	return hasSuccess && hasData
}

func (c *Client) recordDryRun(method, fullURL string, body []byte, opts RequestOptions, out any) error {
	rec := DryRunRecord{
		Method:  method,
		URL:     fullURL,
		Headers: http.Header{},
	}
	if c.cfg.BearerToken != "" {
		rec.Headers.Set("Authorization", "Bearer ***")
	}
	if opts.TenantID != "" {
		rec.Headers.Set("X-Tenant-Id", opts.TenantID)
	}
	if opts.OrgID != "" {
		rec.Headers.Set("X-Organization-Id", opts.OrgID)
	}
	if opts.UserID != "" {
		rec.Headers.Set("X-User-Id", opts.UserID)
	}
	if opts.Idempotency != "" {
		rec.Headers.Set("Idempotency-Key", opts.Idempotency)
	}
	if body != nil {
		rec.Body = append(json.RawMessage(nil), body...)
	}

	c.dryRunMu.Lock()
	c.dryRunRecords = append(c.dryRunRecords, rec)
	c.dryRunMu.Unlock()

	c.logger.Printf("[dry-run] %s %s body=%dB", method, fullURL, len(body))

	// Synthesize a benign response — empty object — so callers that decode
	// into a typed struct don't crash. Tests should assert against the
	// recorded request rather than the (zero-valued) decoded response.
	if out != nil {
		return json.Unmarshal([]byte(`{}`), out)
	}
	return nil
}

func appendQuery(rawURL string, q url.Values) string {
	if len(q) == 0 {
		return rawURL
	}
	if u, err := url.Parse(rawURL); err == nil {
		existing := u.Query()
		for k, vs := range q {
			for _, v := range vs {
				existing.Add(k, v)
			}
		}
		u.RawQuery = existing.Encode()
		return u.String()
	}
	// Fallback for non-URL strings — should not happen since callers build via
	// Target.Path which always returns a parseable URL.
	sep := "?"
	for k, vs := range q {
		for _, v := range vs {
			rawURL += sep + url.QueryEscape(k) + "=" + url.QueryEscape(v)
			sep = "&"
		}
	}
	return rawURL
}

// rateLimiter is a single-token bucket that refills every interval. Callers
// Wait for a token before issuing a request. Stop releases the goroutine.
type rateLimiter struct {
	tokens   chan struct{}
	stop     chan struct{}
	interval time.Duration
	closed   sync.Once
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	rl := &rateLimiter{
		tokens:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
		interval: interval,
	}
	rl.tokens <- struct{}{} // start with one token so the first call doesn't wait
	go rl.refill()
	return rl
}

func (rl *rateLimiter) refill() {
	t := time.NewTicker(rl.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			select {
			case rl.tokens <- struct{}{}:
			default:
				// bucket already full — drop the token
			}
		case <-rl.stop:
			return
		}
	}
}

func (rl *rateLimiter) Wait(ctx context.Context) error {
	select {
	case <-rl.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rl *rateLimiter) Close() {
	rl.closed.Do(func() { close(rl.stop) })
}
