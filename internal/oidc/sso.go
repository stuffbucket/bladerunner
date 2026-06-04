package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// This file implements the browser single-sign-on flow on top of the SSH-key
// identity model. Two paths are supported:
//
//   1. Silent pass-through (the `runner web` happy path). A local CLI that holds a
//      registered SSH private key proves possession over /authn/* and receives a
//      one-time ticket. Opening /authn/consume?ticket=... in the browser sets a
//      session cookie. When Incus later redirects the browser to /authorize, the
//      cookie is present and the provider issues an authorization code with no
//      user interaction.
//
//   2. Challenge (anyone without a usable registered key). /authorize with no
//      session renders an account picker that waits for an out-of-band approval:
//      a terminal holding a registered key runs `runner web approve <request-id>`,
//      which proves possession and binds that identity to the pending request.
//      The browser polls /authorize/poll and proceeds once approved. A requester
//      with no registered key can never satisfy the proof, so they stay blocked.
//
// All proofs are an SSH signature over a server-issued single-use nonce, verified
// against the registered identity's public key. Tokens, tickets, codes, sessions
// and pending requests are short-lived and held in memory only.

const (
	pathAuthnNonce    = "/authn/nonce"
	pathAuthnExchange = "/authn/exchange"
	pathAuthnConsume  = "/authn/consume"
	pathAuthnApprove  = "/authn/approve"
	pathAuthorizePoll = "/authorize/poll"

	// sessionCookieName is the provider-origin cookie that marks a browser as
	// authenticated to a particular SSH identity. SameSite=Lax so it rides the
	// top-level GET redirect Incus performs into /authorize.
	sessionCookieName = "br_oidc_session"

	// grantTypeAuthCode is the standard OAuth2 authorization-code grant, added
	// alongside the custom ssh-key grant so the Incus web UI can complete login.
	grantTypeAuthCode = "authorization_code"

	nonceTTL   = 2 * time.Minute
	ticketTTL  = 30 * time.Second
	codeTTL    = time.Minute
	sessionTTL = 12 * time.Hour
	pendingTTL = 5 * time.Minute

	randTokenBytes = 32
	reqIDBytes     = 16

	// Repeated string literals lifted to constants (goconst).
	pkceMethodS256       = "S256"
	pkceMethodPlain      = "plain"
	statusApproved       = "approved"
	responseTypeCode     = "code"
	formFieldNonce       = "nonce"
	formFieldFingerprint = "fingerprint"
	formFieldSignature   = "signature"
	fieldStatus          = "status"
)

// grant is an authenticated identity bound to a ticket/session, with an expiry.
type grant struct {
	fingerprint string
	comment     string
	expiry      time.Time
}

// authzCode is an issued OAuth2 authorization code awaiting redemption at /token.
type authzCode struct {
	fingerprint   string
	comment       string
	clientID      string
	redirectURI   string
	codeChallenge string
	codeMethod    string
	expiry        time.Time
}

// pendingAuth is an /authorize request that is waiting for a CLI approval.
type pendingAuth struct {
	clientID        string
	redirectURI     string
	state           string
	scope           string
	codeChallenge   string
	codeMethod      string
	approvedFP      string
	approvedComment string
	expiry          time.Time
}

// ssoState holds the short-lived in-memory state for the browser SSO flow.
type ssoState struct {
	mu       sync.Mutex
	nonces   map[string]time.Time
	tickets  map[string]grant
	sessions map[string]grant
	codes    map[string]authzCode
	pending  map[string]*pendingAuth
}

func newSSOState() *ssoState {
	return &ssoState{
		nonces:   map[string]time.Time{},
		tickets:  map[string]grant{},
		sessions: map[string]grant{},
		codes:    map[string]authzCode{},
		pending:  map[string]*pendingAuth{},
	}
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// gc drops expired entries. Callers must hold s.mu.
func (s *ssoState) gc(now time.Time) {
	for k, exp := range s.nonces {
		if now.After(exp) {
			delete(s.nonces, k)
		}
	}
	for k, g := range s.tickets {
		if now.After(g.expiry) {
			delete(s.tickets, k)
		}
	}
	for k, g := range s.sessions {
		if now.After(g.expiry) {
			delete(s.sessions, k)
		}
	}
	for k, c := range s.codes {
		if now.After(c.expiry) {
			delete(s.codes, k)
		}
	}
	for k, p := range s.pending {
		if now.After(p.expiry) {
			delete(s.pending, k)
		}
	}
}

func (s *ssoState) newNonce() (string, error) {
	n, err := randToken(randTokenBytes)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	s.nonces[n] = time.Now().Add(nonceTTL)
	return n, nil
}

// consumeNonce returns true exactly once for a live, previously-issued nonce.
func (s *ssoState) consumeNonce(n string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.nonces[n]
	if !ok {
		return false
	}
	delete(s.nonces, n)
	return time.Now().Before(exp)
}

func (s *ssoState) newTicket(ident Identity) (string, error) {
	t, err := randToken(randTokenBytes)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	s.tickets[t] = grant{fingerprint: ident.Fingerprint, comment: ident.Comment, expiry: time.Now().Add(ticketTTL)}
	return t, nil
}

func (s *ssoState) redeemTicket(t string) (grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.tickets[t]
	if !ok {
		return grant{}, false
	}
	delete(s.tickets, t)
	return g, time.Now().Before(g.expiry)
}

func (s *ssoState) newSession(g grant) (string, error) {
	sid, err := randToken(randTokenBytes)
	if err != nil {
		return "", err
	}
	g.expiry = time.Now().Add(sessionTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	s.sessions[sid] = g
	return sid, nil
}

func (s *ssoState) session(sid string) (grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.sessions[sid]
	if !ok || !time.Now().Before(g.expiry) {
		return grant{}, false
	}
	return g, true
}

func (s *ssoState) newCode(g grant, clientID, redirectURI, challenge, method string) (string, error) {
	code, err := randToken(randTokenBytes)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	s.codes[code] = authzCode{
		fingerprint:   g.fingerprint,
		comment:       g.comment,
		clientID:      clientID,
		redirectURI:   redirectURI,
		codeChallenge: challenge,
		codeMethod:    method,
		expiry:        time.Now().Add(codeTTL),
	}
	return code, nil
}

func (s *ssoState) redeemCode(code string) (authzCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ac, ok := s.codes[code]
	if !ok {
		return authzCode{}, false
	}
	delete(s.codes, code)
	return ac, time.Now().Before(ac.expiry)
}

func (s *ssoState) newPending(clientID, redirectURI, state, scope, challenge, method string) (string, error) {
	id, err := randToken(reqIDBytes)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	s.pending[id] = &pendingAuth{
		clientID:      clientID,
		redirectURI:   redirectURI,
		state:         state,
		scope:         scope,
		codeChallenge: challenge,
		codeMethod:    method,
		expiry:        time.Now().Add(pendingTTL),
	}
	return id, nil
}

// approve binds a proven identity to a pending request. Returns false if no such
// (live) pending request exists.
func (s *ssoState) approve(reqID string, ident Identity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	pa, ok := s.pending[reqID]
	if !ok {
		return false
	}
	pa.approvedFP = ident.Fingerprint
	pa.approvedComment = ident.Comment
	return true
}

// takeApproved inspects a pending request. found is false if it does not exist
// (expired or never created). When found and approved, the pending request is
// removed and the bound grant plus its parameters are returned.
func (s *ssoState) takeApproved(reqID string) (g grant, params pendingAuth, approved, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())
	pa, ok := s.pending[reqID]
	if !ok {
		return grant{}, pendingAuth{}, false, false
	}
	if pa.approvedFP == "" {
		return grant{}, *pa, false, true
	}
	delete(s.pending, reqID)
	g = grant{fingerprint: pa.approvedFP, comment: pa.approvedComment, expiry: time.Now()}
	return g, *pa, true, true
}

// verifySSHProof checks that sigB64 is a valid SSH signature by ident's key over
// the raw bytes of the base64url-encoded nonce.
func verifySSHProof(ident Identity, nonce, sigB64 string) error {
	nonceBytes, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		return fmt.Errorf("decode nonce: %w", err)
	}
	blob, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	pub, _, err := parseAuthorizedKey(ident.PublicKey)
	if err != nil {
		return err
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(blob, &sig); err != nil {
		return fmt.Errorf("unmarshal signature: %w", err)
	}
	return pub.Verify(nonceBytes, &sig)
}

// verifyPKCE validates a code_verifier against the stored challenge per RFC 7636.
// An empty challenge means PKCE was not used.
func verifyPKCE(challenge, method, verifier string) error {
	if challenge == "" {
		return nil
	}
	if verifier == "" {
		return errors.New("code_verifier required")
	}
	switch method {
	case pkceMethodS256:
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			return errors.New("pkce verification failed")
		}
	case pkceMethodPlain, "":
		if verifier != challenge {
			return errors.New("pkce verification failed")
		}
	default:
		return fmt.Errorf("unsupported code_challenge_method: %s", method)
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if h == "" || h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// sanitizeNext guards against open redirects: only loopback (or relative)
// destinations are honored; anything else falls back to "/".
func sanitizeNext(next string) string {
	if next == "" {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil {
		return "/"
	}
	if u.IsAbs() && !isLoopbackHost(u.Hostname()) {
		return "/"
	}
	return next
}

func buildCodeRedirect(redirectURI, code, state string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	q.Set(responseTypeCode, code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- HTTP handlers -------------------------------------------------------

func (p *Provider) handleAuthnNonce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	n, err := p.sso.newNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{formFieldNonce: n})
}

func (p *Provider) handleAuthnExchange(w http.ResponseWriter, r *http.Request) {
	ident, ok := p.verifyProofRequest(w, r)
	if !ok {
		return
	}
	ticket, err := p.sso.newTicket(ident)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func (p *Provider) handleAuthnConsume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	g, ok := p.sso.redeemTicket(r.URL.Query().Get("ticket"))
	if !ok {
		http.Error(w, "invalid or expired ticket", http.StatusBadRequest)
		return
	}
	sid, err := p.sso.newSession(g)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// The provider serves plain HTTP on loopback; setting Secure would stop the
	// cookie from ever being sent. HttpOnly+SameSite are set.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	// sanitizeNext restricts the target to loopback/relative URLs.
	http.Redirect(w, r, sanitizeNext(r.URL.Query().Get("next")), http.StatusFound)
}

func (p *Provider) handleAuthnApprove(w http.ResponseWriter, r *http.Request) {
	ident, ok := p.verifyProofRequest(w, r)
	if !ok {
		return
	}
	reqID := r.PostForm.Get("request_id")
	if reqID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "request_id is required")
		return
	}
	if !p.sso.approve(reqID, ident) {
		writeError(w, http.StatusNotFound, "invalid_request", "no such pending request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: statusApproved})
}

// verifyProofRequest parses a POSTed SSH-key proof (fingerprint+nonce+signature),
// validates it, and returns the proven identity. On any failure it writes the
// error response and returns ok=false. It leaves r.PostForm populated so callers
// can read additional fields (e.g. request_id).
func (p *Provider) verifyProofRequest(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return Identity{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "cannot parse form")
		return Identity{}, false
	}
	fingerprint := strings.TrimSpace(r.PostForm.Get(formFieldFingerprint))
	nonce := r.PostForm.Get(formFieldNonce)
	signature := r.PostForm.Get(formFieldSignature)
	if fingerprint == "" || nonce == "" || signature == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "fingerprint, nonce and signature are required")
		return Identity{}, false
	}
	if !p.sso.consumeNonce(nonce) {
		writeError(w, http.StatusBadRequest, "invalid_request", "nonce invalid or expired")
		return Identity{}, false
	}
	ident, ok := p.store.Lookup(fingerprint)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_grant", "fingerprint not registered")
		return Identity{}, false
	}
	if err := verifySSHProof(ident, nonce, signature); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_grant", "signature verification failed")
		return Identity{}, false
	}
	return ident, true
}

// handleAuthorize is the OAuth2 authorization endpoint. With a valid session it
// issues an authorization code immediately (silent pass-through); otherwise it
// renders the challenge page.
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	respType := q.Get("response_type")
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	scope := q.Get("scope")
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")

	if redirectURI == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		return
	}
	if u, err := url.Parse(redirectURI); err != nil || (u.IsAbs() && !isLoopbackHost(u.Hostname())) {
		writeError(w, http.StatusBadRequest, "invalid_request", "redirect_uri must be loopback")
		return
	}
	if respType != "" && respType != responseTypeCode {
		// redirectURI is validated as loopback above before any redirect.
		http.Redirect(w, r, buildErrorRedirect(redirectURI, "unsupported_response_type", state), http.StatusFound)
		return
	}

	// Case A: an authenticated session already exists — sail straight through.
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if g, ok := p.sso.session(c.Value); ok {
			code, cerr := p.sso.newCode(g, clientID, redirectURI, challenge, method)
			if cerr != nil {
				writeError(w, http.StatusInternalServerError, "server_error", cerr.Error())
				return
			}
			// redirectURI is validated as loopback above before any redirect.
			http.Redirect(w, r, buildCodeRedirect(redirectURI, code, state), http.StatusFound)
			return
		}
	}

	// Case B: no session — challenge the user to pick and prove an account.
	reqID, err := p.sso.newPending(clientID, redirectURI, state, scope, challenge, method)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	p.renderChallenge(w, reqID)
}

func (p *Provider) handleAuthorizePoll(w http.ResponseWriter, r *http.Request) {
	reqID := r.URL.Query().Get("request_id")
	g, params, approved, found := p.sso.takeApproved(reqID)
	if !found {
		writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "expired"})
		return
	}
	if !approved {
		writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "pending"})
		return
	}
	code, err := p.sso.newCode(g, params.clientID, params.redirectURI, params.codeChallenge, params.codeMethod)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		fieldStatus: statusApproved,
		"redirect":  buildCodeRedirect(params.redirectURI, code, params.state),
	})
}

func buildErrorRedirect(redirectURI, errCode, state string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// handleAuthCodeGrant redeems an authorization code at the token endpoint.
// r.PostForm is already parsed by handleToken.
func (p *Provider) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get(responseTypeCode)
	redirectURI := r.PostForm.Get("redirect_uri")
	verifier := r.PostForm.Get("code_verifier")
	clientID := r.PostForm.Get("client_id")
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}
	ac, ok := p.sso.redeemCode(code)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_grant", "invalid or expired code")
		return
	}
	if ac.redirectURI != redirectURI {
		writeError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if ac.clientID != "" && clientID != "" && ac.clientID != clientID {
		writeError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if err := verifyPKCE(ac.codeChallenge, ac.codeMethod, verifier); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	cid := ac.clientID
	if cid == "" {
		cid = clientID
	}
	ident := Identity{Fingerprint: ac.fingerprint, Comment: ac.comment}
	tok, claims, err := p.issuer.Issue(ident, cid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: tok,
		IDToken:     tok,
		TokenType:   "Bearer",
		ExpiresIn:   int(claims.Expiry - claims.IssuedAt),
		Subject:     claims.Subject,
	})
}

type challengeAccount struct {
	Fingerprint string
	Comment     string
}

type challengeData struct {
	RequestID  string
	Accounts   []challengeAccount
	PollPath   string
	ApproveCmd string
}

func (p *Provider) renderChallenge(w http.ResponseWriter, reqID string) {
	ids := p.store.List()
	accts := make([]challengeAccount, 0, len(ids))
	for _, id := range ids {
		accts = append(accts, challengeAccount{Fingerprint: id.Fingerprint, Comment: id.Comment})
	}
	data := challengeData{
		RequestID:  reqID,
		Accounts:   accts,
		PollPath:   pathAuthorizePoll,
		ApproveCmd: fmt.Sprintf("runner web approve %s", reqID),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := challengeTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

var challengeTmpl = template.Must(template.New("challenge").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bladerunner — Sign in to Incus</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; background:#0b0e14; color:#c7d0e0; max-width: 40rem; margin: 4rem auto; padding: 0 1.5rem; line-height: 1.6; }
  h1 { color:#7aa2f7; font-size: 1.4rem; }
  pre { background:#11151f; border:1px solid #2a3142; border-radius:6px; padding:0.8rem 1rem; overflow:auto; color:#9ece6a; }
  code { color:#e0af68; }
  ul { padding-left: 1.2rem; }
  .muted { color:#565f89; }
  #status { margin-top:1.5rem; padding:0.7rem 1rem; border-left:3px solid #7aa2f7; background:#11151f; }
</style>
</head>
<body>
<h1>Sign in to Incus</h1>
<p>This browser session is not yet authenticated with a registered SSH key.</p>
<p>To continue, open a terminal that holds a registered key and run:</p>
<pre>{{.ApproveCmd}}</pre>
<p>You will be signed in as the account whose key approves this request.</p>
<p class="muted">Known accounts:</p>
<ul>
{{range .Accounts}}<li><code>{{.Fingerprint}}</code>{{if .Comment}} — {{.Comment}}{{end}}</li>
{{else}}<li class="muted">No accounts registered. Add one with <code>runner user add &lt;pubkey&gt;</code>.</li>
{{end}}</ul>
<div id="status">Waiting for approval…</div>
<script>
const reqId = {{.RequestID}};
const pollPath = {{.PollPath}};
async function poll() {
  try {
    const res = await fetch(pollPath + '?request_id=' + encodeURIComponent(reqId));
    const data = await res.json();
    if (data.status === 'approved' && data.redirect) { window.location = data.redirect; return; }
    if (data.status === 'expired') { document.getElementById('status').textContent = 'Request expired — reload this page to try again.'; return; }
  } catch (e) { /* transient; keep polling */ }
  setTimeout(poll, 1500);
}
poll();
</script>
</body>
</html>`))
