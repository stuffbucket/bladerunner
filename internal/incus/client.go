package incus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	incusclient "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	sharedtls "github.com/lxc/incus/v6/shared/tls"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// ServerInfo is the part of an Incus GetServer response that bladerunner keeps:
// enough to identify the server in the startup report, and Auth, which is what
// readiness is actually decided on (see checkAuthorized). It is a flat copy so
// that callers — including the JSON report written to disk — do not carry a
// dependency on the Incus API types.
//
// A zero value is what toServerInfo returns for an empty response, so an
// unpopulated ServerInfo means "the server told us nothing", not "no server".
type ServerInfo struct {
	ServerVersion string
	APIVersion    string
	Auth          string
	Addresses     []string
	ServerName    string
	APIExtensions int
}

// WaitProgress describes one failed readiness probe: which attempt it was, how
// long the whole wait has run so far, and the error that probe returned. It
// exists so a UI can say WHY the wait is still going, not merely that it is —
// "connection refused" and "not authorized yet" are different problems.
type WaitProgress struct {
	Attempt   int
	Elapsed   time.Duration
	LastError error
}

// WaitProgressCallback observes a wait in flight. WaitForServer calls it once
// per FAILED probe and never on success, so the last thing a callback sees is
// the last failure, not the outcome.
//
// It runs on the goroutine driving the wait, between probes, so it must return
// promptly and must not block. A nil callback is valid and means "no progress
// reporting"; the wait still logs either way.
type WaitProgressCallback func(WaitProgress)

// EnsureClientCertificate returns the PEM-encoded client certificate and key
// used to talk to Incus, generating a self-signed pair at certPath/keyPath on
// first use.
//
// It is idempotent: an existing pair is read back unchanged. That matters
// because the guest's Incus trust store holds the fingerprint of this exact
// certificate — regenerating it would leave the API answering but refusing to
// authorize us, which is the failure WaitForServer reports as "not authorized
// yet". The PEM bytes are returned rather than the paths because the Incus
// client and WaitForServer take them in memory.
func EnsureClientCertificate(certPath, keyPath string) ([]byte, []byte, error) {
	if err := sharedtls.FindOrGenCert(certPath, keyPath, true, false); err != nil {
		return nil, nil, fmt.Errorf("create/load client cert: %w", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read client key: %w", err)
	}

	logging.L().Info("client TLS credentials ready", "cert", certPath, "key", keyPath)
	return certPEM, keyPEM, nil
}

// WaitForServer probes endpoint every retryEvery until Incus both answers and
// reports this client as trusted, then returns what the server said about
// itself. "It answered" is not readiness on its own: GetServer replies to an
// untrusted client too, so gating on a bare response reports a half-started VM
// as ready. See checkAuthorized.
//
// ctx carries the entire budget for the wait. This function adds no timeout of
// its own and never shortens what the caller gave it; the caller (vmhost, from
// --timeout or the persisted settings) owns that decision. When ctx ends,
// waitEnded distinguishes an exhausted budget from a cancellation and says
// which in the returned error, carrying the last probe error with it — that
// error is the only evidence about the guest.
//
// cb, when non-nil, is called after each failed probe; see
// WaitProgressCallback for the constraints on it. Failures are also logged, on
// the first attempt and then every fifth, so that a long wait does not fill the
// log with the same line.
func WaitForServer(ctx context.Context, endpoint string, certPEM, keyPEM []byte, retryEvery time.Duration, cb WaitProgressCallback) (*ServerInfo, error) {
	ticker := time.NewTicker(retryEvery)
	defer ticker.Stop()
	start := time.Now()
	attempt := 0

	// The budget is logged because it is the only way a reader of this log can
	// tell what --timeout was actually in force. Without it, a wait that ends
	// early is impossible to attribute from the log alone.
	logging.L().Info("waiting for Incus API readiness", "endpoint", endpoint, "retry_every", retryEvery.String(), "budget", budgetOf(ctx))

	for {
		attempt++
		info, err := connectAndGet(endpoint, certPEM, keyPEM)
		if err == nil {
			logging.L().Info("Incus API ready", "endpoint", endpoint, "attempts", attempt, "elapsed", time.Since(start).Round(time.Millisecond).String())
			return info, nil
		}
		if cb != nil {
			cb(WaitProgress{
				Attempt:   attempt,
				Elapsed:   time.Since(start),
				LastError: err,
			})
		}

		if attempt == 1 || attempt%5 == 0 {
			logging.L().Warn("Incus API not ready yet", "attempt", attempt, "elapsed", time.Since(start).Round(time.Second).String(), "err", err)
		}

		select {
		case <-ctx.Done():
			return nil, waitEnded(ctx, endpoint, attempt, time.Since(start), err)
		case <-ticker.C:
		}
	}
}

// budgetOf renders the wait budget the caller bounded ctx with, or "none" for an
// unbounded context.
func budgetOf(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "none"
	}
	return time.Until(deadline).Round(time.Second).String()
}

// waitEnded turns a finished wait into a log line and an error that say WHICH of
// the two possible endings happened.
//
// They are not the same event and they must not read the same way:
//
//   - the budget ran out (context.DeadlineExceeded). The guest never got Incus
//     up in the time it was given. Acting on it means raising --timeout or
//     fixing the guest.
//   - the wait was canceled (context.Canceled). Something released the run — a
//     signal, a `br stop`, a killed process group — while the guest was still
//     coming up. The budget is irrelevant; raising --timeout changes nothing.
//
// Both used to be logged as "Incus API readiness timed out" with an err of
// "context canceled", which reports a timeout that did not happen and hides a
// cancellation that did. The last probe error is carried into the returned error
// too: it is the one piece of evidence about the guest, and it used to be
// dropped on the floor at exactly the moment it mattered.
func waitEnded(ctx context.Context, endpoint string, attempts int, elapsed time.Duration, lastErr error) error {
	elapsedStr := elapsed.Round(time.Second).String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		logging.L().Error("Incus API readiness timed out",
			"endpoint", endpoint, "attempts", attempts, "elapsed", elapsedStr, "last_probe_error", lastErr)
		return fmt.Errorf("wait for incus server: budget exhausted after %s and %d attempts%s: %w",
			elapsedStr, attempts, lastProbeSuffix(lastErr), ctx.Err())
	}
	logging.L().Error("Incus API readiness canceled before the budget ran out",
		"endpoint", endpoint, "attempts", attempts, "elapsed", elapsedStr,
		"cause", causeOf(ctx), "last_probe_error", lastErr)
	return fmt.Errorf("wait for incus server: canceled after %s and %d attempts (cause: %w)%s: %w",
		elapsedStr, attempts, causeOf(ctx), lastProbeSuffix(lastErr), ctx.Err())
}

// causeOf is context.Cause with a floor: a context canceled without one still
// reports something, so the message never renders a nil.
func causeOf(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// lastProbeSuffix appends the last probe failure to a wait error, or nothing
// when the wait ended before any probe failed.
func lastProbeSuffix(lastErr error) string {
	if lastErr == nil {
		return ""
	}
	return fmt.Sprintf("; last probe error: %v", lastErr)
}

func connectAndGet(endpoint string, certPEM, keyPEM []byte) (*ServerInfo, error) {
	client, err := incusclient.ConnectIncus(endpoint, &incusclient.ConnectionArgs{
		TLSClientCert:      string(certPEM),
		TLSClientKey:       string(keyPEM),
		InsecureSkipVerify: true,
		SkipGetEvents:      true,
	})
	if err != nil {
		return nil, err
	}

	server, _, err := client.GetServer()
	if err != nil {
		return nil, err
	}

	if err := checkAuthorized(server); err != nil {
		return nil, err
	}

	return toServerInfo(server), nil
}

// authTrusted is the Auth value Incus reports once it has accepted the client's
// certificate into its trust store. Any other value (notably "untrusted") means
// the API answers but has not yet authorized us.
const authTrusted = "trusted"

// checkAuthorized reports whether the Incus server considers this client
// authorized. GetServer answers even for an untrusted client (with a
// stripped-down response and Auth=="untrusted"), so readiness must gate on the
// trust state: a plain "GetServer responded" check hides trust drift (the cert
// never got added to the trust store) and reports a half-started VM as ready.
// See runner_darwin WaitForIncus.
func checkAuthorized(server *api.Server) error {
	if server == nil {
		return fmt.Errorf("incus server response was empty")
	}
	if server.Auth != authTrusted {
		return fmt.Errorf("incus client not authorized yet (auth=%q)", server.Auth)
	}
	return nil
}

func toServerInfo(server *api.Server) *ServerInfo {
	if server == nil {
		return &ServerInfo{}
	}

	info := &ServerInfo{}
	info.ServerVersion = server.Environment.ServerVersion
	info.APIVersion = server.APIVersion
	info.Auth = server.Auth
	info.ServerName = server.Environment.ServerName
	info.APIExtensions = len(server.APIExtensions)
	if len(server.Environment.Addresses) > 0 {
		info.Addresses = append(info.Addresses, server.Environment.Addresses...)
	}

	return info
}
