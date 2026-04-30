// Package wallet drives wallet-side bookkeeping for PREPAID and HYBRID
// customers during a load run. It complements internal/validate/wallet,
// which contains pure post-run balance arithmetic.
//
// This package handles the OPERATIONAL side:
//
//   - reading every PREPAID/HYBRID customer's pre-run wallet balance
//   - polling the platform's wallet endpoints during the run to capture
//     holds + their lifecycle (PENDING → SETTLED | RELEASED)
//   - re-reading post-run balances and recording the audit trail
//
// The output is wallet_audit.jsonl, which Check 17 reads.
//
// Concurrency: the runtime collector spawns one goroutine per customer.
// Each writes to the shared log via the log's mutex.
package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// Customer is the minimal shape needed to audit one wallet.
type Customer struct {
	TenantID   string
	CustomerID string
	WalletID   string
	Currency   string
}

// Snapshot is one read of a wallet at a point in time.
type Snapshot struct {
	Timestamp     time.Time `json:"ts"`
	TenantID      string    `json:"tenant_id"`
	CustomerID    string    `json:"customer_id"`
	WalletID      string    `json:"wallet_id"`
	Currency      string    `json:"currency"`
	BalanceUSD    float64   `json:"balance_usd"` // primary balance ledger value
	HeldUSD       float64   `json:"held_usd"`    // sum of PENDING holds
	Phase         string    `json:"phase"`       // pre | mid | post | post-expiry
	HoldsActive   int       `json:"holds_active"`
	HoldsReleased int       `json:"holds_released"`
	HoldsSettled  int       `json:"holds_settled"`
	Note          string    `json:"note,omitempty"`
}

// HoldEvent is one observed wallet-hold state transition.
type HoldEvent struct {
	Timestamp      time.Time `json:"ts"`
	TenantID       string    `json:"tenant_id"`
	CustomerID     string    `json:"customer_id"`
	WalletID       string    `json:"wallet_id"`
	HoldID         string    `json:"hold_id"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	State          string    `json:"state"` // PENDING | SETTLED | RELEASED | EXPIRED
	HoldUSD        float64   `json:"hold_usd"`
	SettledUSD     float64   `json:"settled_usd,omitempty"`
	ReleasedUSD    float64   `json:"released_usd,omitempty"`
	Note           string    `json:"note,omitempty"`
}

// AuditLog is the append-only writer for wallet_audit.jsonl.
//
// One file holds two record types — Snapshot and HoldEvent — distinguished
// by an injected "type" field. Mixing them in one file (vs two) is
// deliberate: the validator wants the full chronological story per wallet,
// and a single file simplifies Replay tooling later.
type AuditLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	count  int64
}

// NewAuditLog opens / creates wallet_audit.jsonl in dir. Caller MUST Close.
func NewAuditLog(dir string) (*AuditLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wallet: mkdir: %w", err)
	}
	path := filepath.Join(dir, "wallet_audit.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("wallet: open %s: %w", path, err)
	}
	return &AuditLog{w: f, closer: f}, nil
}

// NewAuditLogTo wraps an io.Writer.
func NewAuditLogTo(w io.Writer) *AuditLog { return &AuditLog{w: w} }

// AppendSnapshot writes a Snapshot row tagged with type=snapshot.
func (a *AuditLog) AppendSnapshot(s Snapshot) error {
	if s.Timestamp.IsZero() {
		s.Timestamp = time.Now().UTC()
	}
	return a.appendTagged("snapshot", s)
}

// AppendHoldEvent writes a HoldEvent row tagged with type=hold_event.
func (a *AuditLog) AppendHoldEvent(h HoldEvent) error {
	if h.Timestamp.IsZero() {
		h.Timestamp = time.Now().UTC()
	}
	return a.appendTagged("hold_event", h)
}

func (a *AuditLog) appendTagged(t string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Inject the type discriminator without re-marshalling. We allocate a
	// new map ONLY because the two structs are heterogeneous; the cost is
	// tiny (audit cadence is per-customer-per-poll, not per-event).
	var asMap map[string]any
	if err := json.Unmarshal(buf, &asMap); err != nil {
		return err
	}
	asMap["type"] = t
	out, err := json.Marshal(asMap)
	if err != nil {
		return err
	}
	out = append(out, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.w.Write(out)
	if err == nil {
		a.count++
	}
	return err
}

// Close flushes the file. Idempotent.
func (a *AuditLog) Close() error {
	if a.closer == nil {
		return nil
	}
	err := a.closer.Close()
	a.closer = nil
	return err
}

// Count returns the appended-record count.
func (a *AuditLog) Count() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

// Collector is the runtime poller — drives Snapshot reads and HoldEvent
// transitions per customer. Uses lifecycle.Client for HTTP.
type Collector struct {
	client     *lifecycle.Client
	log        *AuditLog
	customers  []Customer
	pollEvery  time.Duration
	holdTTL    time.Duration
	postWindow time.Duration

	mu         sync.Mutex
	priorBal   map[string]Snapshot  // by customer id — last snapshot taken
	priorHolds map[string]HoldEvent // by hold id — last state
}

// CollectorConfig configures Collector. PollEvery defaults to 5s.
type CollectorConfig struct {
	Client     *lifecycle.Client
	Log        *AuditLog
	Customers  []Customer
	PollEvery  time.Duration
	HoldTTL    time.Duration // matches scenario.wallet.hold_ttl_seconds
	PostWindow time.Duration // additional window after run end before final snapshot
}

// NewCollector validates and constructs.
func NewCollector(cfg CollectorConfig) (*Collector, error) {
	if cfg.Client == nil {
		return nil, errors.New("wallet: collector requires lifecycle client")
	}
	if cfg.Log == nil {
		return nil, errors.New("wallet: collector requires audit log")
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 5 * time.Second
	}
	if cfg.PostWindow <= 0 {
		cfg.PostWindow = 90 * time.Second // typical HoldExpiryScheduler gap + buffer
	}
	return &Collector{
		client:     cfg.Client,
		log:        cfg.Log,
		customers:  cfg.Customers,
		pollEvery:  cfg.PollEvery,
		holdTTL:    cfg.HoldTTL,
		postWindow: cfg.PostWindow,
		priorBal:   map[string]Snapshot{},
		priorHolds: map[string]HoldEvent{},
	}, nil
}

// CapturePreRun reads each customer's wallet at run start and emits one
// Snapshot{Phase:"pre"} per customer. Returns an error only on transport
// failures aggregated; per-customer failures are logged with a Note.
func (c *Collector) CapturePreRun(ctx context.Context) error {
	for _, cust := range c.customers {
		s, err := c.snapshot(ctx, cust, "pre")
		if err != nil {
			s.Note = fmt.Sprintf("pre snapshot: %v", err)
		}
		_ = c.log.AppendSnapshot(s)
		c.mu.Lock()
		c.priorBal[cust.CustomerID] = s
		c.mu.Unlock()
	}
	return nil
}

// PollUntil runs Snapshot reads + HoldEvent diffs every PollEvery until ctx
// is done. Caller cancels ctx when the run engine has stopped.
func (c *Collector) PollUntil(ctx context.Context) {
	ticker := time.NewTicker(c.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.tickAll(ctx, "mid")
	}
}

// CapturePostRun runs one Snapshot per customer at run-stop, sleeps
// PostWindow + holdTTL to let HoldExpiryScheduler converge, then runs a
// final Snapshot tagged "post-expiry".
func (c *Collector) CapturePostRun(ctx context.Context) error {
	c.tickAll(ctx, "post")
	wait := c.postWindow + c.holdTTL
	if wait <= 0 {
		wait = 60 * time.Second
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
	}
	c.tickAll(ctx, "post-expiry")
	return nil
}

// tickAll snapshots every customer with the given phase tag.
func (c *Collector) tickAll(ctx context.Context, phase string) {
	for _, cust := range c.customers {
		if ctx.Err() != nil {
			return
		}
		s, err := c.snapshot(ctx, cust, phase)
		if err != nil {
			s.Note = fmt.Sprintf("snapshot: %v", err)
		}
		_ = c.log.AppendSnapshot(s)

		// Diff holds and emit any state-transition events.
		holds, err := c.fetchHolds(ctx, cust)
		if err != nil {
			continue
		}
		for _, h := range holds {
			c.mu.Lock()
			prior, hadPrior := c.priorHolds[h.HoldID]
			c.priorHolds[h.HoldID] = h
			c.mu.Unlock()
			if !hadPrior || prior.State != h.State {
				_ = c.log.AppendHoldEvent(h)
			}
		}
	}
}

// snapshot reads a wallet's current balance + summary holds count.
func (c *Collector) snapshot(ctx context.Context, cust Customer, phase string) (Snapshot, error) {
	if cust.WalletID == "" {
		return Snapshot{
			TenantID: cust.TenantID, CustomerID: cust.CustomerID,
			Phase: phase, Note: "no wallet_id; skipping",
		}, nil
	}
	path := fmt.Sprintf(aforo.PathWalletByID, cust.WalletID)
	var resp walletResponse
	if _, err := c.client.GetJSON(ctx, aforo.ServiceBilling, path, cust.TenantID, &resp); err != nil {
		return Snapshot{
			TenantID: cust.TenantID, CustomerID: cust.CustomerID,
			Phase: phase, Note: err.Error(),
		}, err
	}
	return Snapshot{
		TenantID:    cust.TenantID,
		CustomerID:  cust.CustomerID,
		WalletID:    cust.WalletID,
		Currency:    orDefault(cust.Currency, resp.Currency),
		BalanceUSD:  resp.Balance,
		HeldUSD:     resp.HeldAmount,
		Phase:       phase,
		HoldsActive: resp.HoldsActive,
	}, nil
}

// fetchHolds GETs /api/v1/wallets/{id}/holds and decodes the active hold
// rows. Returns empty when the platform has no holds endpoint exposed.
func (c *Collector) fetchHolds(ctx context.Context, cust Customer) ([]HoldEvent, error) {
	if cust.WalletID == "" {
		return nil, nil
	}
	path := fmt.Sprintf("/api/v1/wallets/%s/holds", cust.WalletID)
	var resp struct {
		Data []struct {
			ID             string  `json:"id"`
			SubscriptionID string  `json:"subscription_id"`
			State          string  `json:"state"`
			HoldUSD        float64 `json:"hold_usd"`
			SettledUSD     float64 `json:"settled_usd"`
			ReleasedUSD    float64 `json:"released_usd"`
		} `json:"data"`
	}
	_, err := c.client.GetJSON(ctx, aforo.ServiceBilling, path, cust.TenantID, &resp)
	if err != nil {
		return nil, err
	}
	out := make([]HoldEvent, 0, len(resp.Data))
	for _, h := range resp.Data {
		out = append(out, HoldEvent{
			TenantID: cust.TenantID, CustomerID: cust.CustomerID, WalletID: cust.WalletID,
			HoldID: h.ID, SubscriptionID: h.SubscriptionID,
			State: h.State, HoldUSD: h.HoldUSD,
			SettledUSD: h.SettledUSD, ReleasedUSD: h.ReleasedUSD,
		})
	}
	return out, nil
}

// walletResponse is the subset of platform GET /api/v1/wallets/{id} we read.
type walletResponse struct {
	WalletID    string  `json:"id"`
	Currency    string  `json:"currency"`
	Balance     float64 `json:"balance"`
	HeldAmount  float64 `json:"held_amount"`
	HoldsActive int     `json:"holds_active"`
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
