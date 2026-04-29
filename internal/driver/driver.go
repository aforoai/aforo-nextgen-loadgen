// Package driver implements the per-ingestion-path transports that send
// generated events to the Aforo platform.
//
// Session 4 ships rest_direct only — every other ingestion-path string in
// the scenario YAML resolves to the rest_direct driver as a default until
// future sessions wire SDK / gateway / webhook drivers.
//
// Drivers are stateless from the runner's perspective: Submit takes one
// event and returns a Result. Backpressure and circuit-breaker policy live
// alongside the driver in this package because they share the same error
// semantics (HTTP status, transport class, retry-after).
package driver

import (
	"context"
	"errors"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// Result classifies the outcome of one Submit call. Only one of Status or
// TransportErr is meaningful per Result.
type Result struct {
	Event        *generator.Event
	Status       int           // HTTP status; 0 if TransportErr is set
	Latency      time.Duration // wall-clock from request build to response
	BytesSent    int           // body bytes sent (counter for net I/O)
	BytesRecv    int           // body bytes received
	TransportErr error         // non-nil when Status==0 (DNS, dial, EOF, etc.)
}

// IsSuccess returns true for 2xx responses.
func (r Result) IsSuccess() bool { return r.Status >= 200 && r.Status < 300 }

// IsClientError returns true for 4xx responses — the platform rejected the
// event for shape reasons. Negative-path injectors expect these.
func (r Result) IsClientError() bool { return r.Status >= 400 && r.Status < 500 }

// IsServerError returns true for 5xx responses — the platform was unable
// to handle the event. Counts toward circuit-breaker error rate.
func (r Result) IsServerError() bool { return r.Status >= 500 }

// IsTransport returns true for transport-level failures (no HTTP response).
// Counts toward circuit-breaker error rate.
func (r Result) IsTransport() bool { return r.TransportErr != nil }

// IsExpectedFailure returns true when the event was negative-path-tagged
// AND the response is consistent with that tag's expected outcome:
//
//	future_event   → 4xx
//	malformed      → 4xx
//	wrong_auth     → 401/403
//	stale_key      → 401/403
//	oversize       → 4xx (413 typical)
//	late_event     → 2xx (accepted but flagged late)
//
// Used by the runner so backpressure / circuit-breaker logic doesn't
// flap when the scenario is intentionally injecting faults.
func (r Result) IsExpectedFailure() bool {
	if r.Event == nil {
		return false
	}
	switch r.Event.NegativePath {
	case generator.NPLate:
		return r.IsSuccess()
	case generator.NPFuture, generator.NPMalformed, generator.NPOversize:
		return r.IsClientError()
	case generator.NPWrongAuth, generator.NPStaleKey:
		return r.Status == 401 || r.Status == 403
	}
	return false
}

// Driver dispatches events to the platform. Implementations:
//   - rest_direct (Session 4)
//   - sdk_node, sdk_python, sdk_java, sdk_go (Session 8)
//   - gateway_kong, gateway_apigee, etc. (Session 8)
//   - webhook_receiver, csv_upload (Session 9)
type Driver interface {
	Name() string
	Submit(ctx context.Context, e *generator.Event) Result
	Close() error
}

// ErrCircuitOpen is returned by Submit when the circuit breaker is open.
// Pool retries / drops based on its own policy.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// ErrPaused is returned when the driver is paused (e.g. during a half-open
// probe window). Pool waits before retry.
var ErrPaused = errors.New("driver paused")
