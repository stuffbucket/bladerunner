package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

const (
	// MIME content types
	contentTypeJSON = "application/json"

	// OIDC endpoint paths
	pathDiscovery = "/.well-known/openid-configuration"
	pathJWKS      = "/jwks"
	pathToken     = "/token"
	pathAuthorize = "/authorize"

	// Default grant type for the local CLI flow.
	grantTypeSSHKey = "urn:bladerunner:params:oauth:grant-type:ssh-key"

	// oidcClientID is the default OIDC client_id / audience for bladerunner-issued
	// tokens. Used both as the default Config.Audience and in tests.
	oidcClientID = "bladerunner"

	// formFieldGrantType is the OAuth2 form-field name for the grant type.
	formFieldGrantType = "grant_type"

	// shutdownTimeout caps how long Stop will wait for active requests to drain.
	shutdownTimeout = 5 * time.Second
	// readTimeout caps how long the server waits for a request to arrive.
	readTimeout = 10 * time.Second
	// writeTimeout caps how long the server waits to write a response.
	writeTimeout = 10 * time.Second
	// idleTimeout caps how long an idle keepalive connection lingers.
	idleTimeout = 60 * time.Second
	// maxTokenRequestBytes bounds the body size for /token POSTs.
	maxTokenRequestBytes = 16 * 1024
)

// Provider is a minimal OIDC server. It exposes discovery, JWKS, and a token
// endpoint that mints JWTs for registered SSH-key identities.
//
// Scope for this PR: discovery, JWKS, and a token endpoint that accepts a
// fingerprint and returns a signed JWT if the fingerprint is registered.
// The browser-based authorization code + PKCE flow is intentionally a stub
// that returns 501; it will be implemented in a follow-up.
type Provider struct {
	issuer   *Issuer
	store    *Store
	sso      *ssoState
	listener net.Listener
	server   *http.Server
	addr     string
	baseURL  string

	mu      sync.Mutex
	started bool
}

// Config holds parameters for constructing a Provider.
type Config struct {
	// ListenAddr is the host:port to listen on (e.g. "127.0.0.1:15556").
	ListenAddr string
	// IssuerURL is the public issuer URL advertised in discovery and tokens.
	// In bladerunner this is typically the in-guest URL (loopback inside the VM)
	// because Incus is the consumer.
	IssuerURL string
	// Audience is the expected `aud` claim.
	Audience string
	// SigningKey is the RSA key used to sign issued JWTs.
	SigningKey *SigningKey
	// Store is the identity registry.
	Store *Store
	// TokenTTL is the lifetime of issued tokens. Zero means DefaultTokenTTL.
	TokenTTL time.Duration
}

// NewProvider constructs a Provider but does not start the HTTP server.
// Call Start to bind the listener.
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.SigningKey == nil {
		return nil, errors.New("oidc: signing key required")
	}
	if cfg.Store == nil {
		return nil, errors.New("oidc: identity store required")
	}
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: issuer URL required")
	}
	if cfg.Audience == "" {
		cfg.Audience = oidcClientID
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:15556"
	}

	issuer, err := NewIssuer(cfg.SigningKey, cfg.IssuerURL, cfg.Audience, cfg.TokenTTL)
	if err != nil {
		return nil, err
	}

	return &Provider{
		issuer:  issuer,
		store:   cfg.Store,
		sso:     newSSOState(),
		addr:    cfg.ListenAddr,
		baseURL: cfg.IssuerURL,
	}, nil
}

// Issuer returns the underlying token issuer, useful for callers that want to
// mint or verify tokens directly without going through HTTP.
func (p *Provider) Issuer() *Issuer { return p.issuer }

// Store returns the identity store.
func (p *Provider) Store() *Store { return p.store }

// Addr returns the bound address (only valid after Start).
func (p *Provider) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.addr
}

// Handler returns the HTTP handler — exported so tests can exercise the routes
// without binding a real listener.
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathDiscovery, p.handleDiscovery)
	mux.HandleFunc(pathJWKS, p.handleJWKS)
	mux.HandleFunc(pathToken, p.handleToken)
	mux.HandleFunc(pathAuthorize, p.handleAuthorize)
	mux.HandleFunc(pathAuthorizePoll, p.handleAuthorizePoll)
	mux.HandleFunc(pathAuthnNonce, p.handleAuthnNonce)
	mux.HandleFunc(pathAuthnExchange, p.handleAuthnExchange)
	mux.HandleFunc(pathAuthnConsume, p.handleAuthnConsume)
	mux.HandleFunc(pathAuthnApprove, p.handleAuthnApprove)
	return mux
}

// Start binds the listener and serves the OIDC HTTP API in a background goroutine.
// It returns once the listener is accepting connections.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return errors.New("oidc: already started")
	}

	lc := &net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.addr, err)
	}
	p.listener = ln
	p.server = &http.Server{
		Handler:      p.Handler(),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}
	p.started = true

	logging.L().Info("OIDC provider listening",
		"addr", p.listener.Addr().String(),
		"issuer", p.baseURL,
		"audience", p.issuer.Audience(),
		"identities", p.store.Count(),
	)

	go func() {
		if err := p.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.L().Error("oidc server exited", "err", err)
		}
	}()
	return nil
}

// Stop gracefully shuts the HTTP server down.
func (p *Provider) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := p.server.Shutdown(ctx)
	p.started = false
	return err
}

// discoveryDocument mirrors a subset of OpenID Connect Discovery 1.0 — enough
// for Incus's OIDC verifier and for our local CLI flow.
type discoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	GrantTypesSupported              []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupp     []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported    []string `json:"code_challenge_methods_supported"`
}

func (p *Provider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := discoveryDocument{
		Issuer:                           p.baseURL,
		JWKSURI:                          p.baseURL + pathJWKS,
		TokenEndpoint:                    p.baseURL + pathToken,
		AuthorizationEndpoint:            p.baseURL + pathAuthorize,
		ResponseTypesSupported:           []string{"code", "token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{string(signingAlgorithm)},
		GrantTypesSupported:              []string{grantTypeSSHKey, grantTypeAuthCode},
		TokenEndpointAuthMethodsSupp:     []string{"none", "client_secret_post"},
		CodeChallengeMethodsSupported:    []string{"S256", "plain"},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (p *Provider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	data, err := p.issuer.JWKSJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	_, _ = w.Write(data)
}

// tokenResponse follows RFC 6749 §5.1 for an OAuth2 token endpoint success.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token,omitempty"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Subject     string `json:"sub,omitempty"`
}

// handleToken implements the OAuth2 token endpoint. It dispatches on grant_type:
//
//   - urn:bladerunner:params:oauth:grant-type:ssh-key — the custom CLI grant that
//     exchanges a registered SSH-key fingerprint directly for a JWT.
//   - authorization_code — the standard browser grant used by the Incus web UI,
//     redeeming a code minted by handleAuthorize (see sso.go).
func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "cannot parse form")
		return
	}

	switch r.PostForm.Get(formFieldGrantType) {
	case grantTypeSSHKey:
		p.handleSSHKeyGrant(w, r)
	case grantTypeAuthCode:
		p.handleAuthCodeGrant(w, r)
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grants: "+grantTypeSSHKey+", "+grantTypeAuthCode)
	}
}

// handleSSHKeyGrant exchanges a registered fingerprint for a JWT. r.PostForm is
// already parsed by handleToken.
func (p *Provider) handleSSHKeyGrant(w http.ResponseWriter, r *http.Request) {
	fingerprint := strings.TrimSpace(r.PostForm.Get("fingerprint"))
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fingerprint is required")
		return
	}

	ident, ok := p.store.Lookup(fingerprint)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_grant", "fingerprint not registered")
		return
	}

	clientID := r.PostForm.Get("client_id")
	tok, claims, err := p.issuer.Issue(ident, clientID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	resp := tokenResponse{
		AccessToken: tok,
		IDToken:     tok,
		TokenType:   "Bearer",
		ExpiresIn:   int(claims.Expiry - claims.IssuedAt),
		Subject:     claims.Subject,
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if data, err := json.Marshal(v); err == nil {
		_, _ = w.Write(data)
	}
}

func writeError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}
