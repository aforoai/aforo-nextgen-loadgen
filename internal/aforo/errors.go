package aforo

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is the structured error returned by the HTTP client for any non-2xx
// response. Callers inspect Status to decide whether to retry, treat-as-existing
// (409 Conflict on idempotent create), or surface to the user.
//
// Body is the raw response body — kept short (truncated to 4KB) so error
// messages stay readable in logs.
type APIError struct {
	Status        int    // HTTP status
	Method        string // HTTP method
	URL           string // target URL (with query string redacted of secrets)
	Body          string // response body (truncated)
	UnderlyingErr error
}

// Error renders status, method, URL, and a snippet of the body. Loadgen log
// output is line-oriented so we keep the message single-line.
func (e *APIError) Error() string {
	if e.UnderlyingErr != nil {
		return fmt.Sprintf("aforo %s %s: %v", e.Method, e.URL, e.UnderlyingErr)
	}
	return fmt.Sprintf("aforo %s %s: status=%d body=%s", e.Method, e.URL, e.Status, e.Body)
}

// Unwrap exposes the transport-level error, if any.
func (e *APIError) Unwrap() error { return e.UnderlyingErr }

// IsNotFound reports whether the error is a 404 — used by idempotent create
// flows where a "no existing entity" response means we should POST.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusNotFound
	}
	return false
}

// IsConflict reports whether the error is a 409 — typically returned on
// idempotent POSTs when the entity already exists (we treat as success).
func IsConflict(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusConflict
	}
	return false
}

// IsUnauthorized reports whether the error is 401 or 403 — used by the stale
// key sanity check (a revoked key MUST be rejected with one of these).
func IsUnauthorized(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden
	}
	return false
}

// IsRetryable reports whether the error is one we should retry — 5xx, 408,
// 429, or a transport-level error. 4xx errors other than 408/429 are not
// retryable; the caller has sent something the server can't process.
func IsRetryable(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		// Transport-level errors (DNS, dial, EOF) are retryable.
		return err != nil
	}
	if ae.UnderlyingErr != nil {
		return true
	}
	switch {
	case ae.Status >= 500:
		return true
	case ae.Status == http.StatusRequestTimeout:
		return true
	case ae.Status == http.StatusTooManyRequests:
		return true
	}
	return false
}

// ErrAuthMissing is returned when the bearer token isn't set — surfaced
// before any HTTP call rather than as a 401 mid-run.
var ErrAuthMissing = errors.New("AFORO_ADMIN_TOKEN environment variable is not set")
