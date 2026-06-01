package oidc

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testRedirectURI = "http://127.0.0.1:9999/cb"

// genKeyAndSigner returns an authorized_keys line plus a matching ssh.Signer.
func genKeyAndSigner(t *testing.T, comment string) (string, ssh.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return line, signer
}

// sshProof fetches a nonce from the provider and signs it, returning the
// fingerprint, nonce and base64url signature an /authn/* endpoint expects.
func sshProof(t *testing.T, base string, signer ssh.Signer) (fp, nonce, sig string) {
	t.Helper()
	var nr struct {
		Nonce string `json:"nonce"`
	}
	resp, err := http.Get(base + pathAuthnNonce)
	if err != nil {
		t.Fatalf("nonce get: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&nr); err != nil {
		t.Fatalf("nonce decode: %v", err)
	}
	_ = resp.Body.Close()
	if nr.Nonce == "" {
		t.Fatal("empty nonce")
	}
	nonceBytes, err := base64.RawURLEncoding.DecodeString(nr.Nonce)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	signature, err := signer.Sign(rand.Reader, nonceBytes)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return FingerprintPublicKey(signer.PublicKey()),
		nr.Nonce,
		base64.RawURLEncoding.EncodeToString(ssh.Marshal(signature))
}

var reqIDRe = regexp.MustCompile(`const reqId = "([^"]+)"`)

func extractReqID(t *testing.T, page string) string {
	t.Helper()
	m := reqIDRe.FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatalf("could not find request id in challenge page:\n%s", page)
	}
	return m[1]
}

type pollResult struct {
	Status   string `json:"status"`
	Redirect string `json:"redirect"`
}

func pollOnce(t *testing.T, base, reqID string) pollResult {
	t.Helper()
	resp, err := http.Get(base + pathAuthorizePoll + "?request_id=" + url.QueryEscape(reqID))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var pr pollResult
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("poll decode: %v", err)
	}
	return pr
}

// redeemCodeForToken posts an authorization_code grant and returns the verified claims.
func redeemCodeForToken(t *testing.T, p *Provider, base, code, redirectURI, verifier string) *Claims {
	t.Helper()
	form := url.Values{
		formFieldGrantType: {grantTypeAuthCode},
		responseTypeCode:   {code},
		"redirect_uri":     {redirectURI},
		"client_id":        {oidcClientID},
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	resp, err := http.PostForm(base+pathToken, form)
	if err != nil {
		t.Fatalf("token post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("token status=%d body=%s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatalf("token decode: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("empty access token")
	}
	claims, err := p.issuer.Verify(tok.AccessToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	return claims
}

// TestSilentSSOFlow exercises the happy path: a holder of a registered key
// proves possession, the resulting session lets /authorize issue a code with no
// interaction, and the code redeems for a JWT whose subject is the key.
func TestSilentSSOFlow(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, signer := genKeyAndSigner(t, "alice@host")
	ident, err := p.store.Add(line)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// Step 1: proof -> ticket.
	fp, nonce, sig := sshProof(t, srv.URL, signer)
	exResp, err := http.PostForm(srv.URL+pathAuthnExchange, url.Values{
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

	// Browser with a cookie jar; never auto-follow redirects so we can inspect them.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Step 2: consume the ticket -> sets the session cookie.
	next := "http://127.0.0.1:9999/ui/"
	cResp, err := browser.Get(srv.URL + pathAuthnConsume + "?ticket=" + url.QueryEscape(ex.Ticket) + "&next=" + url.QueryEscape(next))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	_ = cResp.Body.Close()
	if cResp.StatusCode != http.StatusFound {
		t.Fatalf("consume status=%d want 302", cResp.StatusCode)
	}

	// Step 3: /authorize with the session cookie -> immediate code redirect.
	redirectURI := testRedirectURI
	aResp, err := browser.Get(srv.URL + pathAuthorize +
		"?response_type=code&client_id=bladerunner&redirect_uri=" + url.QueryEscape(redirectURI) + "&state=xyz")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = aResp.Body.Close()
	if aResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status=%d want 302 (silent)", aResp.StatusCode)
	}
	loc, _ := url.Parse(aResp.Header.Get("Location"))
	code := loc.Query().Get(responseTypeCode)
	if code == "" {
		t.Fatalf("no code in redirect %s", aResp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "xyz" {
		t.Fatalf("state not echoed: %s", loc.RawQuery)
	}

	// Step 4: redeem the code.
	claims := redeemCodeForToken(t, p, srv.URL, code, redirectURI, "")
	if claims.Subject != ident.Fingerprint {
		t.Fatalf("sub=%s want %s", claims.Subject, ident.Fingerprint)
	}
}

// TestSilentSSOFlowPKCE confirms an S256 code_challenge is enforced end to end.
func TestSilentSSOFlowPKCE(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, signer := genKeyAndSigner(t, "carol@host")
	if _, err := p.store.Add(line); err != nil {
		t.Fatalf("add: %v", err)
	}

	fp, nonce, sig := sshProof(t, srv.URL, signer)
	exResp, _ := http.PostForm(srv.URL+pathAuthnExchange, url.Values{
		formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig},
	})
	var ex struct {
		Ticket string `json:"ticket"`
	}
	_ = json.NewDecoder(exResp.Body).Decode(&ex)
	_ = exResp.Body.Close()

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cResp, _ := browser.Get(srv.URL + pathAuthnConsume + "?ticket=" + url.QueryEscape(ex.Ticket))
	_ = cResp.Body.Close()

	verifier := "this-is-a-sufficiently-long-pkce-code-verifier-string"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	redirectURI := testRedirectURI
	aResp, _ := browser.Get(srv.URL + pathAuthorize +
		"?response_type=code&client_id=bladerunner&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&code_challenge=" + challenge + "&code_challenge_method=S256")
	_ = aResp.Body.Close()
	loc, _ := url.Parse(aResp.Header.Get("Location"))
	code := loc.Query().Get(responseTypeCode)
	if code == "" {
		t.Fatal("no code")
	}

	// Wrong verifier rejected.
	badForm := url.Values{
		formFieldGrantType: {grantTypeAuthCode},
		responseTypeCode:   {code},
		"redirect_uri":     {redirectURI},
		"code_verifier":    {"wrong-verifier"},
	}
	badResp, _ := http.PostForm(srv.URL+pathToken, badForm)
	badStatus := badResp.StatusCode
	_ = badResp.Body.Close()
	if badStatus != http.StatusBadRequest {
		t.Fatalf("wrong verifier status=%d want 400", badStatus)
	}

	// The code was consumed on the failed attempt; re-issue and redeem correctly.
	aResp2, _ := browser.Get(srv.URL + pathAuthorize +
		"?response_type=code&client_id=bladerunner&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&code_challenge=" + challenge + "&code_challenge_method=S256")
	_ = aResp2.Body.Close()
	loc2, _ := url.Parse(aResp2.Header.Get("Location"))
	claims := redeemCodeForToken(t, p, srv.URL, loc2.Query().Get(responseTypeCode), redirectURI, verifier)
	if claims.Subject != fp {
		t.Fatalf("sub=%s want %s", claims.Subject, fp)
	}
}

// TestChallengeApproveFlow exercises the challenge path: no session renders the
// picker, a CLI approval with a registered key resolves the pending request, and
// the browser poll returns a code that redeems for that key's JWT.
func TestChallengeApproveFlow(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, signer := genKeyAndSigner(t, "bob@host")
	ident, err := p.store.Add(line)
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	redirectURI := testRedirectURI
	chResp, err := http.Get(srv.URL + pathAuthorize +
		"?response_type=code&redirect_uri=" + url.QueryEscape(redirectURI) + "&state=foo")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	page, _ := io.ReadAll(chResp.Body)
	_ = chResp.Body.Close()
	if chResp.StatusCode != http.StatusOK {
		t.Fatalf("challenge status=%d", chResp.StatusCode)
	}
	reqID := extractReqID(t, string(page))

	if pr := pollOnce(t, srv.URL, reqID); pr.Status != "pending" {
		t.Fatalf("status=%s want pending", pr.Status)
	}

	// Approve from a terminal that holds the key.
	fp, nonce, sig := sshProof(t, srv.URL, signer)
	apResp, err := http.PostForm(srv.URL+pathAuthnApprove, url.Values{
		"request_id": {reqID}, formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig},
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	apStatus := apResp.StatusCode
	_ = apResp.Body.Close()
	if apStatus != http.StatusOK {
		t.Fatalf("approve status=%d", apStatus)
	}

	pr := pollOnce(t, srv.URL, reqID)
	if pr.Status != statusApproved {
		t.Fatalf("status=%s want approved", pr.Status)
	}
	loc, _ := url.Parse(pr.Redirect)
	if loc.Query().Get("state") != "foo" {
		t.Fatalf("state not echoed: %s", pr.Redirect)
	}
	claims := redeemCodeForToken(t, p, srv.URL, loc.Query().Get(responseTypeCode), redirectURI, "")
	if claims.Subject != ident.Fingerprint {
		t.Fatalf("sub=%s want %s", claims.Subject, ident.Fingerprint)
	}
}

// TestExchangeRejectsUnregisteredKey verifies an unknown key cannot obtain a ticket.
func TestExchangeRejectsUnregisteredKey(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	_, signer := genKeyAndSigner(t, "mallory@host") // never added to the store
	fp, nonce, sig := sshProof(t, srv.URL, signer)
	resp, err := http.PostForm(srv.URL+pathAuthnExchange, url.Values{
		formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig},
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 for unregistered key", resp.StatusCode)
	}
}

// TestNonceIsSingleUse verifies a nonce cannot be replayed.
func TestNonceIsSingleUse(t *testing.T) {
	p := newTestProvider(t)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	line, signer := genKeyAndSigner(t, "dave@host")
	if _, err := p.store.Add(line); err != nil {
		t.Fatalf("add: %v", err)
	}
	fp, nonce, sig := sshProof(t, srv.URL, signer)
	form := url.Values{formFieldFingerprint: {fp}, formFieldNonce: {nonce}, formFieldSignature: {sig}}

	first, _ := http.PostForm(srv.URL+pathAuthnExchange, form)
	firstStatus := first.StatusCode
	_ = first.Body.Close()
	if firstStatus != http.StatusOK {
		t.Fatalf("first exchange status=%d want 200", firstStatus)
	}
	second, _ := http.PostForm(srv.URL+pathAuthnExchange, form)
	secondStatus := second.StatusCode
	_ = second.Body.Close()
	if secondStatus != http.StatusBadRequest {
		t.Fatalf("replayed nonce status=%d want 400", secondStatus)
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "abc123-verifier-value-long-enough"
	sum := sha256.Sum256([]byte(verifier))
	s256 := base64.RawURLEncoding.EncodeToString(sum[:])

	cases := []struct {
		name      string
		challenge string
		method    string
		verifier  string
		wantErr   bool
	}{
		{"no pkce", "", "", "", false},
		{"s256 ok", s256, pkceMethodS256, verifier, false},
		{"s256 mismatch", s256, pkceMethodS256, "nope", true},
		{"s256 missing verifier", s256, pkceMethodS256, "", true},
		{"plain ok", verifier, pkceMethodPlain, verifier, false},
		{"plain default method", verifier, "", verifier, false},
		{"bad method", s256, "S512", verifier, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyPKCE(tc.challenge, tc.method, tc.verifier)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyPKCE(%q,%q,%q) err=%v wantErr=%v", tc.challenge, tc.method, tc.verifier, err, tc.wantErr)
			}
		})
	}
}
