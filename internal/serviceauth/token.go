package serviceauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TokenTTL is the token lifetime; the targets reject exp - iat > 60 s.
const TokenTTL = 60 * time.Second

// RefreshMargin: a cached token is replaced once less than this remains.
const RefreshMargin = 10 * time.Second

// tokenHeader is the JOSE header. Field order is the wire order.
type tokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// TokenClaims is the token payload. Field order is the wire order.
type TokenClaims struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Aud      string `json:"aud"`
	Iat      int64  `json:"iat"`
	Exp      int64  `json:"exp"`
	Jti      string `json:"jti"`
	TenantID string `json:"tenant_id,omitempty"`
	Scope    string `json:"scope,omitempty"`
}

// MintParams are the inputs of one token.
type MintParams struct {
	Key      ed25519.PrivateKey
	KeyID    string
	Audience string
	TenantID string
	Scopes   []string
	Now      time.Time
	// JTI overrides the random token id (tests only).
	JTI string
}

// Mint builds and signs one compact JWS. It validates every input the target
// verifier would reject, so a bad call fails here instead of as a 401.
func Mint(p MintParams) (string, error) {
	if len(p.Key) != ed25519.PrivateKeySize {
		return "", errors.New("serviceauth: no Ed25519 signing key")
	}
	if !kidPattern.MatchString(p.KeyID) {
		return "", errors.New("serviceauth: invalid key id")
	}
	if !audiencePattern.MatchString(p.Audience) {
		return "", fmt.Errorf("serviceauth: invalid audience %q", p.Audience)
	}
	tenant := strings.TrimSpace(p.TenantID)
	if tenant != "" && (!tenantPattern.MatchString(tenant) || tenant == SystemTenantSentinel) {
		return "", fmt.Errorf("serviceauth: tenant id %q cannot be put in a service token", tenant)
	}
	scope, err := normalizeScopes(p.Scopes)
	if err != nil {
		return "", err
	}
	jti := p.JTI
	if jti == "" {
		if jti, err = newUUID(); err != nil {
			return "", err
		}
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	iat := now.Unix()
	hdr, err := json.Marshal(tokenHeader{Alg: Algorithm, Kid: p.KeyID, Typ: TokenType})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(TokenClaims{
		Iss: Issuer, Sub: Issuer, Aud: p.Audience,
		Iat: iat, Exp: iat + int64(TokenTTL/time.Second),
		Jti: jti, TenantID: tenant, Scope: scope,
	})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString(hdr) + "." + b64.EncodeToString(body)
	sig := ed25519.Sign(p.Key, []byte(signingInput))
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// normalizeScopes sorts and de-duplicates scopes (same as aforo-common's
// ServiceTokenMinter, so equal scope sets share one cached token).
func normalizeScopes(scopes []string) (string, error) {
	set := map[string]struct{}{}
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !scopePattern.MatchString(s) {
			return "", fmt.Errorf("serviceauth: invalid scope %q", s)
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, " "), nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("serviceauth: random jti: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
