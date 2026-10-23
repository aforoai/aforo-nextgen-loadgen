package cli

import (
	"io"
	"log"
	"os"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/serviceauth"
)

// internalCredentialsHelp is appended to the help of every command that
// calls Aforo internal endpoints.
const internalCredentialsHelp = `
Internal endpoint credentials (G375):
  Requests to Aforo internal endpoints (/internal/**, /api/v1/internal/**)
  carry loadgen's own signed service token (X-Aforo-Service-Token,
  iss=aforo-loadgen, 60 s, bound to the target service and tenant).
    ` + serviceauth.EnvPrivateKey + `  Ed25519 private key (PKCS#8, PEM or base64 DER)
    ` + serviceauth.EnvKeyID + `       key id printed with it by ServiceKeyTool
    ` + serviceauth.EnvAudiences + `    optional "<url-or-host>=<service-name>,..." overrides
  Transition fallback only (targets still in SHADOW with RBAC on):
    ` + serviceauth.EnvSharedKey + ` + ` + serviceauth.EnvSendSharedKey + `=true
  The shared key is never sent unless that flag is true, and never to
  non-internal paths.
`

// internalAuth builds loadgen's internal-endpoint credential from the
// environment. With required=true, a run that has neither a token key nor
// the explicit shared-key fallback is refused at startup.
func internalAuth(target aforo.Target, errOut io.Writer, required bool, command string) (*serviceauth.Signer, error) {
	if errOut == nil {
		errOut = os.Stderr
	}
	s, err := serviceauth.NewSignerFromEnv(os.Getenv, target, log.New(errOut, "", 0))
	if err != nil {
		return nil, err
	}
	if required {
		if err := s.RequireCredential(command); err != nil {
			return nil, err
		}
	}
	return s, nil
}
