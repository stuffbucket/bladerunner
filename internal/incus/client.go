package incus

import (
	"context"
	"fmt"
	"os"
	"time"

	incusclient "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	sharedtls "github.com/lxc/incus/v6/shared/tls"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

type ServerInfo struct {
	ServerVersion string
	APIVersion    string
	Auth          string
	Addresses     []string
	ServerName    string
	APIExtensions int
}

type WaitProgress struct {
	Attempt   int
	Elapsed   time.Duration
	LastError error
}

type WaitProgressCallback func(WaitProgress)

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

func WaitForServer(ctx context.Context, endpoint string, certPEM, keyPEM []byte, retryEvery time.Duration, cb WaitProgressCallback) (*ServerInfo, error) {
	ticker := time.NewTicker(retryEvery)
	defer ticker.Stop()
	start := time.Now()
	attempt := 0

	logging.L().Info("waiting for Incus API readiness", "endpoint", endpoint, "retry_every", retryEvery.String())

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
			waitErr := fmt.Errorf("wait for incus server: %w", ctx.Err())
			logging.L().Error("Incus API readiness timed out", "endpoint", endpoint, "attempts", attempt, "elapsed", time.Since(start).Round(time.Second).String(), "err", waitErr)
			return nil, fmt.Errorf("wait for incus server: %w", ctx.Err())
		case <-ticker.C:
		}
	}
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

	return toServerInfo(server), nil
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
