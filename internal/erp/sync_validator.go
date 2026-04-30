package erp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// SyncValidator polls the platform's erp_sync_log per invoice and asserts
// that every issued invoice landed at its tenant's configured ERP within
// the SLA. Optionally verifies the provider sandbox round-trip via the
// per-provider Provider.Verify call.
//
// The validator's polling cadence is exponential backoff up to a max
// (sla * 2 — acceptance criterion: "ERP sync validation polls with backoff,
// max wait = scenario.erp.sync_sla_seconds × 2"). Each invoice gets its
// own goroutine so a slow provider doesn't block faster ones.
//
// Concurrency: safe; the validator's state is per-invoice channels.
type SyncValidator struct {
	client         *lifecycle.Client
	providers      map[string]Provider
	slaSeconds     int
	verifyExternal bool
	pollInitial    time.Duration
	pollMax        time.Duration
	logFile        *SyncLog
}

// SyncValidatorConfig configures the validator.
type SyncValidatorConfig struct {
	Client         *lifecycle.Client
	Providers      map[string]Provider
	SLASeconds     int
	VerifyExternal bool
	OutputDir      string
	PollInitial    time.Duration
	PollMax        time.Duration
}

// NewSyncValidator constructs and validates the config.
func NewSyncValidator(cfg SyncValidatorConfig) (*SyncValidator, error) {
	if cfg.Client == nil {
		return nil, errors.New("erp: sync validator requires lifecycle client")
	}
	if cfg.SLASeconds <= 0 {
		return nil, errors.New("erp: sla_seconds must be > 0")
	}
	if cfg.PollInitial <= 0 {
		cfg.PollInitial = 1 * time.Second
	}
	if cfg.PollMax <= 0 {
		cfg.PollMax = time.Duration(cfg.SLASeconds*2) * time.Second
	}
	logPath := filepath.Join(cfg.OutputDir, "erp_sync.jsonl")
	sl, err := NewSyncLog(logPath)
	if err != nil {
		return nil, err
	}
	return &SyncValidator{
		client:         cfg.Client,
		providers:      cfg.Providers,
		slaSeconds:     cfg.SLASeconds,
		verifyExternal: cfg.VerifyExternal,
		pollInitial:    cfg.PollInitial,
		pollMax:        cfg.PollMax,
		logFile:        sl,
	}, nil
}

// Close flushes the sync log.
func (v *SyncValidator) Close() error { return v.logFile.Close() }

// VerifyOne polls the platform's erp_sync_log for invoiceID, then optionally
// verifies the round-trip with the provider sandbox. Returns the populated
// SyncRecord; logs to erp_sync.jsonl unconditionally.
func (v *SyncValidator) VerifyOne(ctx context.Context, tenantID, invoiceID, provider string) SyncRecord {
	rec := SyncRecord{
		Timestamp: time.Now().UTC(),
		InvoiceID: invoiceID,
		TenantID:  tenantID,
		Provider:  provider,
		Status:    "pending",
	}
	deadline := time.Now().Add(time.Duration(v.slaSeconds) * time.Second)
	wait := v.pollInitial
	startedAt := time.Now()
	for {
		extID, status, attempts, err := v.fetchSyncLog(ctx, tenantID, invoiceID)
		if err == nil {
			rec.ExternalID = extID
			rec.Attempts = attempts
			if status == "synced" || status == "completed" || status == "success" {
				rec.LatencySeconds = time.Since(startedAt).Seconds()
				rec.Status = "synced"
				if v.verifyExternal {
					rec.Verified, rec.VerifyReason = v.runVerify(ctx, provider, extID)
				}
				_ = v.logFile.Append(rec)
				return rec
			}
			if status == "failed" {
				rec.Status = "failed"
				rec.Note = "platform reports failed sync"
				_ = v.logFile.Append(rec)
				return rec
			}
		} else {
			rec.Note = err.Error()
		}
		if time.Now().After(deadline) {
			rec.Status = "missing"
			rec.Note = fmt.Sprintf("no synced entry within %ds", v.slaSeconds)
			rec.LatencySeconds = time.Since(startedAt).Seconds()
			_ = v.logFile.Append(rec)
			return rec
		}
		select {
		case <-ctx.Done():
			rec.Status = "missing"
			rec.Note = ctx.Err().Error()
			_ = v.logFile.Append(rec)
			return rec
		case <-time.After(wait):
		}
		wait *= 2
		if wait > v.pollMax {
			wait = v.pollMax
		}
	}
}

// VerifyAll fans out one goroutine per (invoice, provider) pair. Returns
// every SyncRecord; the caller aggregates pass/fail.
func (v *SyncValidator) VerifyAll(ctx context.Context, items []VerifyItem, parallelism int) []SyncRecord {
	if parallelism <= 0 {
		parallelism = 8
	}
	jobs := make(chan VerifyItem, len(items))
	for _, it := range items {
		jobs <- it
	}
	close(jobs)
	out := make([]SyncRecord, 0, len(items))
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	for w := 0; w < parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if ctx.Err() != nil {
					return
				}
				rec := v.VerifyOne(ctx, it.TenantID, it.InvoiceID, it.Provider)
				mu.Lock()
				out = append(out, rec)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].InvoiceID < out[j].InvoiceID })
	return out
}

// VerifyItem is one (tenant, invoice, expected provider) tuple.
type VerifyItem struct {
	TenantID  string
	InvoiceID string
	Provider  string
}

// fetchSyncLog GETs /api/v1/erp-integrations/sync-log?invoice_id=X and
// returns the most recent entry's external id + status. Returns "" status
// when the list is empty (still pending).
func (v *SyncValidator) fetchSyncLog(ctx context.Context, tenantID, invoiceID string) (extID, status string, attempts int, err error) {
	path := "/api/v1/erp-integrations/sync-log?invoice_id=" + invoiceID
	var resp struct {
		Data []struct {
			InvoiceID     string `json:"invoice_id"`
			ExternalDocID string `json:"external_document_id"`
			Status        string `json:"status"`
			Attempts      int    `json:"attempts"`
		} `json:"data"`
	}
	_, err = v.client.GetJSON(ctx, aforo.ServiceBilling, path, tenantID, &resp)
	if err != nil {
		return "", "", 0, err
	}
	if len(resp.Data) == 0 {
		return "", "", 0, nil
	}
	last := resp.Data[len(resp.Data)-1]
	return last.ExternalDocID, last.Status, last.Attempts, nil
}

// runVerify is the per-provider sandbox round-trip. Returns ok + reason.
func (v *SyncValidator) runVerify(ctx context.Context, providerName, externalID string) (bool, string) {
	if v.providers == nil {
		return false, "no providers wired"
	}
	p, ok := v.providers[providerName]
	if !ok {
		return false, "no provider " + providerName
	}
	ok, reason, err := p.Verify(ctx, externalID)
	if err != nil {
		return false, "verify err: " + err.Error()
	}
	return ok, reason
}

// SyncLog is the append-only writer for erp_sync.jsonl.
type SyncLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	count  int64
}

// NewSyncLog opens / creates the file.
func NewSyncLog(path string) (*SyncLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304
	if err != nil {
		return nil, err
	}
	return &SyncLog{w: f, closer: f}, nil
}

// NewSyncLogTo wraps an io.Writer.
func NewSyncLogTo(w io.Writer) *SyncLog { return &SyncLog{w: w} }

// Append writes one record + newline.
func (s *SyncLog) Append(r SyncRecord) error {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	buf, err := json.Marshal(r)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(buf)
	if err == nil {
		s.count++
	}
	return err
}

// Close flushes the writer.
func (s *SyncLog) Close() error {
	if s.closer == nil {
		return nil
	}
	err := s.closer.Close()
	s.closer = nil
	return err
}

// Count returns number of records.
func (s *SyncLog) Count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// LoadSyncLog reads erp_sync.jsonl.
func LoadSyncLog(dir string) ([]SyncRecord, error) {
	path := filepath.Join(dir, "erp_sync.jsonl")
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	out := []SyncRecord{}
	for {
		var rec SyncRecord
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
