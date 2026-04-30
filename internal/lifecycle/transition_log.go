package lifecycle

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TransitionStatus is the verdict of one attempted transition.
//
// Two rows are written per transition:
//   - An INTENT row (status=PENDING) immediately before the API call
//   - An OUTCOME row (status=OK|FAIL|SKIPPED) after the API returns
//
// The intent row exists so a hung API doesn't void the trail. The validator
// counts only OUTCOME rows for transition tallies (PENDING is skipped).
type TransitionStatus string

const (
	StatusPending TransitionStatus = "PENDING" // intent row, awaiting API
	StatusOK      TransitionStatus = "OK"
	StatusFail    TransitionStatus = "FAIL"
	StatusSkipped TransitionStatus = "SKIPPED" // sub became ineligible mid-flight
)

// TransitionRecord is one row of transitions.jsonl. Stable JSON shape —
// the validator unmarshals this directly. New fields ADD only; never
// rename or repurpose.
type TransitionRecord struct {
	Timestamp                  time.Time        `json:"ts"`
	SubscriptionID             string           `json:"subscription_id"`
	TenantID                   string           `json:"tenant_id"`
	CustomerID                 string           `json:"customer_id,omitempty"`
	Archetype                  string           `json:"archetype,omitempty"`
	Transition                 TransitionKind   `json:"transition"`
	FromState                  string           `json:"from_state,omitempty"`
	ExpectedPostState          string           `json:"expected_post_state,omitempty"`
	FromOffering               string           `json:"from_offering,omitempty"`
	ToOffering                 string           `json:"to_offering,omitempty"`
	ExpectedProrationCreditUSD float64          `json:"expected_proration_credit_usd,omitempty"`
	IdempotencyKey             string           `json:"idempotency_key,omitempty"`
	TransitionStatus           TransitionStatus `json:"transition_status"`
	HTTPStatus                 int              `json:"http_status,omitempty"`
	DurationMs                 float64          `json:"duration_ms,omitempty"`
	Error                      string           `json:"error,omitempty"`
	// DunningAttempt is non-zero on dunning-walker rows; it is the dunning
	// retry counter as the agent observed it pre-call.
	DunningAttempt int `json:"dunning_attempt,omitempty"`
}

// TransitionLog is the append-only writer for transitions.jsonl.
//
// CONTRACT — safe for concurrent use by all transition modules. Each
// Append serializes one record + flushes; the file is recoverable if the
// agent crashes mid-run because every record is one self-contained line.
//
// The "log BEFORE the API call" rule (see package docs) is implemented
// by the transition modules calling AppendIntent → calling the API →
// calling AppendOutcome with the resolved status. Both writes are
// independent, so even an agent that hangs on a slow API leaves a
// breadcrumb in the log.
type TransitionLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	count  int

	// stats — consumed by HTML report builder + agent shutdown summary.
	statsMu     sync.Mutex
	byKind      map[TransitionKind]int
	byStatus    map[TransitionStatus]int
	failReasons map[TransitionKind][]string
}

// NewTransitionLog opens (or creates) <dir>/transitions.jsonl for append.
// Caller MUST call Close when the agent shuts down.
func NewTransitionLog(dir string) (*TransitionLog, error) {
	if dir == "" {
		return nil, fmt.Errorf("lifecycle: transition log dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lifecycle: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "transitions.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // #nosec G304 — caller-controlled
	if err != nil {
		return nil, fmt.Errorf("lifecycle: open %s: %w", path, err)
	}
	return &TransitionLog{
		w:           f,
		closer:      f,
		byKind:      map[TransitionKind]int{},
		byStatus:    map[TransitionStatus]int{},
		failReasons: map[TransitionKind][]string{},
	}, nil
}

// NewTransitionLogTo wraps an arbitrary writer — useful for tests that
// want to assert against a bytes.Buffer.
func NewTransitionLogTo(w io.Writer) *TransitionLog {
	return &TransitionLog{
		w:           w,
		byKind:      map[TransitionKind]int{},
		byStatus:    map[TransitionStatus]int{},
		failReasons: map[TransitionKind][]string{},
	}
}

// Append writes one record. Stamps Timestamp if zero (so callers can pass
// an empty time for "now"). Updates the in-memory roll-up stats.
func (l *TransitionLog) Append(rec TransitionRecord) error {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("lifecycle: marshal record: %w", err)
	}
	buf = append(buf, '\n')

	l.mu.Lock()
	if _, err := l.w.Write(buf); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("lifecycle: write transitions.jsonl: %w", err)
	}
	l.count++
	l.mu.Unlock()

	l.statsMu.Lock()
	l.byKind[rec.Transition]++
	l.byStatus[rec.TransitionStatus]++
	if rec.TransitionStatus == StatusFail && rec.Error != "" {
		// Cap stored fail reasons per kind — long runs would otherwise
		// balloon memory.
		if len(l.failReasons[rec.Transition]) < 10 {
			l.failReasons[rec.Transition] = append(l.failReasons[rec.Transition], rec.Error)
		}
	}
	l.statsMu.Unlock()
	return nil
}

// Count returns the number of records appended so far. Cheap — used in
// shutdown logs.
func (l *TransitionLog) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// Snapshot returns a roll-up of transition counts by kind and by status.
// Returned maps are copies — safe to inspect after Close.
func (l *TransitionLog) Snapshot() Snapshot {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	out := Snapshot{
		ByKind:      make(map[TransitionKind]int, len(l.byKind)),
		ByStatus:    make(map[TransitionStatus]int, len(l.byStatus)),
		FailReasons: make(map[TransitionKind][]string, len(l.failReasons)),
	}
	for k, v := range l.byKind {
		out.ByKind[k] = v
	}
	for k, v := range l.byStatus {
		out.ByStatus[k] = v
	}
	for k, v := range l.failReasons {
		cp := make([]string, len(v))
		copy(cp, v)
		out.FailReasons[k] = cp
	}
	return out
}

// Snapshot is the structured roll-up returned by TransitionLog.Snapshot.
type Snapshot struct {
	ByKind      map[TransitionKind]int
	ByStatus    map[TransitionStatus]int
	FailReasons map[TransitionKind][]string
}

// Close flushes the underlying file (if any). Safe to call multiple times.
func (l *TransitionLog) Close() error {
	if l.closer == nil {
		return nil
	}
	err := l.closer.Close()
	l.closer = nil
	return err
}

// LoadTransitionLog reads transitions.jsonl from the run output dir and
// returns every record. Used by the validator (Checks 9-11).
//
// Returns an empty slice (not an error) if the file doesn't exist — runs
// without --lifecycle have nothing to validate, and the validator handles
// "no transitions" by SKIPping the lifecycle checks.
func LoadTransitionLog(dir string) ([]TransitionRecord, error) {
	path := filepath.Join(dir, "transitions.jsonl")
	f, err := os.Open(path) // #nosec G304 — caller-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lifecycle: open %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	out := []TransitionRecord{}
	for {
		var rec TransitionRecord
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("lifecycle: parse %s: %w", path, err)
		}
		out = append(out, rec)
	}
	return out, nil
}
