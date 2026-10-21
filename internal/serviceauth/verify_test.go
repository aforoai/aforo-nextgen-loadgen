package serviceauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// javaRules re-implements, check for check, aforo-common's
// com.aforo.billing.common.serviceauth.ServiceTokenVerifier.verify
// (branch arch-fix/w1-common). Keep the order and the rules in step with it:
// compact JWS, alg=EdDSA, kid trusted, signature, exp/nbf with skew, iat not
// in the future, exp-iat <= 60 s, iss == owner of kid == sub, aud contains the
// service, iat/exp/jti present, tenant_id valid and not the sentinel, scope a
// string.
type javaRules struct {
	serviceName string
	trusted     map[string]trustedKey // by kid
	skew        time.Duration
	now         time.Time
}

type trustedKey struct {
	caller string
	key    ed25519.PublicKey
}

type verified struct {
	issuer   string
	tenantID string
	scopes   []string
	jti      string
}

var errReason = errors.New

var javaTenant = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func (v javaRules) verify(token string) (verified, error) {
	if len(v.trusted) == 0 {
		return verified{}, errReason("NO_TRUSTED_KEYS")
	}
	if strings.TrimSpace(token) == "" || len(token) > 8192 {
		return verified{}, errReason("MALFORMED")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return verified{}, errReason("MALFORMED")
	}
	b64 := base64.RawURLEncoding
	hdrBytes, err := b64.DecodeString(parts[0])
	if err != nil {
		return verified{}, errReason("MALFORMED")
	}
	var hdr map[string]any
	if json.Unmarshal(hdrBytes, &hdr) != nil {
		return verified{}, errReason("MALFORMED")
	}
	if hdr["alg"] != "EdDSA" {
		return verified{}, errReason("UNSUPPORTED_ALGORITHM")
	}
	kid, _ := hdr["kid"].(string)
	tk, ok := v.trusted[kid]
	if !ok {
		return verified{}, errReason("UNKNOWN_KEY")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(tk.key, []byte(parts[0]+"."+parts[1]), sig) {
		return verified{}, errReason("BAD_SIGNATURE")
	}
	bodyBytes, err := b64.DecodeString(parts[1])
	if err != nil {
		return verified{}, errReason("MALFORMED")
	}
	var c map[string]any
	if json.Unmarshal(bodyBytes, &c) != nil {
		return verified{}, errReason("MALFORMED")
	}
	num := func(k string) (int64, bool) {
		f, ok := c[k].(float64)
		return int64(f), ok
	}
	now := v.now
	if exp, ok := num("exp"); ok && now.After(time.Unix(exp, 0).Add(v.skew)) {
		return verified{}, errReason("EXPIRED")
	}
	if nbf, ok := num("nbf"); ok && now.Before(time.Unix(nbf, 0).Add(-v.skew)) {
		return verified{}, errReason("NOT_YET_VALID")
	}
	iss, _ := c["iss"].(string)
	if iss == "" || iss != tk.caller {
		return verified{}, errReason("ISSUER_MISMATCH")
	}
	if sub, _ := c["sub"].(string); sub != iss {
		return verified{}, errReason("ISSUER_MISMATCH")
	}
	audOK := false
	switch a := c["aud"].(type) {
	case string:
		audOK = a == v.serviceName
	case []any:
		for _, x := range a {
			if x == v.serviceName {
				audOK = true
			}
		}
	}
	if !audOK {
		return verified{}, errReason("WRONG_AUDIENCE")
	}
	iat, okI := num("iat")
	exp, okE := num("exp")
	jti, _ := c["jti"].(string)
	if !okI || !okE || strings.TrimSpace(jti) == "" {
		return verified{}, errReason("MISSING_CLAIM")
	}
	if time.Unix(iat, 0).After(now.Add(v.skew)) {
		return verified{}, errReason("NOT_YET_VALID")
	}
	if exp-iat > 60 {
		return verified{}, errReason("LIFETIME_TOO_LONG")
	}
	out := verified{issuer: iss, jti: jti}
	if raw, present := c["tenant_id"]; present && raw != nil {
		t, ok := raw.(string)
		if !ok || !javaTenant.MatchString(t) || t == "__system__" {
			return verified{}, errReason("INVALID_TENANT")
		}
		out.tenantID = t
	}
	if raw, present := c["scope"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return verified{}, errReason("MALFORMED")
		}
		out.scopes = strings.Fields(s)
	}
	return out, nil
}
