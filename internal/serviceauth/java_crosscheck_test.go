package serviceauth

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// TestJavaVerifierCrossCheck verifies loadgen tokens with aforo-common's real
// Java ServiceTokenVerifier. Runs only when AFORO_COMMON_DIR points at an
// aforo-nextgen-common checkout (and java/javac are installed); the
// dependency jars are read from ~/.m2. Example:
//
//	AFORO_COMMON_DIR=../aforo-nextgen-common go test ./internal/serviceauth -run JavaVerifier -v
func TestJavaVerifierCrossCheck(t *testing.T) {
	common := os.Getenv("AFORO_COMMON_DIR")
	if common == "" {
		t.Skip("AFORO_COMMON_DIR not set")
	}
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not installed")
	}
	script, err := filepath.Abs("../../scripts/serviceauth-java-crosscheck/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := NewSigner(testConfig(t), aforo.LocalTarget, quiet())
	s.SetClock(func() time.Time { return now })
	req := newReq(t, http.MethodPost, "http://localhost:8083/internal/v1/subscriptions/x/dunning-update", "lg-java-1")
	if err := s.Authorize(req, aforo.ServicePricing); err != nil {
		t.Fatal(err)
	}
	run := func(token, aud string, at time.Time) (string, error) {
		cmd := exec.Command(script, token, aud, testKID, testPublicKeyB64, strconv.FormatInt(at.Unix(), 10))
		cmd.Env = append(os.Environ(), "AFORO_COMMON_DIR="+common)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}

	out, err := run(req.Header.Get(Header), "pricing-service", now)
	if err != nil || !strings.HasPrefix(out, "OK iss=aforo-loadgen tenant=lg-java-1 ") {
		t.Fatalf("Java verifier rejected a loadgen token: %q (%v)", out, err)
	}
	if out, _ := run(goldenToken, "organization-service", time.Unix(goldenNow+5, 0)); out != "OK iss=aforo-loadgen tenant=lg-acme-01 scopes=platform,tenants:write jti="+goldenJTI {
		t.Errorf("golden vector: %q", out)
	}
	if out, _ := run(req.Header.Get(Header), "billing-service", now); out != "REJECTED WRONG_AUDIENCE" {
		t.Errorf("wrong audience: %q", out)
	}
	if out, _ := run(req.Header.Get(Header), "pricing-service", now.Add(2*time.Minute)); out != "REJECTED EXPIRED" {
		t.Errorf("expired: %q", out)
	}
}
