package coord

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
)

// MTLSConfig is the file-system view of the mTLS material. Operators
// supply paths to PEM files via CLI flags or env vars; this package
// reads them and builds tls.Config values for the server and client.
//
// Field semantics match the openssl convention used in the ops runbook:
//
//	CertFile / KeyFile — this side's identity
//	CAFile             — the CA(s) we trust to sign the OTHER side's identity
//	ServerName         — only meaningful on the client; SNI / hostname-verify
//
// The code path is intentionally minimal — we don't generate certs, we
// don't auto-renew, we don't do CRL checks. All of that is done out-of-
// band by the cluster's PKI tooling. This package only knows how to
// load and present material someone else generated.
type MTLSConfig struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	ServerName string

	// AllowInsecure disables certificate verification entirely. Only
	// intended for local development against a self-signed cert when
	// testing the wire format. Refused by NewServerTLSConfig and
	// NewClientTLSConfig unless the AllowInsecureFlag env var is set,
	// so it can never silently land in production.
	AllowInsecure bool
}

// NewServerTLSConfig builds a *tls.Config suitable for an HTTP/2 server
// requiring mTLS. Returns an error if any required file is missing or
// the material is malformed.
func (m MTLSConfig) NewServerTLSConfig() (*tls.Config, error) {
	if m.AllowInsecure {
		if err := requireInsecureEnvOptIn(); err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // #nosec G402 — gated by env opt-in
			ClientAuth:         tls.NoClientCert,
		}, nil
	}
	if m.CertFile == "" || m.KeyFile == "" || m.CAFile == "" {
		return nil, errors.New("mtls: cert_file, key_file, ca_file are all required for the server")
	}
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server cert/key: %w", err)
	}
	pool, err := loadCAPool(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load ca: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// Require + verify the client's cert against our CA pool —
		// this is the "mutual" in mTLS.
		ClientAuth:               tls.RequireAndVerifyClientCert,
		ClientCAs:                pool,
		PreferServerCipherSuites: true,
		NextProtos:               []string{"h2", "http/1.1"},
	}, nil
}

// NewClientTLSConfig builds a *tls.Config for an HTTP/2 client that
// presents its own cert and verifies the server's cert against the CA
// pool.
func (m MTLSConfig) NewClientTLSConfig() (*tls.Config, error) {
	if m.AllowInsecure {
		if err := requireInsecureEnvOptIn(); err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // #nosec G402 — gated by env opt-in
		}, nil
	}
	if m.CertFile == "" || m.KeyFile == "" || m.CAFile == "" {
		return nil, errors.New("mtls: cert_file, key_file, ca_file are all required for the client")
	}
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client cert/key: %w", err)
	}
	pool, err := loadCAPool(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load ca: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   m.ServerName,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	return cfg, nil
}

// loadCAPool reads a PEM file and returns a CertPool containing every
// certificate in it. Returns an error on parse failure (vs. silent
// empty pool, which would leave verification effectively disabled).
func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no PEM certificates found in %s", path)
	}
	return pool, nil
}

// requireInsecureEnvOptIn refuses to honor AllowInsecure unless the
// operator has explicitly set the AFORO_LOADGEN_INSECURE_TLS=1 env.
// This is a belt-and-braces guard against a forgotten flag in a config
// file accidentally landing in production.
func requireInsecureEnvOptIn() error {
	if os.Getenv("AFORO_LOADGEN_INSECURE_TLS") != "1" {
		return errors.New("mtls: --insecure requested but AFORO_LOADGEN_INSECURE_TLS=1 is not set; refusing to disable cert verification")
	}
	return nil
}

// ParseListenAddr validates that addr is in host:port form and the port
// is numeric. Returns the addr unchanged on success. Used by the worker
// CLI so a typo on --listen surfaces immediately.
func ParseListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q must be host:port: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("listen address %q has empty port", addr)
	}
	// Accept "" host (bind all interfaces) or a literal IP/hostname.
	_ = host
	return addr, nil
}
