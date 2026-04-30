package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// recordingHandler captures every request and returns canned responses.
// One handler per test — fresh state each time.
type recordingHandler struct {
	mu       sync.Mutex
	requests []capturedRequest

	respond func(r *http.Request) (int, string)
}

type capturedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r.Body)
	h.mu.Lock()
	h.requests = append(h.requests, capturedRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: r.Header.Clone(),
		Body:    string(body),
	})
	h.mu.Unlock()
	status, resp := h.respond(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(resp))
}

func readAll(rc interface{ Read([]byte) (int, error) }) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	tmp := make([]byte, 1024)
	for {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	return buf.Bytes(), nil
}

func newTestDeps(t *testing.T, handler *recordingHandler) (Deps, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	target := aforo.Target{
		Name: "test",
		URLs: map[aforo.Service]string{
			aforo.ServicePricing:       srv.URL,
			aforo.ServiceCatalog:       srv.URL,
			aforo.ServiceCustomer:      srv.URL,
			aforo.ServiceBilling:       srv.URL,
			aforo.ServiceOrganization:  srv.URL,
			aforo.ServiceUsageIngestor: srv.URL,
		},
	}
	client, err := NewClient(ClientConfig{Target: target, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	tlog := NewTransitionLogTo(logBuf)

	mf := &seed.Manifest{
		Tenants: []seed.ManifestTenant{
			{
				TenantID:  "ten-1",
				Archetype: "ar-1",
				Offerings: []seed.ManifestOffering{
					{OfferingID: "off-1"},
					{OfferingID: "off-2"},
				},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "cust-1",
						Subscriptions: []seed.ManifestSubscription{
							{SubscriptionID: "sub-1", Status: scenario.StateActive},
							{SubscriptionID: "sub-2", Status: scenario.StateTrialing},
							{SubscriptionID: "sub-3", Status: scenario.StatePastDue},
						},
					},
				},
			},
		},
	}
	picker := NewPicker(mf, 1)
	picker.subjects[0].CurrentOffer = "off-1"

	return Deps{
		Client:        client,
		Log:           tlog,
		Picker:        picker,
		Resumes:       NewResumeScheduler(),
		Dunning:       NewDunningWalker(DefaultDunningConfig()),
		ResumeTimeout: 5 * time.Second,
	}, srv, logBuf
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []TransitionRecord {
	t.Helper()
	return decodeRecordsBytes(t, buf.Bytes())
}

// decodeRecordsBytes decodes from a copy of the buffer's bytes — used by
// tests that involve background goroutines so the race detector doesn't
// fire on a concurrent buf.String()/Bytes() vs Append. Pair with
// TransitionLog.BytesSnapshot() which copies under the log's mutex.
func decodeRecordsBytes(t *testing.T, raw []byte) []TransitionRecord {
	t.Helper()
	out := []TransitionRecord{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec TransitionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func subjectActive() Subject {
	return Subject{
		TenantID:       "ten-1",
		SubscriptionID: "sub-1",
		Archetype:      "ar-1",
		State:          scenario.StateActive,
		OfferingIDs:    []string{"off-1", "off-2"},
		CurrentOffer:   "off-1",
	}
}

func TestFireUpgrade_Success(t *testing.T) {
	h := &recordingHandler{
		respond: func(r *http.Request) (int, string) {
			if !strings.HasSuffix(r.URL.Path, "/upgrade") {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			if r.Header.Get("X-Tenant-Id") != "ten-1" {
				t.Errorf("missing X-Tenant-Id")
			}
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("missing Idempotency-Key")
			}
			return 200, `{"id":"sub-1","status":"ACTIVE","offeringId":"off-2"}`
		},
	}
	deps, _, buf := newTestDeps(t, h)
	if err := FireUpgrade(context.Background(), deps, subjectActive()); err != nil {
		t.Fatalf("FireUpgrade: %v", err)
	}
	if h.requests[0].Headers.Get("Authorization") != "Bearer test-token" {
		t.Errorf("missing bearer token")
	}
	recs := decodeRecords(t, buf)
	// Two rows: intent + outcome.
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (intent + outcome), got %d: %s", len(recs), buf.String())
	}
	if recs[1].TransitionStatus != StatusOK {
		t.Errorf("outcome status = %s, want OK", recs[1].TransitionStatus)
	}
	if recs[1].FromOffering != "off-1" || recs[1].ToOffering != "off-2" {
		t.Errorf("offerings recorded incorrectly: %s → %s", recs[1].FromOffering, recs[1].ToOffering)
	}
	if recs[1].HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d", recs[1].HTTPStatus)
	}
}

func TestFireUpgrade_409Conflict_LogsFail(t *testing.T) {
	h := &recordingHandler{
		respond: func(r *http.Request) (int, string) {
			return 409, `{"error":"conflict"}`
		},
	}
	deps, _, buf := newTestDeps(t, h)
	err := FireUpgrade(context.Background(), deps, subjectActive())
	if err == nil {
		t.Fatal("expected error on 409")
	}
	recs := decodeRecords(t, buf)
	last := recs[len(recs)-1]
	if last.TransitionStatus != StatusFail {
		t.Errorf("status = %s, want FAIL", last.TransitionStatus)
	}
	if last.HTTPStatus != 409 {
		t.Errorf("HTTPStatus = %d", last.HTTPStatus)
	}
}

func TestFireUpgrade_SkipsWhenSingleOffering(t *testing.T) {
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		t.Error("HTTP should not be called when no migrate target exists")
		return 500, "{}"
	}}
	deps, _, buf := newTestDeps(t, h)
	// Override: make the subject's tenant report only one offering.
	s := Subject{
		TenantID: "ten-1", SubscriptionID: "sub-x", State: scenario.StateActive,
		OfferingIDs: []string{"only"}, CurrentOffer: "only",
	}
	if err := FireUpgrade(context.Background(), deps, s); err != nil {
		t.Fatalf("expected no error on skip, got %v", err)
	}
	recs := decodeRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("expected 1 SKIPPED record, got %d", len(recs))
	}
	if recs[0].TransitionStatus != StatusSkipped {
		t.Errorf("status = %s, want SKIPPED", recs[0].TransitionStatus)
	}
}

func TestFirePauseAndScheduleResume_Success(t *testing.T) {
	var pauseCalls, resumeCalls atomic.Int32
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pause"):
			pauseCalls.Add(1)
			return 200, `{"id":"sub-1","status":"PAUSED"}`
		case strings.HasSuffix(r.URL.Path, "/resume"):
			resumeCalls.Add(1)
			return 200, `{"id":"sub-1","status":"ACTIVE"}`
		}
		return 500, "{}"
	}}
	deps, _, buf := newTestDeps(t, h)

	if err := FirePauseAndScheduleResume(context.Background(), deps, subjectActive(), 30*time.Millisecond); err != nil {
		t.Fatalf("FirePauseAndScheduleResume: %v", err)
	}
	if pauseCalls.Load() != 1 {
		t.Fatalf("pause calls = %d, want 1", pauseCalls.Load())
	}
	// Wait until BOTH the resume HTTP call has fired AND the resume
	// goroutine has finished its post-call work (state mutation +
	// transition log writes). Polling resumeCalls alone is racy: the
	// HTTP response can land before fireResume returns from Append, so
	// reading state immediately after resumeCalls increments can
	// observe a half-committed transition. Count() takes the
	// transition-log lock, so once it reads 4 the resume goroutine has
	// definitely returned (intent + outcome × 2 transitions).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resumeCalls.Load() == 1 && deps.Log.Count() == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resumeCalls.Load() != 1 {
		t.Fatalf("resume calls = %d, want 1", resumeCalls.Load())
	}
	if deps.Log.Count() != 4 {
		t.Fatalf("log count = %d, want 4 (resume goroutine has not finished writing)", deps.Log.Count())
	}
	// Picker should reflect ACTIVE again.
	if got := deps.Picker.LiveState("sub-1"); got != scenario.StateActive {
		t.Errorf("after resume, live state = %s, want ACTIVE", got)
	}
	// Transition log: pause-intent + pause-outcome + resume-intent + resume-outcome.
	// Use the lock-safe BytesSnapshot — reading the bytes.Buffer directly
	// would race with any (rarely-still-pending) Append even after Count
	// reads 4 because bytes.Buffer's read methods don't observe the lock.
	snap, ok := deps.Log.BytesSnapshot()
	if !ok {
		t.Fatal("test transition log is not buffer-backed")
	}
	_ = buf // buf is shared with snap; we use snap for the lock-safe view
	recs := decodeRecordsBytes(t, snap)
	kinds := []TransitionKind{}
	for _, r := range recs {
		kinds = append(kinds, r.Transition)
	}
	want := []TransitionKind{TransitionPause, TransitionPause, TransitionResume, TransitionResume}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("transition kinds = %v, want %v", kinds, want)
	}
}

func TestResumeScheduler_CancelStopsPendingResumes(t *testing.T) {
	rs := NewResumeScheduler()
	var fired atomic.Int32
	rs.Schedule("a", 100*time.Millisecond, func() { fired.Add(1) })
	rs.Schedule("b", 200*time.Millisecond, func() { fired.Add(1) })
	if rs.PendingCount() != 2 {
		t.Fatalf("pending = %d, want 2", rs.PendingCount())
	}
	rs.Cancel()
	// Sleep past both delays — neither should fire.
	time.Sleep(300 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("fired = %d after Cancel(), want 0", fired.Load())
	}
}

func TestResumeScheduler_ReplacePending(t *testing.T) {
	rs := NewResumeScheduler()
	var first, second atomic.Int32
	rs.Schedule("a", 100*time.Millisecond, func() { first.Add(1) })
	rs.Schedule("a", 200*time.Millisecond, func() { second.Add(1) })
	time.Sleep(300 * time.Millisecond)
	rs.Cancel()
	if first.Load() != 0 {
		t.Errorf("first should have been replaced, got %d firings", first.Load())
	}
	if second.Load() != 1 {
		t.Errorf("second firings = %d, want 1", second.Load())
	}
}

func TestFireMigrate_StableIDViolation_Logged(t *testing.T) {
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		// Return source != target — the agent should detect and log a synthetic FAIL row.
		return 200, `{"sourceSubscriptionId":"sub-1","targetSubscriptionId":"sub-1-NEW","calendarRefundUsd":12.34}`
	}}
	deps, _, buf := newTestDeps(t, h)
	if err := FireMigrate(context.Background(), deps, subjectActive()); err != nil {
		t.Fatalf("FireMigrate: %v", err)
	}
	recs := decodeRecords(t, buf)
	// Find the synthetic violation row.
	violationFound := false
	for _, r := range recs {
		if r.Transition == TransitionMigrate &&
			r.TransitionStatus == StatusFail &&
			strings.Contains(r.Error, "stable-id violation") {
			violationFound = true
		}
	}
	if !violationFound {
		t.Fatalf("expected stable-id violation row in:\n%s", buf.String())
	}
}

func TestFireTrialConversion_FlipsStateToActive(t *testing.T) {
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/convert-trial") {
			t.Errorf("path = %s", r.URL.Path)
		}
		return 200, `{"id":"sub-2","status":"ACTIVE"}`
	}}
	deps, _, _ := newTestDeps(t, h)
	s := Subject{
		TenantID: "ten-1", SubscriptionID: "sub-2", State: scenario.StateTrialing,
		OfferingIDs: []string{"off-1", "off-2"}, CurrentOffer: "off-1",
	}
	if err := FireTrialConversion(context.Background(), deps, s); err != nil {
		t.Fatalf("FireTrialConversion: %v", err)
	}
	if got := deps.Picker.LiveState("sub-2"); got != scenario.StateActive {
		t.Errorf("after trial conversion, state = %s, want ACTIVE", got)
	}
}

func TestFireTrialCancel_MarksCancelledAndSuspended(t *testing.T) {
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Errorf("path = %s", r.URL.Path)
		}
		return 200, `{}`
	}}
	deps, _, _ := newTestDeps(t, h)
	s := Subject{
		TenantID: "ten-1", SubscriptionID: "sub-2", State: scenario.StateTrialing,
		OfferingIDs: []string{"off-1", "off-2"},
	}
	if err := FireTrialCancel(context.Background(), deps, s); err != nil {
		t.Fatalf("FireTrialCancel: %v", err)
	}
	if got := deps.Picker.LiveState("sub-2"); got != scenario.StateCancelled {
		t.Errorf("state = %s, want CANCELLED", got)
	}
	// Suspended subs are excluded from future picks.
	if cnt := deps.Picker.EligibleCount(TransitionUpgrade); cnt > 2 {
		// Original picker had 3 subjects (sub-1 ACTIVE, sub-2 TRIALING was eligible, sub-3 PAST_DUE was eligible).
		// After cancelling sub-2 + suspending it, eligible UPGRADE is sub-1, sub-3 → 2.
		t.Errorf("after cancel, UPGRADE eligibility = %d (sub-2 should be suspended)", cnt)
	}
}

func TestFireRetryPayment_RecordsHTTPStatus(t *testing.T) {
	h := &recordingHandler{respond: func(r *http.Request) (int, string) {
		return 200, `{"id":"sub-3","status":"ACTIVE","dunningAttempt":2}`
	}}
	deps, _, buf := newTestDeps(t, h)
	s := Subject{TenantID: "ten-1", SubscriptionID: "sub-3", State: scenario.StatePastDue}
	if err := FireRetryPayment(context.Background(), deps, s); err != nil {
		t.Fatalf("FireRetryPayment: %v", err)
	}
	recs := decodeRecords(t, buf)
	last := recs[len(recs)-1]
	if last.DunningAttempt != 2 {
		t.Errorf("DunningAttempt = %d, want 2", last.DunningAttempt)
	}
	if last.ExpectedPostState != "ACTIVE" {
		t.Errorf("ExpectedPostState = %q, want ACTIVE", last.ExpectedPostState)
	}
}

func TestIdempotencyKey_StableForSameInputs(t *testing.T) {
	s := Subject{TenantID: "t", SubscriptionID: "s"}
	a := idempotencyKey(s, TransitionUpgrade)
	b := idempotencyKey(s, TransitionUpgrade)
	if a != b {
		t.Fatalf("idempotency key not stable: %s != %s", a, b)
	}
	c := idempotencyKey(s, TransitionDowngrade)
	if a == c {
		t.Fatalf("idempotency key collided across kinds")
	}
}
