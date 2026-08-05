package oidc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The tests in this file hold the client-binding contract of the
// authorization-code grant: a code belongs to exactly one client, and only that
// client can redeem it. An authorization request that names no client is
// refused outright, so a stored empty client id can never exist and can never
// act as a wildcard.

const attackerClientID = "attacker-client"

// authorizedBrowser returns an HTTP client holding a session cookie for a freshly
// registered key, so /authorize takes the silent pass-through path.
func authorizedBrowser(t *testing.T, p *Provider, base, comment string) *http.Client {
	t.Helper()
	line, signer := genKeyAndSigner(t, comment)
	if _, err := p.store.Add(line); err != nil {
		t.Fatalf("add identity: %v", err)
	}
	fp, nonce, sig := sshProof(t, base, signer)
	exResp, err := http.PostForm(base+PathAuthnExchange, url.Values{
		formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig},
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	var ex struct {
		Ticket string `json:"ticket"`
	}
	_ = json.NewDecoder(exResp.Body).Decode(&ex)
	_ = exResp.Body.Close()
	if ex.Ticket == "" {
		t.Fatal("empty ticket")
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cResp, err := browser.Get(base + PathAuthnConsume + "?ticket=" + url.QueryEscape(ex.Ticket))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	_ = cResp.Body.Close()
	return browser
}

// authorizeFor drives /authorize with the given client_id and returns the
// response status plus the authorization code (empty when none was issued).
func authorizeFor(t *testing.T, browser *http.Client, base, clientID string) (int, string) {
	t.Helper()
	q := url.Values{
		"response_type": {responseTypeCode},
		"redirect_uri":  {testRedirectURI},
	}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	resp, err := browser.Get(base + pathAuthorize + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return resp.StatusCode, ""
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	return resp.StatusCode, loc.Query().Get(responseTypeCode)
}

// postToken redeems a code at /token with the given client_id (omitted when
// empty) and returns the status and body.
func postToken(t *testing.T, base, code, clientID string) (int, string) {
	t.Helper()
	return postTokenWithVerifier(t, base, code, clientID, "")
}

// postTokenWithVerifier redeems a code at /token, omitting client_id and
// code_verifier when empty, and returns the status and body.
func postTokenWithVerifier(t *testing.T, base, code, clientID, verifier string) (int, string) {
	t.Helper()
	form := url.Values{
		formFieldGrantType: {grantTypeAuthCode},
		responseTypeCode:   {code},
		"redirect_uri":     {testRedirectURI},
	}
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	resp, err := http.PostForm(base+pathToken, form)
	if err != nil {
		t.Fatalf("token post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestAuthorizeRequiresClientID holds that an authorization request naming no
// client is refused, so no code can ever be stored without a client binding.
func TestAuthorizeRequiresClientID(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	browser := authorizedBrowser(t, p, srv.URL, "nobody@host")
	status, code := authorizeFor(t, browser, srv.URL, "")
	if status != http.StatusBadRequest {
		t.Fatalf("authorize without client_id: status=%d want 400", status)
	}
	if code != "" {
		t.Fatalf("authorize without client_id issued code %q", code)
	}
}

// TestAuthorizeRejectsUnregisteredClientID holds that only a client in the
// provider's registry can start a flow; an arbitrary value is refused.
func TestAuthorizeRejectsUnregisteredClientID(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	browser := authorizedBrowser(t, p, srv.URL, "eve@host")
	status, code := authorizeFor(t, browser, srv.URL, attackerClientID)
	if status != http.StatusBadRequest {
		t.Fatalf("authorize with unregistered client_id: status=%d want 400", status)
	}
	if code != "" {
		t.Fatalf("unregistered client_id issued code %q", code)
	}
}

// TestTokenRejectsForeignClientRedemption holds that a code issued to client A
// cannot be redeemed by client B, and that omitting client_id at /token is not a
// way around the check either.
func TestTokenRejectsForeignClientRedemption(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	browser := authorizedBrowser(t, p, srv.URL, "alice@host")

	// Client B cannot redeem client A's code.
	status, code := authorizeFor(t, browser, srv.URL, oidcClientID)
	if status != http.StatusFound || code == "" {
		t.Fatalf("authorize status=%d code=%q", status, code)
	}
	tokStatus, body := postToken(t, srv.URL, code, attackerClientID)
	if tokStatus == http.StatusOK {
		t.Fatalf("a foreign client redeemed the code: %s", body)
	}

	// Omitting client_id is not a way around the check.
	status, code = authorizeFor(t, browser, srv.URL, oidcClientID)
	if status != http.StatusFound || code == "" {
		t.Fatalf("authorize status=%d code=%q", status, code)
	}
	tokStatus, body = postToken(t, srv.URL, code, "")
	if tokStatus == http.StatusOK {
		t.Fatalf("a client_id-less token request redeemed the code: %s", body)
	}
}

// TestTokenRejectsUnboundCode holds that an empty stored client id is not a
// wildcard in either direction: such a code cannot be redeemed by any client id,
// and it cannot be redeemed by omitting client_id either. The stored value is
// planted directly because no endpoint can produce one any more.
func TestTokenRejectsUnboundCode(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, _ := genKeyAndSigner(t, "mallory@host")
	ident, err := p.store.Add(line)
	if err != nil {
		t.Fatalf("add identity: %v", err)
	}
	g := grant{fingerprint: ident.Fingerprint, comment: ident.Comment}

	for _, submitted := range []string{attackerClientID, oidcClientID, ""} {
		code, cerr := p.sso.newCode(g, "", testRedirectURI, "", "")
		if cerr != nil {
			t.Fatalf("newCode: %v", cerr)
		}
		status, body := postToken(t, srv.URL, code, submitted)
		if status == http.StatusOK {
			t.Fatalf("unbound code redeemed with client_id=%q: %s", submitted, body)
		}
	}
}

// TestTokenClaimUsesStoredClientID holds that the signed client_id claim comes
// from the authorization grant, never from the token request.
func TestTokenClaimUsesStoredClientID(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	browser := authorizedBrowser(t, p, srv.URL, "bob@host")
	status, code := authorizeFor(t, browser, srv.URL, oidcClientID)
	if status != http.StatusFound || code == "" {
		t.Fatalf("authorize status=%d code=%q", status, code)
	}
	claims := redeemCodeForToken(t, p, srv.URL, code, oidcClientID, "")
	if claims.ClientID != oidcClientID {
		t.Fatalf("client_id claim = %q want %q", claims.ClientID, oidcClientID)
	}
}

// TestChallengePathKeepsClientBinding holds that the out-of-band approval path
// binds the same client the authorization request named, so the poll-issued code
// is no weaker than the silent one.
func TestChallengePathKeepsClientBinding(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, signer := genKeyAndSigner(t, "carol@host")
	if _, err := p.store.Add(line); err != nil {
		t.Fatalf("add identity: %v", err)
	}

	chResp, err := http.Get(srv.URL + pathAuthorize + "?" + url.Values{
		"response_type": {responseTypeCode},
		"client_id":     {oidcClientID},
		"redirect_uri":  {testRedirectURI},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	page, _ := io.ReadAll(chResp.Body)
	_ = chResp.Body.Close()
	reqID := extractReqID(t, string(page))

	approveWithKey(t, srv.URL, reqID, signer)

	pr := pollOnce(t, srv.URL, reqID)
	if pr.Status != statusApproved {
		t.Fatalf("poll status=%s want approved", pr.Status)
	}
	loc, _ := url.Parse(pr.Redirect)
	code := loc.Query().Get(responseTypeCode)
	if code == "" {
		t.Fatal("poll returned no code")
	}
	if status, body := postToken(t, srv.URL, code, attackerClientID); status == http.StatusOK {
		t.Fatalf("poll-issued code redeemed by a foreign client: %s", body)
	}
}

// approveWithKey performs the out-of-band CLI approval for a pending request.
func approveWithKey(t *testing.T, base, reqID string, signer ssh.Signer) {
	t.Helper()
	fp, nonce, sig := sshProof(t, base, signer)
	resp, err := http.PostForm(base+PathAuthnApprove, url.Values{
		"request_id": {reqID}, formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig},
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("approve status=%d", status)
	}
}

// TestConfigClientIDsRegistry holds that Config.ClientIDs is the whole registry:
// each listed client can start a flow and redeem its own code, a client outside
// the list cannot start one, and a listed client cannot redeem another's code.
func TestConfigClientIDsRegistry(t *testing.T) {
	const secondClientID = "bladerunner-cli"

	dir := t.TempDir()
	key, err := LoadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	p, err := NewProvider(Config{
		ListenAddr: "127.0.0.1:0",
		IssuerURL:  "http://127.0.0.1:18556",
		Audience:   oidcClientID,
		ClientIDs:  []string{oidcClientID, secondClientID},
		SigningKey: key,
		Store:      NewStore(t.TempDir()),
		TokenTTL:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	browser := authorizedBrowser(t, p, srv.URL, "dave@host")

	for _, id := range []string{oidcClientID, secondClientID} {
		status, code := authorizeFor(t, browser, srv.URL, id)
		if status != http.StatusFound || code == "" {
			t.Fatalf("registered client %q: status=%d code=%q", id, status, code)
		}
		claims := redeemCodeForToken(t, p, srv.URL, code, id, "")
		if claims.ClientID != id {
			t.Fatalf("client_id claim = %q want %q", claims.ClientID, id)
		}
	}

	// A client outside the registry cannot start a flow.
	if status, code := authorizeFor(t, browser, srv.URL, attackerClientID); status != http.StatusBadRequest || code != "" {
		t.Fatalf("unregistered client: status=%d code=%q", status, code)
	}

	// One registered client cannot redeem another registered client's code.
	status, code := authorizeFor(t, browser, srv.URL, oidcClientID)
	if status != http.StatusFound || code == "" {
		t.Fatalf("authorize status=%d code=%q", status, code)
	}
	if tokStatus, body := postToken(t, srv.URL, code, secondClientID); tokStatus == http.StatusOK {
		t.Fatalf("sibling client redeemed another client's code: %s", body)
	}
}
