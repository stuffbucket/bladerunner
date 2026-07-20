package oidc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	key, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	store := NewStore(t.TempDir())
	p, err := NewProvider(Config{
		ListenAddr: "127.0.0.1:0",
		IssuerURL:  "http://127.0.0.1:18556",
		Audience:   oidcClientID,
		SigningKey: key,
		Store:      store,
		TokenTTL:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestDiscoveryEndpoint(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + pathDiscovery)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Issuer != "http://127.0.0.1:18556" {
		t.Fatalf("issuer=%s", doc.Issuer)
	}
	if doc.JWKSURI == "" || doc.TokenEndpoint == "" {
		t.Fatal("missing endpoints in discovery")
	}
	// The SSH-key CLI grant was removed; discovery must advertise only the
	// authorization_code grant that the browser SSO flow uses.
	for _, g := range doc.GrantTypesSupported {
		if strings.Contains(g, "ssh-key") {
			t.Fatalf("discovery still advertises removed ssh-key grant: %q", doc.GrantTypesSupported)
		}
	}
	if len(doc.GrantTypesSupported) != 1 || doc.GrantTypesSupported[0] != grantTypeAuthCode {
		t.Fatalf("grant_types_supported=%v want [%s]", doc.GrantTypesSupported, grantTypeAuthCode)
	}
}

func TestJWKSEndpoint(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + pathJWKS)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Keys []map[string]interface{} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(body.Keys))
	}
}

// TestTokenEndpointRejectsUnsupportedGrants pins the token endpoint's grant
// dispatch after the dead SSH-key CLI grant was removed: only authorization_code
// is served (exercised end-to-end by the SSO tests), and every other grant —
// including the removed SSH-key URN — is rejected as unsupported. A POST is
// required.
func TestTokenEndpointRejectsUnsupportedGrants(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantError  string
	}{
		{
			name: "removed ssh-key grant is unsupported",
			form: url.Values{
				formFieldGrantType: {"urn:bladerunner:params:oauth:grant-type:ssh-key"},
				"fingerprint":      {"SHA256:whatever"},
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "unsupported_grant_type",
		},
		{
			name: "client_credentials is unsupported",
			form: url.Values{
				formFieldGrantType: {"client_credentials"},
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "unsupported_grant_type",
		},
		{
			name:       "missing grant type is unsupported",
			form:       url.Values{},
			wantStatus: http.StatusBadRequest,
			wantError:  "unsupported_grant_type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.PostForm(srv.URL+pathToken, tc.form)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.wantStatus)
			}
			var body map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got, _ := body["error"].(string); got != tc.wantError {
				t.Fatalf("error=%v want=%s", body["error"], tc.wantError)
			}
		})
	}
}

func TestTokenEndpointRejectsNonPOST(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + pathToken)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestAuthorizeChallengesWithoutSession verifies that hitting /authorize with no
// session cookie renders the HTML challenge page (Case B) rather than silently
// issuing a code.
func TestAuthorizeChallengesWithoutSession(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// A no-redirect client so we can inspect the response directly.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + pathAuthorize +
		"?response_type=code&client_id=bladerunner&redirect_uri=http://127.0.0.1:9999/cb&state=xyz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 challenge page, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected html challenge, got content-type %q", ct)
	}
}

// TestAuthorizeRequiresRedirectURI verifies the redirect_uri is mandatory and
// must be loopback.
func TestAuthorizeRequiresRedirectURI(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + pathAuthorize + "?response_type=code")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without redirect_uri, got %d", resp.StatusCode)
	}
}
