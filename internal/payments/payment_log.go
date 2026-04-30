package payments

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PaymentRecord is one row in payments.jsonl. Stable JSON shape — the
// validator unmarshals these directly. Fields ADD only; never rename.
type PaymentRecord struct {
	Timestamp       time.Time `json:"ts"`
	InvoiceID       string    `json:"invoice_id"`
	TenantID        string    `json:"tenant_id"`
	CustomerID      string    `json:"customer_id,omitempty"`
	SubscriptionID  string    `json:"subscription_id,omitempty"`
	AmountUSD       float64   `json:"amount_usd"`
	Currency        string    `json:"currency"`
	StripeIntentID  string    `json:"stripe_intent_id,omitempty"`
	StripeChargeID  string    `json:"stripe_charge_id,omitempty"`
	Outcome         string    `json:"outcome"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	FailureCode     string    `json:"failure_code,omitempty"`
	Note            string    `json:"note,omitempty"`
}

// PaymentLog is a thread-safe append-only writer. One record per line.
type PaymentLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	count  int64
}

// NewPaymentLog opens / creates path for append. Caller MUST Close.
func NewPaymentLog(path string) (*PaymentLog, error) {
	if path == "" {
		return nil, fmt.Errorf("payments: empty payment log path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("payments: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304 — caller path
	if err != nil {
		return nil, fmt.Errorf("payments: open %s: %w", path, err)
	}
	return &PaymentLog{w: f, closer: f}, nil
}

// NewPaymentLogTo wraps an arbitrary writer. Used by tests.
func NewPaymentLogTo(w io.Writer) *PaymentLog { return &PaymentLog{w: w} }

// Append writes one record + newline.
func (p *PaymentLog) Append(rec PaymentRecord) error {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("payments: marshal: %w", err)
	}
	buf = append(buf, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.w.Write(buf); err != nil {
		return fmt.Errorf("payments: write: %w", err)
	}
	p.count++
	return nil
}

// Count returns appended-record count.
func (p *PaymentLog) Count() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// Close flushes the underlying file. Idempotent.
func (p *PaymentLog) Close() error {
	if p.closer == nil {
		return nil
	}
	err := p.closer.Close()
	p.closer = nil
	return err
}

// Load reads payments.jsonl from a run output dir. Returns an empty slice if
// the file doesn't exist (run had no payment driver).
func Load(dir string) ([]PaymentRecord, error) {
	path := filepath.Join(dir, "payments.jsonl")
	f, err := os.Open(path) // #nosec G304 — caller path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("payments: open %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	out := []PaymentRecord{}
	for {
		var rec PaymentRecord
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("payments: parse %s: %w", path, err)
		}
		out = append(out, rec)
	}
	return out, nil
}
