package creditnotes

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one row of credit_notes.jsonl. Multiple rows per credit note —
// one per state transition (DRAFT, ISSUED, APPLIED) so the validator can
// reconstruct the full progression.
type Record struct {
	Timestamp      time.Time `json:"ts"`
	InvoiceID      string    `json:"invoice_id"`
	TenantID       string    `json:"tenant_id"`
	CustomerID     string    `json:"customer_id,omitempty"`
	CreditNoteID   string    `json:"credit_note_id,omitempty"`
	AmountUSD      float64   `json:"amount_usd"`
	Kind           string    `json:"kind"` // FULL | PARTIAL
	ApplyToInvoice bool      `json:"apply_to_invoice"`
	Status         string    `json:"status"` // DRAFT | ISSUED | APPLIED | error
	Note           string    `json:"note,omitempty"`
}

// Log is the append-only writer for credit_notes.jsonl.
type Log struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	count  int64
}

// NewLog opens / creates path for append.
func NewLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("credit_notes: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("credit_notes: open %s: %w", path, err)
	}
	return &Log{w: f, closer: f}, nil
}

// NewLogTo wraps an io.Writer.
func NewLogTo(w io.Writer) *Log { return &Log{w: w} }

// Append writes one record + newline.
func (l *Log) Append(r Record) error {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	buf, err := json.Marshal(r)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(buf)
	if err == nil {
		l.count++
	}
	return err
}

// Close flushes the underlying file.
func (l *Log) Close() error {
	if l.closer == nil {
		return nil
	}
	err := l.closer.Close()
	l.closer = nil
	return err
}

// Count returns appended-record count.
func (l *Log) Count() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// Load reads credit_notes.jsonl from a run output dir.
func Load(dir string) ([]Record, error) {
	path := filepath.Join(dir, "credit_notes.jsonl")
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	out := []Record{}
	for {
		var r Record
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// LifecycleProgression reconstructs each credit note's state-transition trail
// from the flat record stream. Used by Check 16 to assert DRAFT → ISSUED →
// APPLIED ordering and that an applied credit note's amount matches the
// claim that brought the invoice's amount_due to 0.
type LifecycleProgression struct {
	CreditNoteID string
	InvoiceID    string
	TenantID     string
	AmountUSD    float64
	Kind         string
	HasDraft     bool
	HasIssued    bool
	HasApplied   bool
	HasError     bool
	Errors       []string
	FirstSeen    time.Time
	LastSeen     time.Time
}

// Reconstruct groups records by credit note id and computes the progression.
// Records without a credit_note_id (drafts that errored before id assignment)
// are grouped by invoice id with key "ERR-<invoice>".
func Reconstruct(records []Record) []LifecycleProgression {
	byID := map[string]*LifecycleProgression{}
	for _, r := range records {
		key := r.CreditNoteID
		if key == "" {
			key = "ERR-" + r.InvoiceID
		}
		p, ok := byID[key]
		if !ok {
			p = &LifecycleProgression{
				CreditNoteID: r.CreditNoteID,
				InvoiceID:    r.InvoiceID,
				TenantID:     r.TenantID,
				AmountUSD:    r.AmountUSD,
				Kind:         r.Kind,
				FirstSeen:    r.Timestamp,
				LastSeen:     r.Timestamp,
			}
			byID[key] = p
		}
		if r.Timestamp.After(p.LastSeen) {
			p.LastSeen = r.Timestamp
		}
		switch r.Status {
		case "DRAFT":
			p.HasDraft = true
		case "ISSUED":
			p.HasIssued = true
		case "APPLIED":
			p.HasApplied = true
		case "error":
			p.HasError = true
			if r.Note != "" {
				p.Errors = append(p.Errors, r.Note)
			}
		}
	}
	out := make([]LifecycleProgression, 0, len(byID))
	for _, p := range byID {
		out = append(out, *p)
	}
	return out
}
