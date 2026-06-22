package webproxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeneratesCertWith127001SAN(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "proxy.crt")
	keyPath := filepath.Join(dir, "proxy.key")

	p, err := New(Options{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: "127.0.0.1:18443",
		CertPath:     certPath,
		KeyPath:      keyPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pemBytes, err := p.CertPEM()
	if err != nil {
		t.Fatalf("CertPEM: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("decode cert PEM: no block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	var found bool
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cert IPAddresses %v missing 127.0.0.1", cert.IPAddresses)
	}
}

func TestReusesExistingCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "proxy.crt")
	keyPath := filepath.Join(dir, "proxy.key")

	opts := Options{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: "127.0.0.1:18443",
		CertPath:     certPath,
		KeyPath:      keyPath,
	}

	if _, err := New(opts); err != nil {
		t.Fatalf("New (first): %v", err)
	}
	first, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert after first New: %v", err)
	}

	if _, err := New(opts); err != nil {
		t.Fatalf("New (second): %v", err)
	}
	second, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert after second New: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("cert was regenerated on second New; bytes changed")
	}
}

func TestProxiesPreservingHostAndNoClientCert(t *testing.T) {
	var gotHost string
	var gotPeerCerts int

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if r.TLS != nil {
			gotPeerCerts = len(r.TLS.PeerCertificates)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	// Request (but do not require) a client cert so that a client presenting
	// one would show up in PeerCertificates; we assert it presents none.
	upstream.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	upstream.StartTLS()
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	dir := t.TempDir()
	p, err := New(Options{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: u.Host,
		CertPath:     filepath.Join(dir, "proxy.crt"),
		KeyPath:      filepath.Join(dir, "proxy.key"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	addr := p.listenAddr()
	if addr == "" {
		t.Fatalf("listen addr empty after Start")
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client to self-signed proxy
		},
		Timeout: 10 * time.Second,
	}

	const sentHost = "br-web.local"
	req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = sentHost

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", string(body), "ok")
	}
	if gotHost != sentHost {
		t.Fatalf("upstream saw Host %q, want %q (Host not preserved)", gotHost, sentHost)
	}
	if gotPeerCerts != 0 {
		t.Fatalf("upstream saw %d client certs, want 0 (proxy must present none)", gotPeerCerts)
	}
}
