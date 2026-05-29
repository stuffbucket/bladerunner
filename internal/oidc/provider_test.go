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

func TestTokenEndpoint(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line := genSSHKeyPair(t, "alice@host")
	ident, err := p.store.Add(line)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantError  string
	}{
		{
			name: "success",
			form: url.Values{
				formFieldGrantType: {grantTypeSSHKey},
				"fingerprint":      {ident.Fingerprint},
				"client_id":        {oidcClientID},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "unknown fingerprint",
			form: url.Values{
				formFieldGrantType: {grantTypeSSHKey},
				"fingerprint":      {"SHA256:notreal"},
			},
			wantStatus: http.StatusUnauthorized,
			wantError:  "invalid_grant",
		},
		{
			name: "missing fingerprint",
			form: url.Values{
				formFieldGrantType: {grantTypeSSHKey},
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_request",
		},
		{
			name: "unsupported grant type",
			form: url.Values{
				formFieldGrantType: {"authorization_code"},
			},
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
			if tc.wantError != "" {
				if got, _ := body["error"].(string); got != tc.wantError {
					t.Fatalf("error=%v want=%s", body["error"], tc.wantError)
				}
				return
			}
			token, _ := body["access_token"].(string)
			if token == "" {
				t.Fatal("empty access_token")
			}
			if !strings.Contains(token, ".") {
				t.Fatalf("token does not look like a JWT: %s", token)
			}
			// Verify via the provider's issuer.
			claims, err := p.issuer.Verify(token)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != ident.Fingerprint {
				t.Fatalf("sub mismatch")
			}
		})
	}
}

func TestAuthorizeStub(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + pathAuthorize)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", resp.StatusCode)
	}
}
