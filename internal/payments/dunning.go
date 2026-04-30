package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// DunningDriver walks an invoice / subscription pair through the platform's
// dunning sequence after an initial decline:
//
//	[INITIAL DECLINE] → invoice OPEN, sub PAST_DUE
//	[retry 1]         → still failing → sub PAST_DUE, dunning_attempt=1
//	[retry 2]         → still failing → sub PAST_DUE, dunning_attempt=2
//	[retry N=max]     → escalate      → sub SUSPENDED  or CANCELLED
//
// The driver fires N "retry-payment" calls separated by Config.Interval.
// Between calls, the driver yields to the platform's DunningScheduler so
// each retry has a clean state. After N failures, the driver asserts the
// platform has flipped the sub to SUSPENDED or CANCELLED.
//
// Why this is a separate goroutine per invoice: real dunning is days apart;
// the loadgen compresses to seconds, but multiple sub histories run in
// parallel. One go-routine per invoice keeps the dunning-history record
// per-invoice and avoids "did this retry belong to which sub" confusion in
// transitions.jsonl.
type DunningDriver struct {
	client       *lifecycle.Client
	transitions  *lifecycle.TransitionLog
	maxAttempts  int
	interval     time.Duration
	idemPrefix   string

	mu     sync.Mutex
	cycles map[string]*DunningHistory // by subscription id
}

// DunningConfig configures the driver.
type DunningConfig struct {
	Client          *lifecycle.Client
	TransitionLog   *lifecycle.TransitionLog
	MaxAttempts     int
	Interval        time.Duration
	IdempotencyPrefix string
}

// NewDunningDriver builds the driver. MaxAttempts defaults to 3; Interval
// defaults to 60s (compressed for tests); IdempotencyPrefix defaults
// "aforo-loadgen-dunning".
func NewDunningDriver(cfg DunningConfig) (*DunningDriver, error) {
	if cfg.Client == nil {
		return nil, errors.New("payments: dunning driver requires lifecycle client")
	}
	if cfg.TransitionLog == nil {
		return nil, errors.New("payments: dunning driver requires transition log")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.IdempotencyPrefix == "" {
		cfg.IdempotencyPrefix = "aforo-loadgen-dunning"
	}
	return &DunningDriver{
		client:      cfg.Client,
		transitions: cfg.TransitionLog,
		maxAttempts: cfg.MaxAttempts,
		interval:    cfg.Interval,
		idemPrefix:  cfg.IdempotencyPrefix,
		cycles:      map[string]*DunningHistory{},
	}, nil
}

// DunningHistory is the per-invoice trail the driver records.
type DunningHistory struct {
	TenantID         string
	SubscriptionID   string
	InvoiceID        string
	Attempts         []DunningAttempt
	FinalState       string // platform's final sub state
	EscalatedAt      time.Time
}

// DunningAttempt is one retry attempt + its outcome.
type DunningAttempt struct {
	Number     int
	StartedAt  time.Time
	HTTPStatus int
	Outcome    string // "succeeded" | "still_failing" | "platform_error"
	Note       string
}

// Walk runs the dunning sequence for one (sub, invoice). Returns the
// terminal state observed.
//
// The sequence:
//
//	  for attempt = 1..maxAttempts:
//	      sleep interval
//	      POST /subscriptions/{sub}/retry-payment
//	      examine response — success / still_failing / error
//	  if still failing:
//	      assert platform has flipped sub to SUSPENDED or CANCELLED
//
// Concurrency: safe; the driver guards its history map.
func (d *DunningDriver) Walk(ctx context.Context, tenantID, subID, invoiceID string) (*DunningHistory, error) {
	hist := &DunningHistory{
		TenantID: tenantID, SubscriptionID: subID, InvoiceID: invoiceID,
		Attempts: make([]DunningAttempt, 0, d.maxAttempts),
	}
	d.mu.Lock()
	d.cycles[subID] = hist
	d.mu.Unlock()

	for n := 1; n <= d.maxAttempts; n++ {
		select {
		case <-ctx.Done():
			return hist, ctx.Err()
		case <-time.After(d.interval):
		}
		attempt := DunningAttempt{Number: n, StartedAt: time.Now().UTC()}
		idem := fmt.Sprintf("%s-%s-%d", d.idemPrefix, subID, n)

		path := fmt.Sprintf(aforo.PathSubscriptionRetryPayment, subID)
		var resp map[string]any
		status, err := d.client.PostJSON(ctx, aforo.ServicePricing, path, tenantID, idem, struct{}{}, &resp)
		attempt.HTTPStatus = status
		switch {
		case err == nil:
			// Inspect platform's view of the sub via the response or a follow-up GET.
			// The platform v3 contract returns SubscriptionResponse with status.
			subStatus := readString(resp, "status")
			if subStatus == "ACTIVE" {
				attempt.Outcome = "succeeded"
				attempt.Note = "sub recovered"
				hist.Attempts = append(hist.Attempts, attempt)
				hist.FinalState = subStatus
				_ = d.appendTransition(tenantID, subID, n, idem, status, attempt.Outcome, "")
				return hist, nil
			}
			attempt.Outcome = "still_failing"
			attempt.Note = fmt.Sprintf("sub status=%s", subStatus)
		case status == 0:
			attempt.Outcome = "platform_error"
			attempt.Note = "transport error: " + err.Error()
		default:
			attempt.Outcome = "still_failing"
			attempt.Note = fmt.Sprintf("retry-payment %d: %s", status, err.Error())
		}
		hist.Attempts = append(hist.Attempts, attempt)
		_ = d.appendTransition(tenantID, subID, n, idem, status, attempt.Outcome, attempt.Note)
	}

	// All attempts failed — assert escalation.
	finalState, err := d.observeTerminalState(ctx, tenantID, subID)
	hist.FinalState = finalState
	hist.EscalatedAt = time.Now().UTC()

	expected := finalState == "SUSPENDED" || finalState == "CANCELLED"
	note := fmt.Sprintf("post-max final_state=%s expected_terminal=%t", finalState, expected)
	_ = d.transitions.Append(lifecycle.TransitionRecord{
		Timestamp:        time.Now().UTC(),
		SubscriptionID:   subID,
		TenantID:         tenantID,
		Transition:       lifecycle.TransitionDunningEscalate,
		ExpectedPostState: "SUSPENDED",
		TransitionStatus: terminalStatus(expected, err),
		Error:            note,
	})
	return hist, nil
}

func terminalStatus(escalatedOK bool, err error) lifecycle.TransitionStatus {
	if err != nil {
		return lifecycle.StatusFail
	}
	if escalatedOK {
		return lifecycle.StatusOK
	}
	return lifecycle.StatusFail
}

// observeTerminalState reads the sub's final state. Best-effort: returns
// empty when the GET fails — the caller logs but doesn't crash.
func (d *DunningDriver) observeTerminalState(ctx context.Context, tenantID, subID string) (string, error) {
	path := fmt.Sprintf(aforo.PathSubscriptionByID, subID)
	var resp map[string]any
	_, err := d.client.GetJSON(ctx, aforo.ServicePricing, path, tenantID, &resp)
	if err != nil {
		return "", err
	}
	return readString(resp, "status"), nil
}

func (d *DunningDriver) appendTransition(tenant, sub string, n int, idem string, httpStatus int, outcome, note string) error {
	st := lifecycle.StatusOK
	if outcome == "platform_error" {
		st = lifecycle.StatusFail
	}
	return d.transitions.Append(lifecycle.TransitionRecord{
		Timestamp:        time.Now().UTC(),
		SubscriptionID:   sub,
		TenantID:         tenant,
		Transition:       lifecycle.TransitionDunningStep,
		IdempotencyKey:   idem,
		HTTPStatus:       httpStatus,
		DunningAttempt:   n,
		TransitionStatus: st,
		Error:            note,
	})
}

// Histories returns a snapshot of every dunning history the driver has
// recorded — used by the post-run summary and by the validator.
func (d *DunningDriver) Histories() []*DunningHistory {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*DunningHistory, 0, len(d.cycles))
	for _, h := range d.cycles {
		out = append(out, h)
	}
	return out
}

func readString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
