package payments

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// Driver is the per-run payment orchestrator.
//
// Inputs: a list of issued invoices (id, sub, customer, amount, currency)
// from the bill-run output. For each invoice it:
//
//   1. Picks an outcome from the OutcomePicker (success / decline / insuf).
//   2. Picks the matching Stripe test card.
//   3. Mints an Idempotency-Key.
//   4. POSTs /api/v1/invoices/{id}/payment-link OR /api/v1/payment-methods/{...}/charge
//      depending on platform readiness — the loadgen prefers a single
//      "drive payment" endpoint exposed as /api/v1/internal/payments/drive
//      when available; falls back to recording a synthetic payment intent
//      in the report otherwise (CI offline).
//   5. Records the result in PaymentLog.
//   6. On decline → DunningDriver.Walk runs in a separate goroutine.
//
// Concurrency: safe for concurrent invocation. Driver.ProcessInvoices fans
// out one goroutine per invoice up to the configured worker pool.
type Driver struct {
	stripe       *StripeClient
	picker       *OutcomePicker
	dunning      *DunningDriver
	client       *lifecycle.Client
	transitions  *lifecycle.TransitionLog
	logFile      *PaymentLog
	workers      int
	idemPrefix   string

	// dunningWG tracks fire-and-forget goroutines spawned per decline so
	// the orchestrator can WaitDunning() before closing the transition
	// log. Without this, dunning rows can race the log close and silently
	// drop.
	dunningWG sync.WaitGroup

	processed atomic.Int64
	succeeded atomic.Int64
	declined  atomic.Int64
	insuf     atomic.Int64
	errors    atomic.Int64
}

// DriverConfig configures Driver. Workers default to 16.
type DriverConfig struct {
	Stripe       *StripeClient
	Picker       *OutcomePicker
	Dunning      *DunningDriver
	Client       *lifecycle.Client
	Transitions  *lifecycle.TransitionLog
	OutputDir    string  // where payments.jsonl is written
	Workers      int
	IdemPrefix   string
}

// NewDriver constructs a Driver. Validates required fields. Opens the
// payments.jsonl file inside cfg.OutputDir.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.Stripe == nil {
		return nil, errors.New("payments: driver requires a stripe client")
	}
	if cfg.Picker == nil {
		return nil, errors.New("payments: driver requires an outcome picker")
	}
	if cfg.Client == nil {
		return nil, errors.New("payments: driver requires a lifecycle client (HTTP)")
	}
	if cfg.Transitions == nil {
		return nil, errors.New("payments: driver requires a transition log")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	if cfg.IdemPrefix == "" {
		cfg.IdemPrefix = "aforo-loadgen"
	}
	logPath := filepath.Join(cfg.OutputDir, "payments.jsonl")
	pl, err := NewPaymentLog(logPath)
	if err != nil {
		return nil, err
	}
	return &Driver{
		stripe:      cfg.Stripe,
		picker:      cfg.Picker,
		dunning:     cfg.Dunning,
		client:      cfg.Client,
		transitions: cfg.Transitions,
		logFile:     pl,
		workers:     cfg.Workers,
		idemPrefix:  cfg.IdemPrefix,
	}, nil
}

// Close flushes payments.jsonl. Call once after ProcessInvoices returns.
func (d *Driver) Close() error { return d.logFile.Close() }

// Invoice is the minimal shape the driver needs.
type Invoice struct {
	InvoiceID      string
	TenantID       string
	CustomerID     string
	SubscriptionID string
	AmountUSD      float64
	Currency       string
}

// ProcessInvoices fans out the worker pool and processes every invoice in
// inv. Blocks until all complete or ctx is cancelled.
func (d *Driver) ProcessInvoices(ctx context.Context, invoices []Invoice) error {
	if len(invoices) == 0 {
		return nil
	}
	jobs := make(chan Invoice, len(invoices))
	for _, inv := range invoices {
		jobs <- inv
	}
	close(jobs)

	wg := sync.WaitGroup{}
	for w := 0; w < d.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for inv := range jobs {
				if ctx.Err() != nil {
					return
				}
				d.processOne(ctx, inv)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// processOne is the per-invoice pipeline.
func (d *Driver) processOne(ctx context.Context, inv Invoice) {
	d.processed.Add(1)
	want := d.picker.Pick(inv.InvoiceID)
	card, err := CardFor(want)
	if err != nil {
		d.errors.Add(1)
		_ = d.logFile.Append(PaymentRecord{
			Timestamp:  time.Now().UTC(),
			InvoiceID:  inv.InvoiceID,
			TenantID:   inv.TenantID,
			CustomerID: inv.CustomerID,
			Outcome:    string(OutcomeError),
			Note:       err.Error(),
		})
		return
	}
	idem := fmt.Sprintf("%s-pay-%s", d.idemPrefix, inv.InvoiceID)

	chg, err := d.stripe.CreatePaymentIntent(ctx, inv.AmountUSD, inv.Currency, card, idem, inv.CustomerID)
	if err != nil {
		d.errors.Add(1)
		_ = d.logFile.Append(PaymentRecord{
			Timestamp:  time.Now().UTC(),
			InvoiceID:  inv.InvoiceID,
			TenantID:   inv.TenantID,
			CustomerID: inv.CustomerID,
			Outcome:    string(OutcomeError),
			Note:       err.Error(),
		})
		return
	}

	// Record the platform-side mark. The platform's actual charge happens
	// inside its own /payments/intent endpoint; we drive the same
	// idempotency key so the platform recognises the request as a single
	// logical attempt and we can compare ids.
	_ = d.driverPlatformPayment(ctx, inv, chg)

	d.tally(chg.Outcome)
	_ = d.logFile.Append(PaymentRecord{
		Timestamp:       time.Now().UTC(),
		InvoiceID:       inv.InvoiceID,
		TenantID:        inv.TenantID,
		CustomerID:      inv.CustomerID,
		SubscriptionID:  inv.SubscriptionID,
		AmountUSD:       inv.AmountUSD,
		Currency:        inv.Currency,
		StripeIntentID:  chg.PaymentIntentID,
		StripeChargeID:  chg.ChargeID,
		Outcome:         string(chg.Outcome),
		IdempotencyKey:  idem,
		FailureCode:     chg.FailureCode,
	})

	// Decline → drive dunning to its terminal state. The dunning sequence
	// is long (max_attempts × retry_interval) and we don't block the
	// payment worker on it. WaitDunning() drains the goroutines before the
	// orchestrator closes the transition log.
	if chg.Outcome == OutcomeDeclined || chg.Outcome == OutcomeInsufficientFunds {
		if d.dunning != nil && inv.SubscriptionID != "" {
			d.dunningWG.Add(1)
			go func(inv Invoice) {
				defer d.dunningWG.Done()
				dunctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				_, _ = d.dunning.Walk(dunctx, inv.TenantID, inv.SubscriptionID, inv.InvoiceID)
			}(inv)
		}
	}
}

// WaitDunning blocks until every in-flight dunning walk has returned. The
// orchestrator MUST call this before closing the transition log so dunning
// rows don't race with file close.
func (d *Driver) WaitDunning() {
	d.dunningWG.Wait()
}

// driverPlatformPayment posts to the platform's drive-payment endpoint —
// best-effort. The platform doesn't always expose a "drive" endpoint
// directly; we try a /api/v1/invoices/{id}/charge first and fall back to
// /api/v1/internal/payments/drive. If neither exists, we record a
// transition row and move on. This keeps the driver useful in CI even
// when the platform's own payment flow isn't deployed.
func (d *Driver) driverPlatformPayment(ctx context.Context, inv Invoice, chg *Charge) error {
	body := map[string]any{
		"invoice_id":         inv.InvoiceID,
		"customer_id":        inv.CustomerID,
		"subscription_id":    inv.SubscriptionID,
		"amount_usd":         inv.AmountUSD,
		"currency":           inv.Currency,
		"stripe_intent_id":   chg.PaymentIntentID,
		"stripe_charge_id":   chg.ChargeID,
		"outcome":            chg.Outcome,
		"idempotency_key":    chg.IdempotencyKey,
	}
	idem := chg.IdempotencyKey
	candidates := []string{
		fmt.Sprintf("/api/v1/invoices/%s/charge", inv.InvoiceID),
		"/api/v1/internal/payments/drive",
	}
	var lastErr error
	for _, path := range candidates {
		var resp map[string]any
		status, err := d.client.PostJSON(ctx, aforo.ServiceBilling, path, inv.TenantID, idem, body, &resp)
		if err == nil {
			_ = d.transitions.Append(lifecycle.TransitionRecord{
				Timestamp:        time.Now().UTC(),
				TenantID:         inv.TenantID,
				SubscriptionID:   inv.SubscriptionID,
				Transition:       lifecycle.TransitionRetryPayment,
				IdempotencyKey:   idem,
				HTTPStatus:       status,
				TransitionStatus: lifecycle.StatusOK,
			})
			return nil
		}
		// 404 means the platform didn't ship that endpoint — try the next.
		if status == 404 {
			lastErr = err
			continue
		}
		// Anything else is a real error: record but don't retry on the next path.
		_ = d.transitions.Append(lifecycle.TransitionRecord{
			Timestamp:        time.Now().UTC(),
			TenantID:         inv.TenantID,
			SubscriptionID:   inv.SubscriptionID,
			Transition:       lifecycle.TransitionRetryPayment,
			IdempotencyKey:   idem,
			HTTPStatus:       status,
			TransitionStatus: lifecycle.StatusFail,
			Error:            sanitize(err.Error()),
		})
		return err
	}
	if lastErr != nil {
		_ = d.transitions.Append(lifecycle.TransitionRecord{
			Timestamp:        time.Now().UTC(),
			TenantID:         inv.TenantID,
			SubscriptionID:   inv.SubscriptionID,
			Transition:       lifecycle.TransitionRetryPayment,
			IdempotencyKey:   idem,
			HTTPStatus:       404,
			TransitionStatus: lifecycle.StatusSkipped,
			Error:            "no platform payment-drive endpoint found",
		})
	}
	return nil
}

// Stats is the driver's snapshot of how many invoices it processed and how
// they classified.
type Stats struct {
	Processed int64
	Succeeded int64
	Declined  int64
	Insuf     int64
	Errors    int64
}

// Stats returns a snapshot.
func (d *Driver) Stats() Stats {
	return Stats{
		Processed: d.processed.Load(),
		Succeeded: d.succeeded.Load(),
		Declined:  d.declined.Load(),
		Insuf:     d.insuf.Load(),
		Errors:    d.errors.Load(),
	}
}

func (d *Driver) tally(outcome PaymentOutcome) {
	switch outcome {
	case OutcomeSucceeded:
		d.succeeded.Add(1)
	case OutcomeDeclined:
		d.declined.Add(1)
	case OutcomeInsufficientFunds:
		d.insuf.Add(1)
	case OutcomeError:
		d.errors.Add(1)
	}
}

func sanitize(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}
