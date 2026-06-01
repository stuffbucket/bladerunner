package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/oidc"
)

// Endpoint paths on the local OIDC provider. Kept in sync with internal/oidc;
// duplicated here so the CLI does not import unexported provider internals.
const (
	authnNoncePath    = "/authn/nonce"
	authnExchangePath = "/authn/exchange"
	authnConsumePath  = "/authn/consume"
	authnApprovePath  = "/authn/approve"

	webHTTPTimeout = 10 * time.Second
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the Incus web UI with single sign-on",
	Long: `Open the Incus web UI in your browser, authenticated as your SSH identity.

If your SSH private key is registered with bladerunner (the same key you use for
'br ssh'), 'br web' proves possession of it and hands the browser a session so
you sail straight into the Incus UI with no prompt.

If the key is missing or not registered, the browser instead lands on a sign-in
challenge that asks you to pick an account and approve it from a terminal that
holds a registered key (see 'br web approve').`,
	RunE: runWeb,
}

var webApproveCmd = &cobra.Command{
	Use:   "approve <request-id>",
	Short: "Approve a pending Incus web sign-in challenge with your SSH key",
	Long: `Approve a browser sign-in challenge shown by the Incus web UI.

The challenge page prints a request id. Run this command in a terminal that holds
a registered SSH key to prove possession and bind that account to the request;
the waiting browser then completes sign-in as that account.`,
	Args: cobra.ExactArgs(1),
	RunE: runWebApprove,
}

func init() {
	webCmd.AddCommand(webApproveCmd)
}

// webEndpoints resolves the running VM's provider URL, Incus UI URL and SSH key
// path from the control socket.
func webEndpoints() (providerBase, incusUI, keyPath string, err error) {
	client, err := requireRunningVM()
	if err != nil {
		return "", "", "", err
	}

	oidcPort, err := client.GetConfig(control.ConfigKeyLocalOIDCPort)
	if err != nil {
		logging.L().Debug("get local-oidc-port failed", "err", err)
		return "", "", "", errVMNotRunning
	}
	if oidcPort == "" || oidcPort == "0" {
		return "", "", "", errors.New("the local OIDC provider is disabled (LocalOIDCPort=0); web SSO is unavailable")
	}
	apiPort, err := client.GetConfig(control.ConfigKeyLocalAPIPort)
	if err != nil {
		logging.L().Debug("get local-api-port failed", "err", err)
		return "", "", "", errVMNotRunning
	}
	keyPath, _ = client.GetConfig(control.ConfigKeySSHPrivateKeyPath)

	providerBase = fmt.Sprintf("http://127.0.0.1:%s", oidcPort)
	incusUI = fmt.Sprintf("https://127.0.0.1:%s/ui/", apiPort)
	return providerBase, incusUI, keyPath, nil
}

func runWeb(_ *cobra.Command, _ []string) error {
	providerBase, incusUI, keyPath, err := webEndpoints()
	if err != nil {
		return err
	}

	ticket, perr := proveAndGetTicket(providerBase, keyPath)
	if perr != nil {
		// No usable registered key: fall back to the browser challenge.
		fmt.Printf("%s %s\n", subtle("Could not sign in with your SSH key:"), perr.Error())
		fmt.Println(subtle("Opening the Incus web UI; you'll be challenged to pick an account."))
		return openBrowser(incusUI)
	}

	consume := providerBase + authnConsumePath +
		"?ticket=" + url.QueryEscape(ticket) +
		"&next=" + url.QueryEscape(incusUI)
	fmt.Printf("%s Signing in to Incus as your SSH identity…\n", success("✓"))
	return openBrowser(consume)
}

func runWebApprove(_ *cobra.Command, args []string) error {
	reqID := strings.TrimSpace(args[0])
	if reqID == "" {
		return errors.New("request id is required")
	}
	providerBase, _, keyPath, err := webEndpoints()
	if err != nil {
		return err
	}

	signer, err := loadSSHSigner(keyPath)
	if err != nil {
		return err
	}
	fp, nonce, sig, err := signNonce(providerBase, signer)
	if err != nil {
		return err
	}

	form := url.Values{
		"request_id":  {reqID},
		"fingerprint": {fp},
		"nonce":       {nonce},
		"signature":   {sig},
	}
	if err := postForm(providerBase+authnApprovePath, form, nil); err != nil {
		return fmt.Errorf("approve request: %w", err)
	}
	fmt.Printf("%s Approved sign-in request %s as %s\n", success("✓"), value(reqID), value(fp))
	fmt.Println(subtle("Return to your browser; it will complete sign-in automatically."))
	return nil
}

// proveAndGetTicket performs the SSH-key proof and returns a one-time consume
// ticket for the browser session bridge.
func proveAndGetTicket(providerBase, keyPath string) (string, error) {
	signer, err := loadSSHSigner(keyPath)
	if err != nil {
		return "", err
	}
	fp, nonce, sig, err := signNonce(providerBase, signer)
	if err != nil {
		return "", err
	}
	form := url.Values{"fingerprint": {fp}, "nonce": {nonce}, "signature": {sig}}
	var resp struct {
		Ticket string `json:"ticket"`
	}
	if err := postForm(providerBase+authnExchangePath, form, &resp); err != nil {
		return "", err
	}
	if resp.Ticket == "" {
		return "", errors.New("provider returned an empty ticket")
	}
	return resp.Ticket, nil
}

// signNonce fetches a fresh nonce from the provider and signs it with signer,
// returning the key fingerprint, the nonce and the base64url signature blob.
func signNonce(providerBase string, signer ssh.Signer) (fingerprint, nonce, sigB64 string, err error) {
	var nr struct {
		Nonce string `json:"nonce"`
	}
	if err = httpGetJSON(providerBase+authnNoncePath, &nr); err != nil {
		return "", "", "", fmt.Errorf("get nonce: %w", err)
	}
	if nr.Nonce == "" {
		return "", "", "", errors.New("provider returned an empty nonce")
	}
	nonceBytes, err := base64.RawURLEncoding.DecodeString(nr.Nonce)
	if err != nil {
		return "", "", "", fmt.Errorf("decode nonce: %w", err)
	}
	sig, err := signer.Sign(rand.Reader, nonceBytes)
	if err != nil {
		return "", "", "", fmt.Errorf("sign nonce: %w", err)
	}
	fingerprint = oidc.FingerprintPublicKey(signer.PublicKey())
	sigB64 = base64.RawURLEncoding.EncodeToString(ssh.Marshal(sig))
	return fingerprint, nr.Nonce, sigB64, nil
}

func loadSSHSigner(keyPath string) (ssh.Signer, error) {
	if keyPath == "" {
		return nil, errors.New("no SSH private key is configured for this VM")
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		var pmErr *ssh.PassphraseMissingError
		if errors.As(err, &pmErr) {
			return nil, fmt.Errorf("SSH key %s is passphrase-protected; ssh-agent support is not yet implemented", keyPath)
		}
		return nil, fmt.Errorf("parse SSH key %s: %w", keyPath, err)
	}
	return signer, nil
}

// --- small HTTP helpers --------------------------------------------------

func httpClient() *http.Client { return &http.Client{Timeout: webHTTPTimeout} }

func httpGetJSON(rawURL string, out any) error {
	resp, err := httpClient().Get(rawURL) //nolint:noctx // short-lived loopback CLI call
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeOrError(resp, out)
}

func postForm(rawURL string, form url.Values, out any) error {
	resp, err := httpClient().PostForm(rawURL, form) //nolint:noctx // short-lived loopback CLI call
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeOrError(resp, out)
}

// decodeOrError turns a non-2xx response into the provider's error_description,
// otherwise decodes the body into out (when non-nil).
func decodeOrError(resp *http.Response, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Desc != "" {
			return fmt.Errorf("%s (%s)", e.Desc, e.Error)
		}
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// openBrowser launches the platform browser for target, printing the URL as a
// fallback when no opener is available.
func openBrowser(target string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		fmt.Printf("Open this URL in your browser:\n  %s\n", target)
		return nil
	}
	fmt.Printf("%s %s\n", subtle("Opening"), value(target))
	if err := exec.CommandContext(context.Background(), name, target).Start(); err != nil {
		fmt.Printf("Could not launch a browser; open this URL manually:\n  %s\n", target)
	}
	return nil
}
