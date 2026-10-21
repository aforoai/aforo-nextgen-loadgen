package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/serviceauth"
)

func clearInternalAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{serviceauth.EnvPrivateKey, serviceauth.EnvKeyID, serviceauth.EnvAudiences,
		serviceauth.EnvSharedKey, serviceauth.EnvSendSharedKey} {
		t.Setenv(k, "")
	}
}

// G375 (4): a live seed with neither a token key nor the explicit fallback
// refuses to start — before any request is sent.
func TestSeed_G375_RefusesToStartWithoutInternalCredential(t *testing.T) {
	clearInternalAuthEnv(t)
	t.Setenv(serviceauth.EnvSharedKey, "fleet-shared") // present but not opted in: not enough
	var out, errOut bytes.Buffer
	err := runSeed(context.Background(), &out, &errOut, &seedFlags{target: "local", tokenEnv: "AFORO_ADMIN_TOKEN"})
	if !errors.Is(err, serviceauth.ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
	if strings.Contains(out.String()+errOut.String()+err.Error(), "fleet-shared") {
		t.Error("output leaks the shared key")
	}
}

func TestSeed_G375_RejectsHalfConfiguredKey(t *testing.T) {
	clearInternalAuthEnv(t)
	t.Setenv(serviceauth.EnvKeyID, "aforo-loadgen-1")
	var out, errOut bytes.Buffer
	err := runSeed(context.Background(), &out, &errOut, &seedFlags{target: "local", tokenEnv: "AFORO_ADMIN_TOKEN"})
	if err == nil || !strings.Contains(err.Error(), serviceauth.EnvPrivateKey) {
		t.Fatalf("want a startup error naming %s, got %v", serviceauth.EnvPrivateKey, err)
	}
}

func TestSeed_G375_DryRunNeedsNoCredential(t *testing.T) {
	clearInternalAuthEnv(t)
	if _, err := internalAuth(aforo.LocalTarget, nil, false, "seed"); err != nil {
		t.Fatalf("dry-run / optional use must not require a credential: %v", err)
	}
}

func TestSeedHelp_G375_DocumentsEnvVars(t *testing.T) {
	long := newSeedCommand(nil).Long
	for _, v := range []string{serviceauth.EnvPrivateKey, serviceauth.EnvKeyID, serviceauth.EnvSendSharedKey, serviceauth.EnvAudiences} {
		if !strings.Contains(long, v) {
			t.Errorf("seed --help does not mention %s", v)
		}
	}
}
