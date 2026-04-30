package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Authenticator validates a Supabase JWT and resolves the caller's
// internal Control Tower role. Two implementations:
//
//   - SupabaseAuthenticator: real round-trip to /auth/v1/user + a
//     PostgREST GET on internal_roles.
//   - StaticAuthenticator: dev-only fixed identity; used by tests
//     and by `--allow-anonymous` for local development.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (*Identity, error)
}

// Identity is the resolved caller. role is the internal_roles.role value;
// "platform_admin" is the only role that can trigger runs in v1.
type Identity struct {
	UserID string
	Email  string
	Role   string
}

// IsPlatformAdmin returns true if the identity can trigger and cancel
// runs. Read access is broader (any authenticated internal role).
func (i *Identity) IsPlatformAdmin() bool {
	return i != nil && i.Role == "platform_admin"
}

// IsInternal returns true if the identity has any Control Tower role.
// Used for read-side endpoints (list, detail, scenarios).
func (i *Identity) IsInternal() bool {
	if i == nil {
		return false
	}
	switch i.Role {
	case "platform_admin", "support_agent", "finance_viewer", "content_moderator":
		return true
	}
	return false
}

// SupabaseAuthenticator is the production implementation. It calls
// Supabase's /auth/v1/user to validate the JWT (transparently handles
// HS256 + RS256), then PostgREST to read the user's internal role.
//
// The transport is the stdlib http client with a 5s per-request
// timeout. Service-role key is required for the internal_roles read
// because RLS hides that table from anon callers.
type SupabaseAuthenticator struct {
	URL            string
	AnonKey        string
	ServiceRoleKey string
	HTTPClient     *http.Client
}

// NewSupabaseAuthenticator builds a configured authenticator. Both
// keys must be non-empty in production; tests pass StaticAuthenticator
// instead.
func NewSupabaseAuthenticator(url, anonKey, serviceRoleKey string) (*SupabaseAuthenticator, error) {
	if url == "" {
		return nil, errors.New("supabase url is required")
	}
	if anonKey == "" {
		return nil, errors.New("supabase anon key is required")
	}
	if serviceRoleKey == "" {
		return nil, errors.New("supabase service role key is required")
	}
	return &SupabaseAuthenticator{
		URL:            strings.TrimRight(url, "/"),
		AnonKey:        anonKey,
		ServiceRoleKey: serviceRoleKey,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (s *SupabaseAuthenticator) Authenticate(ctx context.Context, bearer string) (*Identity, error) {
	bearer = strings.TrimSpace(bearer)
	if !strings.HasPrefix(strings.ToLower(bearer), "bearer ") {
		return nil, errors.New("missing or malformed Authorization header")
	}
	token := strings.TrimSpace(bearer[len("bearer "):])
	if token == "" {
		return nil, errors.New("empty bearer token")
	}

	user, err := s.fetchUser(ctx, token)
	if err != nil {
		return nil, err
	}

	role, err := s.fetchRole(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve internal role: %w", err)
	}

	return &Identity{UserID: user.ID, Email: user.Email, Role: role}, nil
}

type supabaseUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func (s *SupabaseAuthenticator) fetchUser(ctx context.Context, token string) (*supabaseUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/auth/v1/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("apikey", s.AnonKey)
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("invalid or expired token")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("supabase auth %d: %s", resp.StatusCode, string(body))
	}

	var u supabaseUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("supabase auth decode: %w", err)
	}
	if u.ID == "" {
		return nil, errors.New("supabase auth: empty user id")
	}
	return &u, nil
}

// roleRow is what we read from internal_roles via PostgREST.
type roleRow struct {
	Role string `json:"role"`
}

func (s *SupabaseAuthenticator) fetchRole(ctx context.Context, userID string) (string, error) {
	// PostgREST: select=role, filter user_id=eq.<id>, single row
	url := fmt.Sprintf("%s/rest/v1/internal_roles?select=role&user_id=eq.%s", s.URL, userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", s.ServiceRoleKey)
	req.Header.Set("Authorization", "Bearer "+s.ServiceRoleKey)
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("postgrest %d: %s", resp.StatusCode, string(body))
	}
	var rows []roleRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return "", err
	}
	if len(rows) == 0 {
		// No row = no Control Tower role assigned. Return empty string;
		// caller decides whether that's a 403 or a fallback.
		return "", nil
	}
	return rows[0].Role, nil
}

// StaticAuthenticator is a dev/test fixed identity. Production must
// not use this — the server logs a prominent warning when constructed
// with a non-platform_admin role.
type StaticAuthenticator struct {
	Identity *Identity
}

func (s *StaticAuthenticator) Authenticate(_ context.Context, _ string) (*Identity, error) {
	if s.Identity == nil {
		return nil, errors.New("static authenticator has nil identity")
	}
	return s.Identity, nil
}
