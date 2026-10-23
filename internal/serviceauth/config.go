// Package serviceauth gives loadgen its own credential for Aforo's internal
// endpoints (/internal/** and /api/v1/internal/**): a short-lived signed
// service token, as defined by aforo-common W1.5
// (com.aforo.billing.common.serviceauth, docs/service-auth.md).
//
// Before this package (gap G375) loadgen forwarded the fleet-wide
// X-Internal-Api-Key on every seed request. That key lets its holder call any
// internal endpoint of any service as any service, for any tenant, and cannot
// be revoked for loadgen alone. A service token is signed with loadgen's own
// Ed25519 key (iss = sub = "aforo-loadgen"), is addressed to one target
// service (aud), is bound to the request's tenant, and lives 60 s. A target
// accepts it only if DevOps listed loadgen's public key under
// aforo.service-auth.trusted.aforo-loadgen — which is done in test
// environments only. Removing that entry revokes loadgen everywhere.
package serviceauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Wire constants. They mirror com.aforo.billing.common.serviceauth.ServiceTokens.
const (
	// Header carries the token on the request.
	Header = "X-Aforo-Service-Token"
	// SharedKeyHeader is the legacy fleet-wide shared secret header
	// (aforo-common InternalApiKeyFilter). Sent only in the opt-in
	// transition fallback.
	SharedKeyHeader = "X-Internal-Api-Key"
	// TokenType is the JOSE typ header value.
	TokenType = "aforo-service+jwt"
	// Algorithm is the only accepted JWS algorithm (RFC 8037 EdDSA, Ed25519).
	Algorithm = "EdDSA"
	// Issuer is loadgen's service name: iss and sub of every token, and the
	// key under aforo.service-auth.trusted.<caller> on the targets.
	Issuer = "aforo-loadgen"
	// ClaimTenant is the optional tenant claim.
	ClaimTenant = "tenant_id"
	// ClaimScope is the optional space-separated scope claim.
	ClaimScope = "scope"
	// SystemTenantSentinel is reserved in-process on the targets and is never
	// valid on the wire.
	SystemTenantSentinel = "__system__"
)

// Environment variables read by ConfigFromEnv.
const (
	// EnvPrivateKey holds loadgen's Ed25519 private key: PKCS#8, PEM or the
	// bare base64 of the DER on one line (the form ServiceKeyTool prints as
	// AFORO_SERVICE_AUTH_PRIVATE_KEY). A literal "\n" is accepted.
	EnvPrivateKey = "AFORO_LOADGEN_SERVICE_PRIVATE_KEY"
	// EnvKeyID is the key id ServiceKeyTool printed with the pair
	// (AFORO_SERVICE_AUTH_KEY_ID).
	EnvKeyID = "AFORO_LOADGEN_SERVICE_KEY_ID"
	// EnvAudiences optionally overrides the audience for a destination:
	// comma-separated "<url-or-host>=<service-name>" entries, e.g.
	// "http://localhost:8084=usage-ingestor-service,gw.test.internal=pricing-service".
	// Needed only when a target's spring.application.name (or
	// aforo.service-auth.service-name) differs from the defaults in
	// DefaultAudiences.
	EnvAudiences = "AFORO_LOADGEN_SERVICE_AUDIENCES"
	// EnvSharedKey is the legacy fleet-wide internal key.
	EnvSharedKey = "AFORO_INTERNAL_API_KEY"
	// EnvSendSharedKey opts in to also sending EnvSharedKey on internal
	// requests (transition fallback for targets still in SHADOW with RBAC on).
	EnvSendSharedKey = "AFORO_LOADGEN_SEND_SHARED_INTERNAL_KEY"
)

var (
	kidPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	audiencePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	scopePattern    = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
	tenantPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

// Config is loadgen's service-auth configuration. The zero value has no
// credential: every internal request then fails before it is sent.
type Config struct {
	// PrivateKey signs tokens. Nil means "no token key".
	PrivateKey ed25519.PrivateKey
	// KeyID is the kid header; required with PrivateKey.
	KeyID string
	// AudienceOverrides map a destination (URL prefix or bare host) to the
	// target's service name. Checked before the per-service defaults.
	AudienceOverrides []AudienceRule
	// SharedKey is the legacy fleet-wide internal key. Sent only when
	// SendSharedKey is true.
	SharedKey string
	// SendSharedKey opts in to the transition fallback.
	SendSharedKey bool
}

// CanMint reports whether a token key is configured.
func (c Config) CanMint() bool { return len(c.PrivateKey) == ed25519.PrivateKeySize && c.KeyID != "" }

// SharedFallbackActive reports whether the legacy key will be sent.
func (c Config) SharedFallbackActive() bool { return c.SendSharedKey && c.SharedKey != "" }

// HasCredential reports whether internal requests can be authenticated at all.
func (c Config) HasCredential() bool { return c.CanMint() || c.SharedFallbackActive() }

// String never prints key material.
func (c Config) String() string {
	return fmt.Sprintf("serviceauth.Config{token:%t kid:%q overrides:%d sharedKeySet:%t sendSharedKey:%t}",
		c.CanMint(), c.KeyID, len(c.AudienceOverrides), c.SharedKey != "", c.SendSharedKey)
}

// GoString keeps %#v from dumping the private key.
func (c Config) GoString() string { return c.String() }

// ConfigFromEnv reads the configuration through getenv (os.Getenv in
// production). It returns an error for a half-filled or unparsable
// configuration so the CLI can refuse to start; error messages never contain
// key material.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	var cfg Config
	rawKey := strings.TrimSpace(getenv(EnvPrivateKey))
	kid := strings.TrimSpace(getenv(EnvKeyID))
	switch {
	case rawKey != "" && kid == "":
		return Config{}, fmt.Errorf("%s is set but %s is empty: both are required", EnvPrivateKey, EnvKeyID)
	case rawKey == "" && kid != "":
		return Config{}, fmt.Errorf("%s is set but %s is empty: both are required", EnvKeyID, EnvPrivateKey)
	case rawKey != "":
		if !kidPattern.MatchString(kid) {
			return Config{}, fmt.Errorf("%s may contain only letters, digits, '.', '_' and '-' (1-128 chars)", EnvKeyID)
		}
		key, err := ParsePrivateKey(rawKey)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvPrivateKey, err)
		}
		cfg.PrivateKey = key
		cfg.KeyID = kid
	}

	rules, err := ParseAudienceRules(getenv(EnvAudiences))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvAudiences, err)
	}
	cfg.AudienceOverrides = rules

	cfg.SharedKey = strings.TrimSpace(getenv(EnvSharedKey))
	if raw := strings.TrimSpace(getenv(EnvSendSharedKey)); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", EnvSendSharedKey)
		}
		cfg.SendSharedKey = b
	}
	if cfg.SendSharedKey && cfg.SharedKey == "" {
		return Config{}, fmt.Errorf("%s=true but %s is empty", EnvSendSharedKey, EnvSharedKey)
	}
	return cfg, nil
}

// ParsePrivateKey parses a PKCS#8 Ed25519 private key given as PEM or as the
// bare base64 of the DER (aforo-common ServiceKeys.parsePrivateKey accepts
// the same forms). Literal "\n" sequences are tolerated.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	der, err := decodeKey(text, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("not a PKCS#8 private key")
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return priv, nil
}

// ParsePublicKey parses an X.509 SubjectPublicKeyInfo Ed25519 public key
// (PEM or bare base64 of the DER).
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	der, err := decodeKey(text, "PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("not an X.509 public key")
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return pub, nil
}

func decodeKey(text, pemType string) ([]byte, error) {
	s := strings.TrimSpace(strings.ReplaceAll(text, `\n`, "\n"))
	if s == "" {
		return nil, errors.New("key is empty")
	}
	if strings.HasPrefix(s, "-----BEGIN") {
		block, _ := pem.Decode([]byte(s))
		if block == nil || block.Type != pemType {
			return nil, fmt.Errorf("not a PEM %q block", pemType)
		}
		return block.Bytes, nil
	}
	compact := strings.Join(strings.Fields(s), "")
	der, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, errors.New("key is not valid base64 or PEM")
	}
	return der, nil
}

// AudienceRule maps a destination to a target service name.
type AudienceRule struct {
	// Host matches the request host on any scheme/port (bare-host entry).
	Host string
	// Prefix matches scheme://host[:port][/path...] (URL entry).
	Prefix *url.URL
	// Audience is the target's service name.
	Audience string
}

func (r AudienceRule) matches(u *url.URL) (bool, int) {
	if r.Prefix == nil {
		if strings.EqualFold(u.Hostname(), r.Host) {
			return true, len(r.Host)
		}
		return false, 0
	}
	if !strings.EqualFold(u.Scheme, r.Prefix.Scheme) || !strings.EqualFold(hostPort(u), hostPort(r.Prefix)) {
		return false, 0
	}
	p := strings.TrimRight(r.Prefix.Path, "/")
	if p != "" && u.Path != p && !strings.HasPrefix(u.Path, p+"/") {
		return false, 0
	}
	return true, len(r.Prefix.Scheme) + 3 + len(hostPort(r.Prefix)) + len(p) + 1000
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// ParseAudienceRules parses EnvAudiences. Empty input yields no rules.
func ParseAudienceRules(raw string) ([]AudienceRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var rules []AudienceRule
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		i := strings.LastIndex(entry, "=")
		if i <= 0 || i == len(entry)-1 {
			return nil, fmt.Errorf("entry %q must be <url-or-host>=<service-name>", entry)
		}
		dest, aud := strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
		if !audiencePattern.MatchString(aud) {
			return nil, fmt.Errorf("service name %q is not valid", aud)
		}
		rule := AudienceRule{Audience: aud}
		if strings.Contains(dest, "://") {
			u, err := url.Parse(dest)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("destination %q is not a valid URL", dest)
			}
			rule.Prefix = u
		} else {
			if strings.ContainsAny(dest, "/:") {
				return nil, fmt.Errorf("destination %q: use a bare host or a full URL with scheme", dest)
			}
			rule.Host = strings.ToLower(dest)
		}
		rules = append(rules, rule)
	}
	// Most specific first: URL rules before host rules, longer before shorter.
	sort.SliceStable(rules, func(a, b int) bool {
		return specificity(rules[a]) > specificity(rules[b])
	})
	return rules, nil
}

func specificity(r AudienceRule) int {
	if r.Prefix == nil {
		return len(r.Host)
	}
	return 1000 + len(r.Prefix.String())
}

// IsInternalPath reports whether an URL path is an Aforo internal endpoint
// (both conventions, as aforo-common InternalApiKeyFilter and
// ServiceTokenAuthFilter match them).
func IsInternalPath(p string) bool {
	return hasSegmentPrefix(p, "/internal") || hasSegmentPrefix(p, "/api/v1/internal")
}

func hasSegmentPrefix(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
