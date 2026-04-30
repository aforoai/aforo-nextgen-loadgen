// Package creditnotes drives credit-note + refund flows for the load test.
//
// The platform's CreditNoteService owns the state machine
// (DRAFT → ISSUED → VOID) and the apply-to-invoice flow. This driver
// fires the operator-side workflow: pick the configured share of PAID
// invoices, draft a credit note (full or partial), issue it, optionally
// apply it back to the invoice, and record the trail in
// credit_notes.jsonl for Check 16.
//
// Determinism is anchored on scenario.seed — a re-run of the same scenario
// picks the same invoices for refund / partial / no-op.
package creditnotes

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Driver is the orchestrator. Construct, ProcessInvoices, Close.
type Driver struct {
	client      *lifecycle.Client
	transitions *lifecycle.TransitionLog
	logFile     *Log
	mix         scenario.CreditNotes
	rng         *rand.Rand
	rngMu       sync.Mutex

	processed atomic.Int64
	drafted   atomic.Int64
	issued    atomic.Int64
	applied   atomic.Int64
	skipped   atomic.Int64
	errors    atomic.Int64
}

// DriverConfig configures Driver. ApplyToInvoicePct defaults to 1.0,
// PartialAmountPct to 0.5.
type DriverConfig struct {
	Client      *lifecycle.Client
	Transitions *lifecycle.TransitionLog
	OutputDir   string
	Mix         scenario.CreditNotes
	Seed        int64
}

// NewDriver constructs the driver. Validates config; opens
// credit_notes.jsonl for append.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("credit_notes: client required")
	}
	if cfg.Transitions == nil {
		return nil, errors.New("credit_notes: transition log required")
	}
	// Driver still constructs even when cfg.Mix.Enabled is false (caller
	// may toggle at runtime); Process becomes a no-op in that case.
	if cfg.Mix.PartialAmountPct <= 0 {
		cfg.Mix.PartialAmountPct = 0.5
	}
	if cfg.Mix.ApplyToInvoicePct <= 0 {
		cfg.Mix.ApplyToInvoicePct = 1.0
	}
	if cfg.Mix.Reason == "" {
		cfg.Mix.Reason = "PRORATION"
	}
	logPath := filepath.Join(cfg.OutputDir, "credit_notes.jsonl")
	lf, err := NewLog(logPath)
	if err != nil {
		return nil, err
	}
	return &Driver{
		client:      cfg.Client,
		transitions: cfg.Transitions,
		logFile:     lf,
		mix:         cfg.Mix,
		rng:         rand.New(rand.NewSource(cfg.Seed)),
	}, nil
}

// Close flushes the credit_notes log.
func (d *Driver) Close() error { return d.logFile.Close() }

// Invoice is the minimal shape needed to drive a refund.
type Invoice struct {
	InvoiceID  string
	TenantID   string
	CustomerID string
	AmountUSD  float64
	Currency   string
}

// ProcessInvoices iterates the input invoices and draws each into one of:
//
//	full refund     — credit note for entire amount, mark invoice fully credited
//	partial refund  — credit note for PartialAmountPct of amount
//	no-op           — invoice unchanged
//
// All of refund_pct + partial_pct + no-op weighting must already be
// validated to sum to ≤ 1 — the no-op weight is the residual.
//
// Concurrency: ProcessInvoices itself is sequential (refund volumes are
// small relative to event traffic). Per-invoice apply runs synchronously
// so the validator sees the full DRAFT → ISSUED → applied progression.
func (d *Driver) ProcessInvoices(ctx context.Context, invoices []Invoice) error {
	if !d.mix.Enabled {
		return nil
	}
	for _, inv := range invoices {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.processed.Add(1)
		kind := d.pickKind()
		if kind == kindNone {
			d.skipped.Add(1)
			continue
		}
		amount := inv.AmountUSD
		if kind == kindPartial {
			amount = roundCents(inv.AmountUSD * d.mix.PartialAmountPct)
		}
		applyToInvoice := d.flip(d.mix.ApplyToInvoicePct)
		if err := d.runFlow(ctx, inv, amount, kind, applyToInvoice); err != nil {
			d.errors.Add(1)
			_ = d.logFile.Append(Record{
				Timestamp: time.Now().UTC(),
				InvoiceID: inv.InvoiceID, TenantID: inv.TenantID, CustomerID: inv.CustomerID,
				AmountUSD: amount, Kind: string(kind),
				ApplyToInvoice: applyToInvoice, Status: "error", Note: err.Error(),
			})
		}
	}
	return nil
}

type kind string

const (
	kindFull    kind = "FULL"
	kindPartial kind = "PARTIAL"
	kindNone    kind = "NONE"
)

// pickKind weights the three kinds. RNG is seed-deterministic.
func (d *Driver) pickKind() kind {
	d.rngMu.Lock()
	defer d.rngMu.Unlock()
	r := d.rng.Float64()
	if r < d.mix.RefundPct {
		return kindFull
	}
	if r < d.mix.RefundPct+d.mix.PartialPct {
		return kindPartial
	}
	return kindNone
}

// flip is a seeded coin flip with probability p.
func (d *Driver) flip(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	d.rngMu.Lock()
	defer d.rngMu.Unlock()
	return d.rng.Float64() < p
}

// runFlow drafts → issues → optionally applies a credit note.
func (d *Driver) runFlow(ctx context.Context, inv Invoice, amount float64, kind kind, applyToInvoice bool) error {
	idem := fmt.Sprintf("aforo-loadgen-cn-%s-%s", kind, inv.InvoiceID)

	// 1) DRAFT
	draftBody := map[string]any{
		"invoice_id":  inv.InvoiceID,
		"customer_id": inv.CustomerID,
		"amount_usd":  amount,
		"currency":    inv.Currency,
		"reason":      d.mix.Reason,
	}
	var draftResp map[string]any
	status, err := d.client.PostJSON(ctx, aforo.ServiceBilling, "/api/v1/credit-notes", inv.TenantID, idem+"-draft", draftBody, &draftResp)
	if err != nil {
		return fmt.Errorf("draft credit note: %w (status %d)", err, status)
	}
	d.drafted.Add(1)
	creditNoteID := readString(draftResp, "id")
	if creditNoteID == "" {
		creditNoteID = readString(draftResp, "creditNoteId")
	}
	_ = d.logFile.Append(Record{
		Timestamp: time.Now().UTC(),
		InvoiceID: inv.InvoiceID, TenantID: inv.TenantID, CustomerID: inv.CustomerID,
		CreditNoteID: creditNoteID, AmountUSD: amount, Kind: string(kind),
		ApplyToInvoice: applyToInvoice, Status: "DRAFT",
	})

	// 2) ISSUE
	if creditNoteID == "" {
		// platform may not have returned an id (e.g. older shape) — best-effort,
		// record DRAFT and exit.
		return nil
	}
	issuePath := fmt.Sprintf("/api/v1/credit-notes/%s/issue", creditNoteID)
	var issueResp map[string]any
	status, err = d.client.PostJSON(ctx, aforo.ServiceBilling, issuePath, inv.TenantID, idem+"-issue", struct{}{}, &issueResp)
	if err != nil {
		return fmt.Errorf("issue credit note %s: %w (status %d)", creditNoteID, err, status)
	}
	d.issued.Add(1)
	_ = d.logFile.Append(Record{
		Timestamp: time.Now().UTC(),
		InvoiceID: inv.InvoiceID, TenantID: inv.TenantID, CustomerID: inv.CustomerID,
		CreditNoteID: creditNoteID, AmountUSD: amount, Kind: string(kind),
		ApplyToInvoice: applyToInvoice, Status: "ISSUED",
	})

	// 3) APPLY (optional)
	if !applyToInvoice {
		return nil
	}
	applyPath := fmt.Sprintf("/api/v1/credit-notes/%s/apply", creditNoteID)
	applyBody := map[string]any{
		"invoice_id": inv.InvoiceID,
		"amount_usd": amount,
	}
	var applyResp map[string]any
	status, err = d.client.PostJSON(ctx, aforo.ServiceBilling, applyPath, inv.TenantID, idem+"-apply", applyBody, &applyResp)
	if err != nil {
		return fmt.Errorf("apply credit note %s: %w (status %d)", creditNoteID, err, status)
	}
	d.applied.Add(1)
	_ = d.logFile.Append(Record{
		Timestamp: time.Now().UTC(),
		InvoiceID: inv.InvoiceID, TenantID: inv.TenantID, CustomerID: inv.CustomerID,
		CreditNoteID: creditNoteID, AmountUSD: amount, Kind: string(kind),
		ApplyToInvoice: applyToInvoice, Status: "APPLIED",
	})
	return nil
}

// Stats returns a snapshot of the driver's counters.
type Stats struct {
	Processed int64
	Drafted   int64
	Issued    int64
	Applied   int64
	Skipped   int64
	Errors    int64
}

// Stats returns a snapshot.
func (d *Driver) Stats() Stats {
	return Stats{
		Processed: d.processed.Load(),
		Drafted:   d.drafted.Load(),
		Issued:    d.issued.Load(),
		Applied:   d.applied.Load(),
		Skipped:   d.skipped.Load(),
		Errors:    d.errors.Load(),
	}
}

func readString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
