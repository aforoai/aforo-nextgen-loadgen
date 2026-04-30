package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// HTTPDoer is the minimal HTTP interface the transition modules need. The
// stdlib *http.Client satisfies it. Tests inject a stub.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the lightweight HTTP client used by every transition module.
// It is deliberately separate from internal/seed.Client because the seed
// client carries seed-time concerns (rate limit, semaphore, dry-run capture)
// that the lifecycle agent doesn't need at run-time pace.
//
// Concurrency: safe for use from many goroutines (the underlying HTTPDoer
// must be — *http.Client is).
//
// Auth: bearer token + tenant header on every call. Idempotency-Key is
// applied when caller supplies it (we mint per-transition keys to make
// retries safe).
type Client struct {
	target    aforo.Target
	doer      HTTPDoer
	token     string
	timeout   time.Duration
	userAgent string
}

// ClientConfig configures a Client. Zero defaults applied for omitted fields.
type ClientConfig struct {
	Target    aforo.Target
	Token     string
	HTTP      HTTPDoer      // nil → stdlib http.Client with sane defaults
	Timeout   time.Duration // 0 → 30s
	UserAgent string        // 0 → "aforo-loadgen/lifecycle"
}

// NewClient constructs a Client. Returns an error only on misconfiguration.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "aforo-loadgen/lifecycle"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		}
	}
	return &Client{
		target:    cfg.Target,
		doer:      cfg.HTTP,
		token:     cfg.Token,
		timeout:   cfg.Timeout,
		userAgent: cfg.UserAgent,
	}, nil
}

// Target exposes the underlying Target — transition modules use it to
// build URLs.
func (c *Client) Target() aforo.Target { return c.target }

// PostJSON sends a POST with JSON body to the resolved URL on the given
// service. Returns parsed body on 2xx, *aforo.APIError on non-2xx.
//
// idempotencyKey, when non-empty, is sent as the Idempotency-Key header
// — recommended for every non-GET call to a state-mutating endpoint.
func (c *Client) PostJSON(
	ctx context.Context,
	svc aforo.Service,
	path string,
	tenantID string,
	idempotencyKey string,
	body any,
	out any,
) (int, error) {
	url, err := c.target.Path(svc, path)
	if err != nil {
		return 0, err
	}
	return c.do(ctx, http.MethodPost, url, tenantID, idempotencyKey, body, out)
}

// GetJSON is the read-side counterpart. No body. Returns the response body
// (decoded into out) on 2xx, error otherwise.
func (c *Client) GetJSON(
	ctx context.Context,
	svc aforo.Service,
	path string,
	tenantID string,
	out any,
) (int, error) {
	url, err := c.target.Path(svc, path)
	if err != nil {
		return 0, err
	}
	return c.do(ctx, http.MethodGet, url, tenantID, "", nil, out)
}

// DeleteJSON is the cancel/delete counterpart.
func (c *Client) DeleteJSON(
	ctx context.Context,
	svc aforo.Service,
	path string,
	tenantID string,
	idempotencyKey string,
	out any,
) (int, error) {
	url, err := c.target.Path(svc, path)
	if err != nil {
		return 0, err
	}
	return c.do(ctx, http.MethodDelete, url, tenantID, idempotencyKey, nil, out)
}

func (c *Client) do(
	ctx context.Context,
	method, url, tenantID, idempotency string,
	body any,
	out any,
) (int, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("lifecycle: marshal %s body: %w", method, err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("lifecycle: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, &aforo.APIError{Method: method, URL: url, UnderlyingErr: err}
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return resp.StatusCode, &aforo.APIError{
			Method: method, URL: url, Status: resp.StatusCode, UnderlyingErr: readErr,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyStr := string(respBody)
		if len(bodyStr) > 4096 {
			bodyStr = bodyStr[:4096] + "…(truncated)"
		}
		return resp.StatusCode, &aforo.APIError{
			Method: method, URL: url, Status: resp.StatusCode, Body: bodyStr,
		}
	}

	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp.StatusCode, &aforo.APIError{
				Method: method, URL: url, Status: resp.StatusCode, Body: string(respBody),
				UnderlyingErr: fmt.Errorf("decode response: %w", err),
			}
		}
	}
	return resp.StatusCode, nil
}

// HTTPStatus extracts the HTTP status from an error returned by Post/Get/Delete.
// Returns zero if the error isn't an *aforo.APIError.
func HTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	var ae *aforo.APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// IsRetryable reports whether the error is one a caller could retry. Same
// semantics as aforo.IsRetryable but importable from this package.
func IsRetryable(err error) bool { return aforo.IsRetryable(err) }

// FormatError condenses an error to a single short string for the
// transitions.jsonl `error` field. We deliberately avoid full stack traces
// — readers of transitions.jsonl want to see "409 conflict" not 500 lines
// of context.
func FormatError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
