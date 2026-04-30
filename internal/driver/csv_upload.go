package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// CSVUploadConfig configures the CSV bulk-upload driver.
type CSVUploadConfig struct {
	HTTPBaseConfig
	// BatchSize is the number of events accumulated before uploading.
	// Default 100. The platform's FileUploadController accepts up to 50MB
	// per file; at ~200B per CSV row that's a 250K-row ceiling. We default
	// far below that to keep a single failed upload from losing a lot.
	BatchSize int
}

// CSVUpload accumulates events and uploads them as a multipart CSV file
// to /v1/ingest/upload (the platform's FileUploadController).
//
// Behavior diverges from the other drivers in two ways:
//
//  1. Each Submit accumulates into a per-tenant buffer rather than firing
//     immediately. When a tenant's buffer reaches BatchSize, it's flushed
//     synchronously inside Submit and the Result reflects the upload's
//     overall outcome (the same Result is reported for every event in the
//     batch — see Result.BytesSent for accurate per-event accounting).
//  2. The flush sends a multipart/form-data upload with `file` (the CSV
//     bytes), `defaultMetricName`, and `defaultCustomerId`. Tenant
//     identification flows via the standard X-Tenant-Id header and
//     bearer auth like every other path.
//
// The driver uses one buffer per tenant so a flush only contains events
// for one customer (as the platform expects) and a slow tenant doesn't
// block the others.
type CSVUpload struct {
	cfg     CSVUploadConfig
	client  *http.Client
	url     string
	mu      sync.Mutex
	buffers map[string]*csvTenantBuf // keyed by tenant id
}

// csvTenantBuf is the per-tenant accumulator. Synchronizing on the parent
// driver's mu around access keeps the implementation simple at the cost
// of contention — at the BatchSize cadence (every ~100 events) this is
// far from the hot path.
type csvTenantBuf struct {
	rows    int
	body    bytes.Buffer
	lastEvt *generator.Event
}

// NewCSVUpload constructs the CSV bulk-upload driver.
func NewCSVUpload(cfg CSVUploadConfig) (*CSVUpload, error) {
	cfg.applyDefaults()
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	base, err := cfg.Target.URL(aforo.ServiceUsageIngestor)
	if err != nil {
		return nil, fmt.Errorf("csv_upload: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	url := base + "/v1/ingest/upload"
	return &CSVUpload{
		cfg:     cfg,
		client:  cfg.HTTPClient,
		url:     url,
		buffers: map[string]*csvTenantBuf{},
	}, nil
}

// Name reports the driver identifier.
func (d *CSVUpload) Name() string { return "csv_upload" }

// Submit appends one event to the per-tenant buffer. When the buffer
// reaches BatchSize, the flush fires synchronously and the upload's
// Result is returned.
func (d *CSVUpload) Submit(ctx context.Context, e *generator.Event) Result {
	d.mu.Lock()
	tid := e.Envelope.TenantID
	buf := d.buffers[tid]
	if buf == nil {
		buf = &csvTenantBuf{}
		// Header row — written once per buffer.
		buf.body.WriteString("event_id,event_timestamp,tenant_id,customer_id,subscription_id,product_type,metric_id,quantity\n")
		d.buffers[tid] = buf
	}
	d.appendRow(buf, e)
	buf.rows++
	buf.lastEvt = e
	if buf.rows < d.cfg.BatchSize {
		d.mu.Unlock()
		// Buffered — return a synthetic 202-equivalent so the runner records
		// it as success and counts byte deltas. Latency is near-zero for
		// the buffered case (we measure flush latency at upload time).
		return Result{
			Event:     e,
			Status:    202,
			BytesSent: 0,
		}
	}
	// Take ownership of the buffer to flush; replace with a fresh one so
	// the next Submit isn't blocked behind us.
	flushBuf := buf
	delete(d.buffers, tid)
	d.mu.Unlock()
	return d.flush(ctx, e, flushBuf)
}

// appendRow writes one CSV row from the event. Caller must hold d.mu.
func (d *CSVUpload) appendRow(buf *csvTenantBuf, e *generator.Event) {
	// quantity defaults to 1 for every event — the platform uses the
	// CSV row count as the base counter; per-event quantity is optional.
	const quantity = 1
	row := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%d\n",
		csvEscape(e.Envelope.EventID),
		e.Envelope.EventTimestamp.Format("2006-01-02T15:04:05Z07:00"),
		csvEscape(e.Envelope.TenantID),
		csvEscape(e.Envelope.CustomerID),
		csvEscape(e.Envelope.SubscriptionID),
		csvEscape(e.Envelope.ProductType),
		csvEscape(e.Envelope.MetricID),
		quantity,
	)
	buf.body.WriteString(row)
}

// Flush forces all buffered tenants to upload. Used at end-of-run by the
// runner to drain the in-memory queue. Returns the number of CSV uploads
// fired and the count of any failures.
func (d *CSVUpload) Flush(ctx context.Context) (uploads, failures int) {
	d.mu.Lock()
	pending := d.buffers
	d.buffers = map[string]*csvTenantBuf{}
	d.mu.Unlock()
	for _, buf := range pending {
		if buf.lastEvt == nil {
			continue
		}
		res := d.flush(ctx, buf.lastEvt, buf)
		uploads++
		if !res.IsSuccess() {
			failures++
		}
	}
	return uploads, failures
}

// flush builds the multipart body and POSTs the upload.
func (d *CSVUpload) flush(ctx context.Context, e *generator.Event, buf *csvTenantBuf) Result {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// `file` part — the CSV bytes themselves with the standard MIME type.
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{
		`form-data; name="file"; filename="aforo-loadgen-` + e.Envelope.TenantID + `.csv"`,
	}
	header["Content-Type"] = []string{"text/csv"}
	part, err := writer.CreatePart(header)
	if err != nil {
		return Result{Event: e, TransportErr: fmt.Errorf("csv part: %w", err)}
	}
	if _, err := io.Copy(part, &buf.body); err != nil {
		return Result{Event: e, TransportErr: fmt.Errorf("csv copy: %w", err)}
	}

	// Optional default metric name and default customer id form fields.
	_ = writer.WriteField("defaultMetricName", e.Envelope.MetricID)
	_ = writer.WriteField("defaultCustomerId", e.Envelope.CustomerID)
	_ = writer.WriteField("rowCount", strconv.Itoa(buf.rows))
	if err := writer.Close(); err != nil {
		return Result{Event: e, TransportErr: fmt.Errorf("csv writer close: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, &body)
	if err != nil {
		return Result{Event: e, TransportErr: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	applyAuthHeaders(req, e, d.cfg.AdminToken)
	applyTenantHeaders(req, e)
	applyTraceHeaders(req, e)
	req.Header.Set("User-Agent", "aforo-loadgen-csv/1.0")
	return doHTTPRequest(d.client, req, e, body.Len())
}

// Close flushes any remaining buffers and releases idle connections.
func (d *CSVUpload) Close() error {
	// Best-effort flush at close — runner usually calls Flush explicitly first.
	d.Flush(context.Background())
	closeIdle(d.client)
	return nil
}

// csvEscape returns s with embedded commas, quotes, or newlines escaped per
// RFC 4180. The platform's CSV parser is the standard Spring Boot one which
// honors RFC 4180 if Content-Type=text/csv.
func csvEscape(s string) string {
	if s == "" {
		return ""
	}
	needs := false
	for _, r := range s {
		if r == ',' || r == '"' || r == '\n' || r == '\r' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' {
			out = append(out, '"', '"')
			continue
		}
		out = append(out, []byte(string(r))...)
	}
	out = append(out, '"')
	return string(out)
}
