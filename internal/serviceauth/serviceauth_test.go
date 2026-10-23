package serviceauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// Test-only key pair: Ed25519 seed 0x00..0x1f. Never used outside tests.
// Encoded exactly as aforo-common's ServiceKeyTool prints keys (bare base64
// of the PKCS#8 / X.509 DER).
const (
	testPrivateKeyB64 = "MC4CAQAwBQYDK2VwBCIEIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
	testPublicKeyB64  = "MCowBQYDK2VwAyEAA6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	testKID           = "aforo-loadgen-20261021"
)

// Golden vector: header + payload layout from aforo-common docs/service-auth.md.
const (
	goldenNow   = int64(1_800_000_000)
	goldenJTI   = "3f2b8c1e-7a4d-4e5f-9b0a-1c2d3e4f5a6b"
	goldenHdr   = `{"alg":"EdDSA","kid":"aforo-loadgen-20261021","typ":"aforo-service+jwt"}`
	goldenBody  = `{"iss":"aforo-loadgen","sub":"aforo-loadgen","aud":"organization-service","iat":1800000000,"exp":1800000060,"jti":"3f2b8c1e-7a4d-4e5f-9b0a-1c2d3e4f5a6b","tenant_id":"lg-acme-01","scope":"platform tenants:write"}`
	goldenToken = "eyJhbGciOiJFZERTQSIsImtpZCI6ImFmb3JvLWxvYWRnZW4tMjAyNjEwMjEiLCJ0eXAiOiJhZm9yby1zZXJ2aWNlK2p3dCJ9.eyJpc3MiOiJhZm9yby1sb2FkZ2VuIiwic3ViIjoiYWZvcm8tbG9hZGdlbiIsImF1ZCI6Im9yZ2FuaXphdGlvbi1zZXJ2aWNlIiwiaWF0IjoxODAwMDAwMDAwLCJleHAiOjE4MDAwMDAwNjAsImp0aSI6IjNmMmI4YzFlLTdhNGQtNGU1Zi05YjBhLTFjMmQzZTRmNWE2YiIsInRlbmFudF9pZCI6ImxnLWFjbWUtMDEiLCJzY29wZSI6InBsYXRmb3JtIHRlbmFudHM6d3JpdGUifQ.ZtgySpmL4azxG6IGO7L9eM68e00Kgg8yge4mCZzAKkojyr1up06WAiVvlgYnrWmZyjfV-__oeDkh2PvE5G88Aw" // verified OK by aforo-common ServiceTokenVerifier (run.sh), 2026-10-21
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	k, err := ParsePrivateKey(testPrivateKeyB64)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return k
}

func testRules(t *testing.T, service string, now time.Time) javaRules {
	t.Helper()
	pub, err := ParsePublicKey(testPublicKeyB64)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	return javaRules{
		serviceName: service,
		trusted:     map[string]trustedKey{testKID: {caller: Issuer, key: pub}},
		skew:        30 * time.Second,
		now:         now,
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := ConfigFromEnv(env(map[string]string{EnvPrivateKey: testPrivateKeyB64, EnvKeyID: testKID}))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func TestMint_GoldenVector(t *testing.T) {
	tok, err := Mint(MintParams{
		Key: testKey(t), KeyID: testKID, Audience: "organization-service",
		TenantID: "lg-acme-01", Scopes: []string{"tenants:write", "platform", "platform"},
		Now: time.Unix(goldenNow, 0), JTI: goldenJTI,
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %d parts", len(parts))
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if string(hdr) != goldenHdr {
		t.Errorf("header\n got %s\nwant %s", hdr, goldenHdr)
	}
	if string(body) != goldenBody {
		t.Errorf("payload\n got %s\nwant %s", body, goldenBody)
	}
	if strings.ContainsAny(tok, "=+/") {
		t.Error("token must be unpadded base64url")
	}
	if tok != goldenToken {
		t.Errorf("token changed (Ed25519 is deterministic):\n got %s\nwant %s", tok, goldenToken)
	}
}

func TestMint_VerifiesUnderJavaRules(t *testing.T) {
	now := time.Unix(goldenNow, 0)
	tok, err := Mint(MintParams{Key: testKey(t), KeyID: testKID, Audience: "pricing-service", TenantID: "t1", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	got, err := testRules(t, "pricing-service", now).verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.issuer != Issuer || got.tenantID != "t1" || len(got.jti) != 36 || len(got.scopes) != 0 {
		t.Errorf("unexpected claims: %+v", got)
	}
	// Each rule the target enforces.
	cases := []struct {
		name   string
		rules  func(javaRules) javaRules
		reason string
	}{
		{"wrong audience", func(r javaRules) javaRules { r.serviceName = "billing-service"; return r }, "WRONG_AUDIENCE"},
		{"expired beyond skew", func(r javaRules) javaRules { r.now = now.Add(91 * time.Second); return r }, "EXPIRED"},
		{"iat in the future", func(r javaRules) javaRules { r.now = now.Add(-31 * time.Second); return r }, "NOT_YET_VALID"},
		{"untrusted kid", func(r javaRules) javaRules { r.trusted = map[string]trustedKey{"other": r.trusted[testKID]}; return r }, "UNKNOWN_KEY"},
		{"kid owned by another caller", func(r javaRules) javaRules {
			k := r.trusted[testKID]
			k.caller = "organization-service"
			r.trusted = map[string]trustedKey{testKID: k}
			return r
		}, "ISSUER_MISMATCH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.rules(testRules(t, "pricing-service", now)).verify(tok)
			if err == nil || err.Error() != c.reason {
				t.Errorf("want %s, got %v", c.reason, err)
			}
		})
	}
	t.Run("tampered payload", func(t *testing.T) {
		p := strings.Split(tok, ".")
		body, _ := base64.RawURLEncoding.DecodeString(p[1])
		body = bytes.Replace(body, []byte(`"t1"`), []byte(`"t2"`), 1)
		forged := p[0] + "." + base64.RawURLEncoding.EncodeToString(body) + "." + p[2]
		if _, err := testRules(t, "pricing-service", now).verify(forged); err == nil || err.Error() != "BAD_SIGNATURE" {
			t.Errorf("want BAD_SIGNATURE, got %v", err)
		}
	})
}

func TestMint_RejectsWhatTargetsReject(t *testing.T) {
	k := testKey(t)
	for name, p := range map[string]MintParams{
		"sentinel tenant": {Key: k, KeyID: testKID, Audience: "pricing-service", TenantID: SystemTenantSentinel},
		"bad tenant":      {Key: k, KeyID: testKID, Audience: "pricing-service", TenantID: "a b"},
		"long tenant":     {Key: k, KeyID: testKID, Audience: "pricing-service", TenantID: strings.Repeat("x", 65)},
		"bad audience":    {Key: k, KeyID: testKID, Audience: "pricing service"},
		"bad scope":       {Key: k, KeyID: testKID, Audience: "pricing-service", Scopes: []string{"a b"}},
		"no key":          {KeyID: testKID, Audience: "pricing-service"},
		"bad kid":         {Key: k, KeyID: "kid with space", Audience: "pricing-service"},
	} {
		if _, err := Mint(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\\n" + testPrivateKeyB64 + "\\n-----END PRIVATE KEY-----\\n"
	for name, tc := range map[string]struct {
		env       map[string]string
		wantErr   string
		mint      bool
		sendShare bool
	}{
		"empty":            {env: map[string]string{}},
		"base64 key":       {env: map[string]string{EnvPrivateKey: testPrivateKeyB64, EnvKeyID: testKID}, mint: true},
		"pem with \\n":     {env: map[string]string{EnvPrivateKey: pem, EnvKeyID: testKID}, mint: true},
		"key without kid":  {env: map[string]string{EnvPrivateKey: testPrivateKeyB64}, wantErr: EnvKeyID},
		"kid without key":  {env: map[string]string{EnvKeyID: testKID}, wantErr: EnvPrivateKey},
		"garbage key":      {env: map[string]string{EnvPrivateKey: "not-a-key!!", EnvKeyID: testKID}, wantErr: "base64"},
		"public as priv":   {env: map[string]string{EnvPrivateKey: testPublicKeyB64, EnvKeyID: testKID}, wantErr: "PKCS#8"},
		"shared not sent":  {env: map[string]string{EnvSharedKey: "s3cr3t"}},
		"shared opt in":    {env: map[string]string{EnvSharedKey: "s3cr3t", EnvSendSharedKey: "true"}, sendShare: true},
		"opt in no key":    {env: map[string]string{EnvSendSharedKey: "true"}, wantErr: EnvSharedKey},
		"opt in not bool":  {env: map[string]string{EnvSharedKey: "x", EnvSendSharedKey: "yes please"}, wantErr: EnvSendSharedKey},
		"bad audience map": {env: map[string]string{EnvAudiences: "localhost:8086=org"}, wantErr: "bare host"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ConfigFromEnv(env(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				if strings.Contains(err.Error(), testPrivateKeyB64) {
					t.Fatal("error leaks key material")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CanMint() != tc.mint || cfg.SharedFallbackActive() != tc.sendShare {
				t.Errorf("mint=%t share=%t", cfg.CanMint(), cfg.SharedFallbackActive())
			}
			for _, s := range []string{cfg.String(), fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg)} {
				if strings.Contains(s, "s3cr3t") || strings.Contains(s, testPrivateKeyB64) {
					t.Errorf("formatted config leaks secrets: %s", s)
				}
			}
		})
	}
}

func TestIsInternalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/internal/v1/subscriptions/past-due": true,
		"/api/v1/internal/tenants":            true,
		"/api/v1/internal":                    true,
		"/internal":                           true,
		"/api/v1/internalish":                 false,
		"/internals/x":                        false,
		"/api/v1/products":                    false,
		"/v1/ingest/batch":                    false,
	} {
		if IsInternalPath(p) != want {
			t.Errorf("%s: want %t", p, want)
		}
	}
}

func newReq(t *testing.T, method, rawURL, tenant string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, rawURL, nil)
	if tenant != "" {
		r.Header.Set("X-Tenant-Id", tenant)
	}
	return r
}

func TestSigner_TokenOnInternalOnly_SharedKeyNotSentByDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.SharedKey = "fleet-shared"
	s := NewSigner(cfg, aforo.LocalTarget, quiet())
	now := time.Unix(goldenNow, 0)
	s.SetClock(func() time.Time { return now })

	pub := newReq(t, http.MethodGet, "http://localhost:8086/api/v1/products", "")
	if err := s.Authorize(pub, aforo.ServiceOrganization); err != nil {
		t.Fatal(err)
	}
	if pub.Header.Get(Header) != "" || pub.Header.Get(SharedKeyHeader) != "" {
		t.Error("non-internal request must carry neither credential")
	}

	in := newReq(t, http.MethodPost, "http://localhost:8086/api/v1/internal/tenants", "lg-1")
	if err := s.Authorize(in, aforo.ServiceOrganization); err != nil {
		t.Fatal(err)
	}
	if in.Header.Get(SharedKeyHeader) != "" {
		t.Error("shared key sent without opt-in")
	}
	got, err := testRules(t, "organization-service", now).verify(in.Header.Get(Header))
	if err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
	if got.tenantID != "lg-1" {
		t.Errorf("tenant_id = %q, want lg-1 (bound to X-Tenant-Id)", got.tenantID)
	}
}

func TestSigner_SharedFallbackOptIn(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{EnvSharedKey: "fleet-shared", EnvSendSharedKey: "true"}))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := NewSigner(cfg, aforo.LocalTarget, log.New(&logs, "", 0))
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "transition fallback") {
		t.Errorf("expected a WARN about the transition fallback, got %q", logs.String())
	}
	if strings.Contains(logs.String(), "fleet-shared") {
		t.Error("log leaks the shared key")
	}
	r := newReq(t, http.MethodGet, "http://localhost:8083/internal/v1/subscriptions/past-due", "t")
	if err := s.Authorize(r, aforo.ServicePricing); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get(SharedKeyHeader) != "fleet-shared" || r.Header.Get(Header) != "" {
		t.Errorf("fallback-only config: headers %v", r.Header)
	}
	pub := newReq(t, http.MethodGet, "http://localhost:8083/api/v1/rate-plans", "t")
	_ = s.Authorize(pub, aforo.ServicePricing)
	if pub.Header.Get(SharedKeyHeader) != "" {
		t.Error("shared key must never go to non-internal paths")
	}
}

func TestSigner_NoCredentialFailsBeforeSend(t *testing.T) {
	for _, s := range []*Signer{nil, NewSigner(Config{SharedKey: "set-but-not-opted-in"}, aforo.LocalTarget, quiet())} {
		r := newReq(t, http.MethodGet, "http://localhost:8086/api/v1/internal/tenants", "")
		err := s.Authorize(r, aforo.ServiceOrganization)
		if !errors.Is(err, ErrNoCredential) {
			t.Errorf("want ErrNoCredential, got %v", err)
		}
		if r.Header.Get(SharedKeyHeader) != "" {
			t.Error("must not fall back silently")
		}
		ok := newReq(t, http.MethodGet, "http://localhost:8086/api/v1/products", "")
		if err := s.Authorize(ok, aforo.ServiceOrganization); err != nil {
			t.Errorf("non-internal request must not need a credential: %v", err)
		}
	}
}

func TestSigner_CachePerAudienceTenantScope_RefreshTenSecondsBeforeExpiry(t *testing.T) {
	s := NewSigner(testConfig(t), aforo.LocalTarget, quiet())
	now := time.Unix(goldenNow, 0)
	s.SetClock(func() time.Time { return now })

	a1, _ := s.Token("pricing-service", "t1", nil)
	a2, _ := s.Token("pricing-service", "t1", nil)
	if a1 != a2 {
		t.Error("same (aud, tenant, scope) must reuse the token")
	}
	b, _ := s.Token("pricing-service", "t2", nil)
	c, _ := s.Token("billing-service", "t1", nil)
	d, _ := s.Token("pricing-service", "t1", []string{"platform"})
	if b == a1 || c == a1 || d == a1 {
		t.Error("different tenant / audience / scope must get a different token")
	}
	now = now.Add(49 * time.Second)
	if a3, _ := s.Token("pricing-service", "t1", nil); a3 != a1 {
		t.Error("token reused until 10 s before expiry")
	}
	now = now.Add(1 * time.Second) // 50 s after issue = 10 s before exp
	a4, _ := s.Token("pricing-service", "t1", nil)
	if a4 == a1 {
		t.Error("token must be re-minted 10 s before expiry")
	}
	if _, err := testRules(t, "pricing-service", now).verify(a4); err != nil {
		t.Errorf("refreshed token: %v", err)
	}
}

func TestSigner_AudienceResolution(t *testing.T) {
	now := time.Unix(goldenNow, 0)
	aud := func(t *testing.T, s *Signer, rawURL string, svc aforo.Service) (string, error) {
		t.Helper()
		s.SetClock(func() time.Time { return now })
		r := newReq(t, http.MethodGet, rawURL, "")
		if err := s.Authorize(r, svc); err != nil {
			return "", err
		}
		for _, name := range []string{"organization-service", "usage-ingestor-service", "pricing-service", "custom-org"} {
			if _, err := testRules(t, name, now).verify(r.Header.Get(Header)); err == nil {
				return name, nil
			}
		}
		return "", errors.New("token verifies for none of the expected audiences")
	}

	local := NewSigner(testConfig(t), aforo.LocalTarget, quiet())
	if got, _ := aud(t, local, "http://localhost:8084/internal/v1/x", ""); got != "usage-ingestor-service" {
		t.Errorf("url match on local target: %q", got)
	}
	if got, _ := aud(t, local, "http://localhost:8086/api/v1/internal/tenants", aforo.ServiceOrganization); got != "organization-service" {
		t.Errorf("explicit service: %q", got)
	}

	gw, _ := aforo.ResolveTarget("http://gateway.test:8000")
	shared := NewSigner(testConfig(t), gw, quiet())
	if _, err := aud(t, shared, "http://gateway.test:8000/internal/v1/x", ""); !errors.Is(err, ErrAudienceUnknown) {
		t.Errorf("single ingress without service: want ErrAudienceUnknown, got %v", err)
	}
	if got, _ := aud(t, shared, "http://gateway.test:8000/internal/v1/x", aforo.ServicePricing); got != "pricing-service" {
		t.Errorf("single ingress with explicit service: %q", got)
	}

	cfg := testConfig(t)
	cfg.AudienceOverrides, _ = ParseAudienceRules("localhost=pricing-service, http://localhost:8086=custom-org")
	ov := NewSigner(cfg, aforo.LocalTarget, quiet())
	if got, _ := aud(t, ov, "http://localhost:8086/api/v1/internal/tenants", aforo.ServiceOrganization); got != "custom-org" {
		t.Errorf("URL override must beat host override and service default: %q", got)
	}
	if got, _ := aud(t, ov, "http://localhost:9999/internal/v1/x", aforo.ServiceOrganization); got != "pricing-service" {
		t.Errorf("host override: %q", got)
	}
}

func TestSigner_TenantFromQueryParamWhenNoHeader(t *testing.T) {
	now := time.Unix(goldenNow, 0)
	s := NewSigner(testConfig(t), aforo.LocalTarget, quiet())
	s.SetClock(func() time.Time { return now })
	r := newReq(t, http.MethodGet, "http://localhost:8083/internal/v1/x?tenantId=q1", "")
	if err := s.Authorize(r, aforo.ServicePricing); err != nil {
		t.Fatal(err)
	}
	got, err := testRules(t, "pricing-service", now).verify(r.Header.Get(Header))
	if err != nil || got.tenantID != "q1" {
		t.Errorf("got %+v err %v", got, err)
	}
	bad := newReq(t, http.MethodGet, "http://localhost:8083/internal/v1/x", SystemTenantSentinel)
	if err := s.Authorize(bad, aforo.ServicePricing); err == nil {
		t.Error("sentinel tenant must be refused before sending")
	}
}
