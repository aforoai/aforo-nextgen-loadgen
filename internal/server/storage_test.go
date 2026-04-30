package server

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStore_PutAndGet(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocalStore(root)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if got := s.Kind(); got != "local-fs" {
		t.Errorf("kind: %s", got)
	}

	uri, err := s.Put(context.Background(), "ci-smoke-1700000000-aabbccdd", bytes.NewReader([]byte(`{"hello":"world"}`)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(uri, "file://") {
		t.Errorf("uri scheme: %s", uri)
	}
	if !strings.Contains(uri, "ci-smoke-1700000000-aabbccdd.json") {
		t.Errorf("uri filename: %s", uri)
	}

	rc, err := s.Get(context.Background(), uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != `{"hello":"world"}` {
		t.Errorf("body: %s", string(body))
	}
}

func TestLocalStore_RejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocalStore(root)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}

	// A poisoned locator pointing outside root must be rejected.
	bad := "file:///etc/passwd"
	_, err = s.Get(context.Background(), bad)
	if err == nil {
		t.Fatal("expected path-traversal rejection, got nil")
	}
	if !strings.Contains(err.Error(), "escapes store root") {
		t.Errorf("want escape error, got: %v", err)
	}
}

func TestLocalStore_RejectsBadRunID(t *testing.T) {
	root := t.TempDir()
	s, _ := NewLocalStore(root)

	for _, bad := range []string{
		"",
		"../etc/passwd",
		"foo/bar",
		"FOO-UPPERCASE",
		"foo bar",
		strings.Repeat("a", 201),
	} {
		_, err := s.Put(context.Background(), bad, bytes.NewReader(nil))
		if err == nil {
			t.Errorf("Put(%q): expected error, got nil", bad)
		}
	}
}

func TestMemoryIndex_BasicCRUD(t *testing.T) {
	idx := NewMemoryIndex()
	ctx := context.Background()

	r := Run{RunID: "r1", Scenario: "ci-smoke", Target: "local", Status: RunStatusQueued}
	if err := idx.Insert(ctx, r); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Duplicate insert should error — important so we never lose
	// state by clobbering on retry.
	if err := idx.Insert(ctx, r); err == nil {
		t.Fatal("duplicate insert: expected error")
	}

	got, err := idx.Get(ctx, "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != RunStatusQueued {
		t.Errorf("status: %s", got.Status)
	}

	r.Status = RunStatusRunning
	if err := idx.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = idx.Get(ctx, "r1")
	if got.Status != RunStatusRunning {
		t.Errorf("after update: %s", got.Status)
	}

	// Update of unknown id is an error so the lifecycle goroutine
	// doesn't silently lose state when the row was deleted.
	if err := idx.Update(ctx, Run{RunID: "missing"}); err == nil {
		t.Fatal("update missing: expected error")
	}
}

func TestMemoryIndex_ListPagination(t *testing.T) {
	idx := NewMemoryIndex()
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		_ = idx.Insert(ctx, Run{RunID: "r" + string(rune('a'+i)), Scenario: "ci-smoke", Status: RunStatusCompleted})
	}
	resp, err := idx.List(ctx, ListQuery{PerPage: 3, Page: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.Total != 7 {
		t.Errorf("total: %d", resp.Total)
	}
	if len(resp.Runs) != 3 {
		t.Errorf("page len: %d", len(resp.Runs))
	}
}

func TestParseContentRangeTotal(t *testing.T) {
	cases := map[string]int{
		"0-24/123":  123,
		"0-9/10":    10,
		"":          7, // fallback
		"0-9/*":     7, // PostgREST emits */ when count not requested
		"malformed": 7,
	}
	for in, want := range cases {
		got := parseContentRangeTotal(in, 7)
		if got != want {
			t.Errorf("parse(%q)=%d want %d", in, got, want)
		}
	}
}

func TestS3Locator_BucketMismatch(t *testing.T) {
	_, err := s3LocatorURI("good-bucket", "s3://bad-bucket/key")
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if !strings.Contains(err.Error(), "bucket mismatch") {
		t.Errorf("err: %v", err)
	}
}

func TestLocalPathFromLocator_AbsoluteOnly(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	abs, err := localPathFromLocator(root, "file://"+root+"/foo.json")
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if abs != root+"/foo.json" {
		t.Errorf("path: %s", abs)
	}

	if _, err := localPathFromLocator(root, "https://example.com/foo"); err == nil {
		t.Error("non-file:// locator should be rejected")
	}
}
