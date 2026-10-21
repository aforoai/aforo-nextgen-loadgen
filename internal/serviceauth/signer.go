package serviceauth

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// DefaultAudiences maps loadgen's service keys to each target's
// spring.application.name (the aud the target verifies against). Verified
// 2026-10-21 against every service's application.yml; note usage-ingestor
// registers as "usage-ingestor-service".
var DefaultAudiences = map[aforo.Service]string{
	aforo.ServiceOrganization:  "organization-service",
	aforo.ServiceCatalog:       "catalog-service",
	aforo.ServicePricing:       "pricing-service",
	aforo.ServiceCustomer:      "customer-service",
	aforo.ServiceBilling:       "billing-service",
	aforo.ServiceUsageIngestor: "usage-ingestor-service",
	aforo.ServiceAnalytics:     "analytics-service",
	aforo.ServiceStorefront:    "storefront-service",
	aforo.ServiceAIService:     "ai-service",
}

// ErrNoCredential is returned, before anything is sent, for an internal
// request when neither a token key nor the explicit shared-key fallback is
// configured. Not retryable.
var ErrNoCredential = errors.New("no credential for Aforo internal endpoints: set " +
	EnvPrivateKey + " and " + EnvKeyID + " (loadgen's service-token key), or — transition only — " +
	EnvInternalFallbackHint)

// EnvInternalFallbackHint names the opt-in fallback in error messages.
const EnvInternalFallbackHint = EnvSendSharedKey + "=true with " + EnvSharedKey

// ErrAudienceUnknown is returned when an internal request's target service
// cannot be determined (e.g. a single-ingress target and no explicit service).
var ErrAudienceUnknown = errors.New("cannot tell which Aforo service this internal request is for")

const maxCachedTokens = 10_000

type cacheKey struct{ aud, tenant, scope string }

type cachedToken struct {
	token     string
	refreshAt time.Time
}

// Signer authenticates loadgen's requests to Aforo internal endpoints. Safe
// for concurrent use. A nil *Signer behaves like a Signer with no credential.
type Signer struct {
	cfg    Config
	target aforo.Target
	now    func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cachedToken
}

// NewSigner builds a Signer for target. logger (nil → log.Default()) receives
// the startup notices; key material is never logged.
func NewSigner(cfg Config, target aforo.Target, logger *log.Logger) *Signer {
	if logger == nil {
		logger = log.Default()
	}
	if cfg.SharedFallbackActive() {
		logger.Printf("WARN: %s=true — the fleet-wide X-Internal-Api-Key is sent on Aforo internal requests. "+
			"This is a transition fallback for targets still in service-auth SHADOW with RBAC on (their "+
			"InternalApiKeyFilter still requires the shared key). Turn it off once every target runs ENFORCE "+
			"and trusts %s.", EnvSendSharedKey, Issuer)
	} else if cfg.SharedKey != "" {
		logger.Printf("INFO: %s is set but not sent (set %s=true to send it during the transition).",
			EnvSharedKey, EnvSendSharedKey)
	}
	if cfg.CanMint() {
		logger.Printf("INFO: service tokens enabled for Aforo internal endpoints (iss=%s kid=%s)", Issuer, cfg.KeyID)
	}
	return &Signer{cfg: cfg, target: target, now: time.Now, cache: map[cacheKey]cachedToken{}}
}

// SetClock replaces the clock (tests).
func (s *Signer) SetClock(now func() time.Time) { s.now = now }

// HasCredential reports whether internal requests can be authenticated.
func (s *Signer) HasCredential() bool { return s != nil && s.cfg.HasCredential() }

// CanMint reports whether a token key is configured.
func (s *Signer) CanMint() bool { return s != nil && s.cfg.CanMint() }

// SendsSharedKey reports whether the opt-in shared-key fallback is active.
func (s *Signer) SendsSharedKey() bool { return s != nil && s.cfg.SharedFallbackActive() }

// Authorize adds loadgen's credential to req when req targets an Aforo
// internal endpoint, and does nothing otherwise (the shared key is never sent
// to non-internal paths). svc names the target service when the caller knows
// it; pass "" to resolve it from the URL. The token is bound to the request's
// X-Tenant-Id header (else its tenantId query parameter), so set those first.
func (s *Signer) Authorize(req *http.Request, svc aforo.Service) error {
	return s.AuthorizeScoped(req, svc, nil)
}

// AuthorizeScoped is Authorize with scopes added to the token.
func (s *Signer) AuthorizeScoped(req *http.Request, svc aforo.Service, scopes []string) error {
	if req == nil || req.URL == nil || !IsInternalPath(req.URL.Path) {
		return nil
	}
	if !s.HasCredential() {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, ErrNoCredential)
	}
	if s.cfg.CanMint() {
		aud, err := s.audience(req.URL, svc)
		if err != nil {
			return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
		}
		tok, err := s.Token(aud, tenantOf(req), scopes)
		if err != nil {
			return err
		}
		req.Header.Set(Header, tok)
	}
	if s.cfg.SharedFallbackActive() {
		req.Header.Set(SharedKeyHeader, s.cfg.SharedKey)
	}
	return nil
}

// Token returns a token for (audience, tenant, scopes), reusing a cached one
// until RefreshMargin before it expires.
func (s *Signer) Token(audience, tenant string, scopes []string) (string, error) {
	if !s.CanMint() {
		return "", ErrNoCredential
	}
	scope, err := normalizeScopes(scopes)
	if err != nil {
		return "", err
	}
	key := cacheKey{aud: audience, tenant: strings.TrimSpace(tenant), scope: scope}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[key]; ok && now.Before(c.refreshAt) {
		return c.token, nil
	}
	tok, err := Mint(MintParams{
		Key: s.cfg.PrivateKey, KeyID: s.cfg.KeyID, Audience: audience,
		TenantID: key.tenant, Scopes: scopes, Now: now,
	})
	if err != nil {
		return "", err
	}
	if len(s.cache) >= maxCachedTokens {
		s.cache = map[cacheKey]cachedToken{}
	}
	// Mint truncates iat to whole seconds; refresh relative to that.
	issued := time.Unix(now.Unix(), 0)
	s.cache[key] = cachedToken{token: tok, refreshAt: issued.Add(TokenTTL - RefreshMargin)}
	return tok, nil
}

// audience resolves the target service name: an explicit override for the
// destination wins, then the caller-supplied service, then a unique match of
// the request's base URL against the target's per-service URL map.
func (s *Signer) audience(u *url.URL, svc aforo.Service) (string, error) {
	best, bestScore := "", -1
	for _, r := range s.cfg.AudienceOverrides {
		if ok, score := r.matches(u); ok && score > bestScore {
			best, bestScore = r.Audience, score
		}
	}
	if best != "" {
		return best, nil
	}
	if svc != "" {
		if aud, ok := DefaultAudiences[svc]; ok {
			return aud, nil
		}
		return "", fmt.Errorf("%w: no audience known for service %q (set %s)", ErrAudienceUnknown, svc, EnvAudiences)
	}
	matches := map[string]struct{}{}
	for svcKey, base := range s.target.URLs {
		b, err := url.Parse(strings.TrimRight(base, "/"))
		if err != nil || b.Host == "" {
			continue
		}
		if strings.EqualFold(b.Scheme, u.Scheme) && hostPort(b) == hostPort(u) {
			if aud, ok := DefaultAudiences[svcKey]; ok {
				matches[aud] = struct{}{}
			}
		}
	}
	if len(matches) == 1 {
		for aud := range matches {
			return aud, nil
		}
	}
	return "", fmt.Errorf("%w: %s://%s matches %d services in target %q (set %s)",
		ErrAudienceUnknown, u.Scheme, u.Host, len(matches), s.target.Name, EnvAudiences)
}

// tenantOf returns the tenant the request acts for: the X-Tenant-Id header,
// else a single tenantId query parameter. Both are what the target compares
// the token's tenant_id against.
func tenantOf(req *http.Request) string {
	if t := strings.TrimSpace(req.Header.Get("X-Tenant-Id")); t != "" {
		return t
	}
	if vs := req.URL.Query()["tenantId"]; len(vs) == 1 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// NewSignerFromEnv reads the environment (see the Env* constants) and builds
// a Signer for target. A half-filled or unparsable configuration is an
// error, so commands refuse to start instead of failing mid-run.
func NewSignerFromEnv(getenv func(string) string, target aforo.Target, logger *log.Logger) (*Signer, error) {
	cfg, err := ConfigFromEnv(getenv)
	if err != nil {
		return nil, fmt.Errorf("internal endpoint credentials: %w", err)
	}
	return NewSigner(cfg, target, logger), nil
}

// RequireCredential returns ErrNoCredential (wrapped with what) when the
// Signer has no credential. Commands that always call internal endpoints use
// it to fail at startup.
func (s *Signer) RequireCredential(what string) error {
	if s.HasCredential() {
		return nil
	}
	return fmt.Errorf("%s calls Aforo internal endpoints: %w", what, ErrNoCredential)
}
