package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// PathUsageIngestBatch is usage-ingestor's synchronous batch endpoint
// (UsageEventController.ingestBatch, @PostMapping "/v1/ingest/batch").
const PathUsageIngestBatch = "/v1/ingest/batch"

// RESTBatchMaxSize is the server-side cap: IngestBatchRequest.events carries
// @Size(max = 1000). A larger batch is rejected whole with 400.
const RESTBatchMaxSize = 1000

// RESTBatchConfig configures the rest_batch driver.
type RESTBatchConfig struct {
	HTTPBaseConfig
	// Size is the number of events per POST. Default 100, max 1000.
	Size int
	// MaxWait bounds how long an event waits for its batch to fill before
	// the partial batch is sent. Default 100ms.
	MaxWait time.Duration
}

// RESTBatch sends events to POST /v1/ingest/batch, one tenant per request.
//
// Each Submit blocks until the batch carrying its event has been sent and
// returns that event's own outcome, so the runner's per-event counters and
// latency histogram stay meaningful:
//
//   - 2xx: the response body ({success, data:{accepted, duplicates, failed,
//     errors:[{index, message}]}} from ApiResponseAdvice, or the bare
//     IngestBatchResponse) is read; an event whose index is in errors gets
//     Status 422 and the server's message in BodyExcerpt, every other event
//     gets the batch status (202). The server does not say per index which
//     accepted events were duplicates — only a count — so duplicates are
//     reported as success here and counted from the database or the
//     ingestor's own metrics.
//   - non-2xx or transport failure: every event in the batch gets it.
//
// Latency recorded for every event is the batch round trip.
//
// Batches are per tenant because the credential decides the tenant
// (ApiKeyAuthFilter: the key's tenant wins over X-Tenant-Id). X-Customer-Id
// is not sent, because one batch spans customers; usage-ingestor's
// per-customer limiter (CustomerRateLimitFilter) lets requests without the
// header through. The per-tenant limiter (aforo-common RateLimitFilter,
// aforo.rate-limit.max-requests-per-minute) counts one batch as one request.
//
// Events tagged with a negative path are sent alone through rest_direct so
// their deliberately bad credential or body cannot fail a whole batch.
//
// Throughput note: an event occupies one worker while it waits, so a batch
// can only fill when --workers is at least Size per concurrently filling
// tenant. Otherwise MaxWait sends partial batches.
type RESTBatch struct {
	cfg    RESTBatchConfig
	client *http.Client
	url    string
	single *RESTDirect

	mu      sync.Mutex
	pending map[string]*batchBuf // tenant id → filling batch
	closed  bool
}

type batchItem struct {
	ctx context.Context
	e   *generator.Event
	ch  chan Result
}

type batchBuf struct {
	items []batchItem
	timer *time.Timer
}

// NewRESTBatch constructs the batch driver.
func NewRESTBatch(cfg RESTBatchConfig) (*RESTBatch, error) {
	cfg.applyDefaults()
	if cfg.Size <= 0 {
		cfg.Size = 100
	}
	if cfg.Size > RESTBatchMaxSize {
		return nil, fmt.Errorf("rest_batch: size %d exceeds the server limit of %d events per batch", cfg.Size, RESTBatchMaxSize)
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 100 * time.Millisecond
	}
	url, err := cfg.Target.Path(aforo.ServiceUsageIngestor, PathUsageIngestBatch)
	if err != nil {
		return nil, fmt.Errorf("rest_batch: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	single, err := NewRESTDirect(RESTDirectConfig{
		Target:     cfg.Target,
		HTTPClient: cfg.HTTPClient,
		AdminToken: cfg.AdminToken,
	})
	if err != nil {
		return nil, err
	}
	return &RESTBatch{
		cfg:     cfg,
		client:  cfg.HTTPClient,
		url:     url,
		single:  single,
		pending: map[string]*batchBuf{},
	}, nil
}

// Name reports the driver identifier.
func (d *RESTBatch) Name() string { return "rest_batch" }

// Submit queues the event into its tenant's batch and waits for the outcome.
func (d *RESTBatch) Submit(ctx context.Context, e *generator.Event) Result {
	if e.NegativePath != "" || len(e.RawBody) > 0 {
		return d.single.Submit(ctx, e)
	}
	item := batchItem{ctx: ctx, e: e, ch: make(chan Result, 1)}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return Result{Event: e, TransportErr: fmt.Errorf("rest_batch: driver closed")}
	}
	buf := d.pending[e.TenantID]
	if buf == nil {
		buf = &batchBuf{}
		d.pending[e.TenantID] = buf
		tenant := e.TenantID
		buf.timer = time.AfterFunc(d.cfg.MaxWait, func() { d.flushTenant(tenant, buf) })
	}
	buf.items = append(buf.items, item)
	full := len(buf.items) >= d.cfg.Size
	if full {
		delete(d.pending, e.TenantID)
		buf.timer.Stop()
	}
	d.mu.Unlock()

	if full {
		d.send(buf.items)
	}
	select {
	case res := <-item.ch:
		return res
	case <-ctx.Done():
		return Result{Event: e, TransportErr: ctx.Err()}
	}
}

// flushTenant sends a partial batch when MaxWait expires. It is a no-op if
// the batch already filled and was sent.
func (d *RESTBatch) flushTenant(tenant string, buf *batchBuf) {
	d.mu.Lock()
	if d.pending[tenant] != buf {
		d.mu.Unlock()
		return
	}
	delete(d.pending, tenant)
	d.mu.Unlock()
	d.send(buf.items)
}

type batchRequestBody struct {
	Events []generator.Envelope `json:"events"`
}

type batchError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

type batchResponseBody struct {
	Accepted   int          `json:"accepted"`
	Duplicates int          `json:"duplicates"`
	Failed     int          `json:"failed"`
	Errors     []batchError `json:"errors"`
}

// send POSTs one batch and delivers a Result to every waiting Submit.
func (d *RESTBatch) send(items []batchItem) {
	if len(items) == 0 {
		return
	}
	first := items[0].e
	body := batchRequestBody{Events: make([]generator.Envelope, len(items))}
	for i, it := range items {
		body.Events[i] = it.e.Envelope
	}
	payload, err := json.Marshal(body)
	if err != nil {
		d.deliverAll(items, Result{TransportErr: fmt.Errorf("marshal batch: %w", err)})
		return
	}
	// The batch is sent under the first event's context: once the request
	// is on the wire it belongs to every event in it.
	req, err := http.NewRequestWithContext(items[0].ctx, http.MethodPost, d.url, bytes.NewReader(payload))
	if err != nil {
		d.deliverAll(items, Result{TransportErr: fmt.Errorf("build request: %w", err)})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyAuthHeaders(req, first, d.cfg.AdminToken)
	if first.TenantID != "" {
		req.Header.Set("X-Tenant-Id", first.TenantID)
	}
	req.Header.Set("X-Loadgen-Batch-Size", fmt.Sprint(len(items)))

	started := time.Now()
	resp, err := d.client.Do(req)
	if err != nil {
		d.deliverAll(items, Result{TransportErr: err, Latency: time.Since(started), BytesSent: len(payload) / len(items)})
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(started)
	perSent := len(payload) / len(items)
	perRecv := len(raw) / len(items)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.deliverAll(items, Result{Status: resp.StatusCode, Latency: latency, BytesSent: perSent, BytesRecv: perRecv, BodyExcerpt: excerpt(raw)})
		return
	}
	parsed, ok := parseBatchResponse(raw)
	failed := map[int]string{}
	if ok {
		for _, be := range parsed.Errors {
			failed[be.Index] = be.Message
		}
	}
	for i, it := range items {
		r := Result{Event: it.e, Status: resp.StatusCode, Latency: latency, BytesSent: perSent, BytesRecv: perRecv}
		if msg, bad := failed[i]; bad {
			r.Status = http.StatusUnprocessableEntity
			r.BodyExcerpt = excerpt([]byte("batch error: " + msg))
		}
		it.ch <- r
	}
}

func (d *RESTBatch) deliverAll(items []batchItem, tmpl Result) {
	for _, it := range items {
		r := tmpl
		r.Event = it.e
		it.ch <- r
	}
}

// parseBatchResponse accepts both the {success, data, meta} envelope that
// usage-ingestor's ApiResponseAdvice wraps 2xx bodies in and the bare
// IngestBatchResponse.
func parseBatchResponse(raw []byte) (batchResponseBody, bool) {
	var env struct {
		Data *batchResponseBody `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Data != nil {
		return *env.Data, true
	}
	var bare batchResponseBody
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare, true
	}
	return batchResponseBody{}, false
}

func excerpt(b []byte) string {
	if len(b) > BodyExcerptMax {
		return string(b[:BodyExcerptMax]) + "…"
	}
	return string(b)
}

// Close sends every partial batch and releases idle connections.
func (d *RESTBatch) Close() error {
	d.mu.Lock()
	d.closed = true
	pending := d.pending
	d.pending = map[string]*batchBuf{}
	d.mu.Unlock()
	for _, buf := range pending {
		buf.timer.Stop()
		d.send(buf.items)
	}
	closeIdle(d.client)
	return d.single.Close()
}
