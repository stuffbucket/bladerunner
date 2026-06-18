// Package webproxy provides a host-side HTTPS reverse proxy that terminates
// the browser's TLS without requesting a client certificate (so browsers never
// show the "select a certificate" picker) and forwards every request to a local
// Incus HTTPS endpoint.
//
// The proxy deliberately presents no client certificate to Incus either, so
// Incus authenticates the browser session via OIDC rather than a cert. It also
// preserves the inbound Host header end-to-end (Incus builds its OIDC
// redirect_uri from the request Host) and transparently supports the WebSocket
// upgrades the Incus UI uses for instance consoles and terminals.
package webproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

const (
	// responseHeaderTimeout bounds how long the upstream may take to send
	// response headers; long-lived streaming bodies are unaffected.
	responseHeaderTimeout = 60 * time.Second
	// readHeaderTimeout bounds slow-header (Slowloris) clients on the browser side.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout bounds graceful shutdown.
	shutdownTimeout = 5 * time.Second
	// serialBits is the bit length of the random certificate serial number.
	serialBits = 128

	certFilePerm os.FileMode = 0o644
	keyFilePerm  os.FileMode = 0o600
	dirPerm      os.FileMode = 0o755
)

// Options configures the proxy.
type Options struct {
	ListenAddr   string // host:port the browser connects to, e.g. "127.0.0.1:18444"
	UpstreamAddr string // Incus HTTPS host:port to forward to, e.g. "127.0.0.1:18443"
	CertPath     string // PEM cert path; generated (self-signed) if missing
	KeyPath      string // PEM key path; generated if missing
}

// Proxy is a running (or ready-to-run) HTTPS reverse proxy.
type Proxy struct {
	opts    Options
	srv     *http.Server
	certPEM []byte

	mu       sync.Mutex
	ln       net.Listener
	listenAt string // actual listen addr, populated after Start
}

// New loads-or-generates the self-signed server cert at CertPath/KeyPath and
// builds the reverse proxy. It does not start listening.
func New(opts Options) (*Proxy, error) {
	if opts.ListenAddr == "" {
		return nil, fmt.Errorf("webproxy: ListenAddr is required")
	}
	if opts.UpstreamAddr == "" {
		return nil, fmt.Errorf("webproxy: UpstreamAddr is required")
	}
	if opts.CertPath == "" || opts.KeyPath == "" {
		return nil, fmt.Errorf("webproxy: CertPath and KeyPath are required")
	}

	certPEM, keyPEM, err := loadOrGenerateCert(opts.CertPath, opts.KeyPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("webproxy: parse keypair: %w", err)
	}

	// Upstream transport: a loopback hop to our own Incus with a self-signed
	// cert, so InsecureSkipVerify is intended. Crucially it presents no client
	// certificate (no Certificates field) so Incus falls back to OIDC.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // loopback hop to our own self-signed Incus
			MinVersion:         tls.VersionTLS12,
		},
		ResponseHeaderTimeout: responseHeaderTimeout,
		// No overall timeout: long-lived WebSocket/streaming connections must
		// not be killed.
	}

	upstream := &url.URL{Scheme: "https", Host: opts.UpstreamAddr}
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Point the outbound request at the upstream, but keep req.Host
			// as the browser sent it so Incus's OIDC redirect_uri stays
			// consistent with the browser's origin.
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			// Do NOT overwrite req.Host; ReverseProxy forwards it as the
			// outbound Host header when it differs from req.URL.Host.
		},
		Transport:     transport,
		FlushInterval: -1, // stream responses immediately (SSE, consoles)
	}

	// Server TLS config. ClientAuth: tls.NoClientCert is the whole point: it
	// stops the browser from prompting the user to select a certificate.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS12,
	}

	srv := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           rp,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	return &Proxy{
		opts:    opts,
		srv:     srv,
		certPEM: certPEM,
	}, nil
}

// Start begins serving HTTPS in a background goroutine. Non-blocking.
func (p *Proxy) Start() error {
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", p.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("webproxy: listen on %s: %w", p.opts.ListenAddr, err)
	}

	p.mu.Lock()
	p.ln = ln
	p.listenAt = ln.Addr().String()
	p.mu.Unlock()

	logging.L().Info("web proxy listening", "addr", p.listenAt, "upstream", p.opts.UpstreamAddr)

	go func() {
		// Certs already live in TLSConfig, so the file args are empty.
		if err := p.srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			logging.L().Error("web proxy serve stopped", "err", err)
		}
	}()

	return nil
}

// Close gracefully shuts the server down.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

// CertPEM returns the server certificate as PEM (so callers can add it to the
// OS trust store).
func (p *Proxy) CertPEM() ([]byte, error) {
	if len(p.certPEM) == 0 {
		return nil, fmt.Errorf("webproxy: no certificate available")
	}
	out := make([]byte, len(p.certPEM))
	copy(out, p.certPEM)
	return out, nil
}

// listenAddr reports the actual address the proxy is serving on after Start.
// Unexported helper used by tests that listen on an ephemeral :0 port.
func (p *Proxy) listenAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listenAt
}

// loadOrGenerateCert returns the cert and key PEM bytes, reusing the files at
// certPath/keyPath when both exist and parse as a valid keypair, otherwise
// generating a fresh self-signed leaf and persisting it atomically.
func loadOrGenerateCert(certPath, keyPath string) (certPEM, keyPEM []byte, err error) {
	if cp, kp, ok := tryLoadCert(certPath, keyPath); ok {
		return cp, kp, nil
	}

	certPEM, keyPEM, err = generateCert()
	if err != nil {
		return nil, nil, err
	}

	if err := writeCertFiles(certPath, keyPath, certPEM, keyPEM); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// tryLoadCert reads and validates an existing keypair. ok is false if either
// file is missing/unreadable or they do not form a valid keypair.
func tryLoadCert(certPath, keyPath string) (certPEM, keyPEM []byte, ok bool) {
	cp, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, false
	}
	kp, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, false
	}
	if _, err := tls.X509KeyPair(cp, kp); err != nil {
		return nil, nil, false
	}
	return cp, kp, true
}

// generateCert builds a self-signed ECDSA P-256 leaf certificate suitable for
// direct keychain trust by the browser.
func generateCert() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("webproxy: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, nil, fmt.Errorf("webproxy: serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "bladerunner web proxy"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, //nolint:mnd // IPv4 loopback octets
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("webproxy: create certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("webproxy: marshal key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// writeCertFiles persists the cert (0644) and key (0600) atomically via
// temp-file + rename, creating the parent directory (0755) if needed.
func writeCertFiles(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(filepath.Dir(certPath), dirPerm); err != nil {
		return fmt.Errorf("webproxy: mkdir cert dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), dirPerm); err != nil {
		return fmt.Errorf("webproxy: mkdir key dir: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, certFilePerm); err != nil {
		return fmt.Errorf("webproxy: write cert: %w", err)
	}
	if err := writeFileAtomic(keyPath, keyPEM, keyFilePerm); err != nil {
		return fmt.Errorf("webproxy: write key: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in the destination's directory and
// renames it into place so readers never observe a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
