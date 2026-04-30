package lifecycle

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransitionLog_AppendAndCount(t *testing.T) {
	dir := t.TempDir()
	log, err := NewTransitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	rec := TransitionRecord{
		SubscriptionID:   "sub-1",
		TenantID:         "ten-1",
		Transition:       TransitionUpgrade,
		FromState:        "ACTIVE",
		TransitionStatus: StatusOK,
	}
	if err := log.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := log.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}

	// Confirm timestamp was stamped.
	loaded, err := LoadTransitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d records, want 1", len(loaded))
	}
	if loaded[0].Timestamp.IsZero() {
		t.Fatal("timestamp should have been auto-stamped")
	}
	if loaded[0].SubscriptionID != "sub-1" {
		t.Errorf("loaded SubscriptionID = %q", loaded[0].SubscriptionID)
	}
}

func TestTransitionLog_ConcurrentAppend(t *testing.T) {
	buf := &bytes.Buffer{}
	log := NewTransitionLogTo(buf)

	var wg sync.WaitGroup
	const writers = 8
	const writes = 50
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				_ = log.Append(TransitionRecord{
					SubscriptionID:   "sub",
					TenantID:         "ten",
					Transition:       TransitionUpgrade,
					TransitionStatus: StatusOK,
					Timestamp:        time.Now().UTC(),
					IdempotencyKey:   "key-" + string(rune('a'+w)),
				})
			}
		}()
	}
	wg.Wait()

	if log.Count() != writers*writes {
		t.Fatalf("Count = %d, want %d", log.Count(), writers*writes)
	}
	// Every line must be valid JSON — concurrent writers must NOT interleave.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != writers*writes {
		t.Fatalf("got %d lines, want %d", len(lines), writers*writes)
	}
	for i, line := range lines {
		var rec TransitionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v\nline=%q", i, err, line)
		}
	}
}

func TestTransitionLog_Snapshot(t *testing.T) {
	log := NewTransitionLogTo(&bytes.Buffer{})
	_ = log.Append(TransitionRecord{Transition: TransitionUpgrade, TransitionStatus: StatusOK})
	_ = log.Append(TransitionRecord{Transition: TransitionUpgrade, TransitionStatus: StatusOK})
	_ = log.Append(TransitionRecord{Transition: TransitionUpgrade, TransitionStatus: StatusFail, Error: "409"})
	_ = log.Append(TransitionRecord{Transition: TransitionPause, TransitionStatus: StatusOK})
	_ = log.Append(TransitionRecord{Transition: TransitionPause, TransitionStatus: StatusSkipped, Error: "no candidate"})

	s := log.Snapshot()
	if s.ByKind[TransitionUpgrade] != 3 {
		t.Errorf("UPGRADE count = %d, want 3", s.ByKind[TransitionUpgrade])
	}
	if s.ByKind[TransitionPause] != 2 {
		t.Errorf("PAUSE count = %d, want 2", s.ByKind[TransitionPause])
	}
	if s.ByStatus[StatusOK] != 3 {
		t.Errorf("OK = %d, want 3", s.ByStatus[StatusOK])
	}
	if s.ByStatus[StatusFail] != 1 {
		t.Errorf("FAIL = %d, want 1", s.ByStatus[StatusFail])
	}
	if s.ByStatus[StatusSkipped] != 1 {
		t.Errorf("SKIPPED = %d, want 1", s.ByStatus[StatusSkipped])
	}
	if len(s.FailReasons[TransitionUpgrade]) != 1 {
		t.Errorf("UPGRADE fail reasons = %d, want 1", len(s.FailReasons[TransitionUpgrade]))
	}
}

func TestLoadTransitionLog_MissingFile_NoError(t *testing.T) {
	dir := t.TempDir()
	recs, err := LoadTransitionLog(filepath.Join(dir, "nonexistent"))
	if err == nil {
		// LoadTransitionLog opens dir/transitions.jsonl; "nonexistent" is the
		// caller passing a fake dir → file doesn't exist → empty + nil.
		if len(recs) != 0 {
			t.Fatalf("non-existent file should return empty, got %d", len(recs))
		}
	}
	// Use a real empty dir — same expectation.
	recs, err = LoadTransitionLog(dir)
	if err != nil {
		t.Fatalf("empty dir should not error, got: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("empty dir should return zero records, got %d", len(recs))
	}
}
