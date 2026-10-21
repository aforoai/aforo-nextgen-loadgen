package seed

import (
	"crypto/ed25519"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/serviceauth"
)

var (
	testAuthOnce sync.Once
	testAuthKey  ed25519.PrivateKey
)

// testServiceAuth returns a Signer with a throwaway Ed25519 key, the
// credential a seed run needs for Aforo internal endpoints (G375).
func testServiceAuth(t *testing.T) *serviceauth.Signer {
	t.Helper()
	testAuthOnce.Do(func() {
		_, testAuthKey, _ = ed25519.GenerateKey(nil)
	})
	return serviceauth.NewSigner(serviceauth.Config{PrivateKey: testAuthKey, KeyID: "loadgen-test"},
		aforo.Target{}, log.New(io.Discard, "", 0))
}

// internalAuthViolation reports why an inbound request to the fake backend
// breaks the G375 contract, or "" when it is fine: internal endpoints need a
// service token; the fleet-wide shared key must never appear.
func internalAuthViolation(r *http.Request) string {
	if r.Header.Get(serviceauth.SharedKeyHeader) != "" {
		return "fleet-wide X-Internal-Api-Key sent"
	}
	if serviceauth.IsInternalPath(r.URL.Path) && r.Header.Get(serviceauth.Header) == "" {
		return "internal endpoint called without " + serviceauth.Header
	}
	if !serviceauth.IsInternalPath(r.URL.Path) && r.Header.Get(serviceauth.Header) != "" {
		return "service token sent to a non-internal path"
	}
	if tok := r.Header.Get(serviceauth.Header); tok != "" && strings.Count(tok, ".") != 2 {
		return "service token is not a compact JWS"
	}
	return ""
}
