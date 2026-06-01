package driver

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCSVUpload_BatchesAccumulateAndFlush verifies the per-tenant buffer
// semantics: events accumulate up to BatchSize and the driver fires a
// single multipart upload when the threshold is hit.
func TestCSVUpload_BatchesAccumulateAndFlush(t *testing.T) {
	var uploads atomic.Int64
	var capturedRows atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		// Parse multipart form, count CSV body rows.
		mediaType := r.Header.Get("Content-Type")
		if !strings.HasPrefix(mediaType, "multipart/form-data") {
			t.Errorf("unexpected Content-Type: %q", mediaType)
		}
		_, params := parseContentType(mediaType)
		boundary := params["boundary"]
		if boundary == "" {
			t.Fatal("missing multipart boundary")
		}
		mr := multipart.NewReader(r.Body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("multipart read: %v", err)
			}
			if part.FormName() == "file" {
				body, _ := io.ReadAll(part)
				rows := bytes.Count(body, []byte("\n"))
				// First line is the CSV header.
				capturedRows.Add(int64(rows - 1))
			}
			_ = part.Close()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	const batchSize = 5
	d, err := NewCSVUpload(CSVUploadConfig{
		HTTPBaseConfig: HTTPBaseConfig{Target: targetForServer(srv)},
		BatchSize:      batchSize,
	})
	if err != nil {
		t.Fatalf("NewCSVUpload: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Submit batchSize events for the same tenant — should fire exactly
	// one upload.
	for i := 0; i < batchSize; i++ {
		res := d.Submit(context.Background(), newTestEvent())
		if !res.IsSuccess() {
			t.Fatalf("submit %d: status=%d err=%v", i, res.Status, res.TransportErr)
		}
	}
	if got := uploads.Load(); got != 1 {
		t.Errorf("uploads: got %d want 1 after %d events", got, batchSize)
	}
	if got := capturedRows.Load(); got != int64(batchSize) {
		t.Errorf("captured rows: got %d want %d", got, batchSize)
	}

	// One more event without flushing — buffered, no upload.
	d.Submit(context.Background(), newTestEvent())
	if got := uploads.Load(); got != 1 {
		t.Errorf("buffered submit triggered upload: %d", got)
	}

	// Explicit Flush drains the partial buffer.
	flushed, fails := d.Flush(context.Background())
	if flushed != 1 || fails != 0 {
		t.Errorf("Flush: got (%d, %d) want (1, 0)", flushed, fails)
	}
	if got := uploads.Load(); got != 2 {
		t.Errorf("uploads after flush: got %d want 2", got)
	}
}

// parseContentType is a tiny stand-in for mime.ParseMediaType, kept inline
// to avoid the extra import path in test files.
func parseContentType(s string) (string, map[string]string) {
	parts := strings.Split(s, ";")
	mt := strings.TrimSpace(parts[0])
	params := map[string]string{}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if eq := strings.IndexByte(p, '='); eq > 0 {
			params[p[:eq]] = strings.Trim(p[eq+1:], `"`)
		}
	}
	return mt, params
}

// TestCSVUpload_PerTenantIsolation ensures one slow tenant doesn't block
// the other from flushing.
func TestCSVUpload_PerTenantIsolation(t *testing.T) {
	var uploads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploads.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	d, err := NewCSVUpload(CSVUploadConfig{
		HTTPBaseConfig: HTTPBaseConfig{Target: targetForServer(srv)},
		BatchSize:      3,
	})
	if err != nil {
		t.Fatalf("NewCSVUpload: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Tenant A: 3 events → 1 upload
	for i := 0; i < 3; i++ {
		e := newTestEvent()
		e.TenantID = "tenant-A"
		d.Submit(context.Background(), e)
	}
	// Tenant B: 3 events → 1 upload (independent)
	for i := 0; i < 3; i++ {
		e := newTestEvent()
		e.TenantID = "tenant-B"
		d.Submit(context.Background(), e)
	}
	if got := uploads.Load(); got != 2 {
		t.Errorf("expected 2 uploads (one per tenant), got %d", got)
	}
}
