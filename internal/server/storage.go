package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ManifestStore persists run manifests for download. Two impls:
//
//   - LocalStore: writes to --manifests-dir; URL is file://<abs-path>
//   - S3Store: shells out to `aws s3 cp` to push under s3://bucket/key.
//     Requires `aws` in PATH and IAM creds via the standard chain.
//
// Returning an addressable URL (file:// or s3://) lets the Control
// Tower detail page render the right "Download" / "Open in S3" link
// without needing to know the storage class.
type ManifestStore interface {
	Put(ctx context.Context, runID string, src io.Reader) (string, error)
	Get(ctx context.Context, locator string) (io.ReadCloser, error)
	Kind() string // "local-fs" | "s3"
}

// LocalStore persists manifests as files under Root. URL scheme is
// file:///abs/path/<run-id>.json.
type LocalStore struct {
	Root string
}

// NewLocalStore returns a store rooted at root. The directory is
// created with 0o755 perms if it doesn't already exist.
func NewLocalStore(root string) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("local manifest store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{Root: abs}, nil
}

func (l *LocalStore) Put(_ context.Context, runID string, src io.Reader) (string, error) {
	if !validRunID(runID) {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	dst := filepath.Join(l.Root, runID+".json")
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return "", err
	}
	return "file://" + dst, nil
}

func (l *LocalStore) Get(_ context.Context, locator string) (io.ReadCloser, error) {
	path, err := localPathFromLocator(l.Root, locator)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (l *LocalStore) Kind() string { return "local-fs" }

// S3Store pushes manifests to s3://Bucket/Prefix/<run-id>.json via the
// aws CLI. The CLI is the only AWS surface in this codebase — using
// the SDK would pull in a multi-MB dependency for a single PUT.
type S3Store struct {
	Bucket string
	Prefix string
	// AWSBin lets tests stub the CLI with a fake. Default "aws".
	AWSBin string
}

// NewS3Store returns a configured store. bucket is required; prefix
// is optional (defaults to "loadgen-runs/").
func NewS3Store(bucket, prefix string) (*S3Store, error) {
	if bucket == "" {
		return nil, errors.New("s3 bucket is required")
	}
	if prefix == "" {
		prefix = "loadgen-runs/"
	}
	prefix = strings.TrimLeft(prefix, "/")
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	bin, err := exec.LookPath("aws")
	if err != nil {
		return nil, fmt.Errorf("aws CLI not in PATH (required for s3 manifest store): %w", err)
	}
	return &S3Store{Bucket: bucket, Prefix: prefix, AWSBin: bin}, nil
}

func (s *S3Store) Put(ctx context.Context, runID string, src io.Reader) (string, error) {
	if !validRunID(runID) {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	key := s.Prefix + runID + ".json"
	uri := "s3://" + s.Bucket + "/" + key

	cmd := exec.CommandContext(ctx, s.AWSBin, "s3", "cp", "-", uri)
	cmd.Stdin = src
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("aws s3 cp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return uri, nil
}

func (s *S3Store) Get(ctx context.Context, locator string) (io.ReadCloser, error) {
	uri, err := s3LocatorURI(s.Bucket, locator)
	if err != nil {
		return nil, err
	}
	// Stream to a temp file via `aws s3 cp <uri> -` (stdout). Using
	// CombinedOutput here would buffer everything in memory; we want
	// a stream. Pipe stdout instead.
	cmd := exec.CommandContext(ctx, s.AWSBin, "s3", "cp", uri, "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReadCloser{Reader: stdout, cmd: cmd}, nil
}

func (s *S3Store) Kind() string { return "s3" }

// cmdReadCloser wraps a process pipe so the caller can Close() the
// stream and reap the subprocess.
type cmdReadCloser struct {
	io.Reader
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	// Drain remaining bytes to avoid SIGPIPE on the subprocess. Cap
	// the drain at 4 MiB so a malicious server can't block us
	// indefinitely.
	_, _ = io.Copy(io.Discard, io.LimitReader(c.Reader, 4<<20))
	return c.cmd.Wait()
}

// validRunID restricts run IDs to safe filesystem + URL characters.
// The run engine generates IDs of the form <scenario>-<unix>; we
// further constrain to [a-z0-9-]+ to prevent path traversal.
func validRunID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func localPathFromLocator(root, locator string) (string, error) {
	if !strings.HasPrefix(locator, "file://") {
		return "", fmt.Errorf("not a file:// locator: %s", locator)
	}
	path := strings.TrimPrefix(locator, "file://")
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Refuse anything outside Root — defense against a poisoned
	// index row pointing at /etc/passwd.
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) && abs != root {
		return "", fmt.Errorf("manifest path escapes store root: %s", abs)
	}
	return abs, nil
}

func s3LocatorURI(bucket, locator string) (string, error) {
	if !strings.HasPrefix(locator, "s3://") {
		return "", fmt.Errorf("not an s3:// locator: %s", locator)
	}
	rest := strings.TrimPrefix(locator, "s3://")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[0] != bucket {
		return "", fmt.Errorf("s3 locator bucket mismatch (want %q, got %q)", bucket, parts[0])
	}
	return locator, nil
}
