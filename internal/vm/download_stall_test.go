package vm

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/httpfetch"
)

// The guest image download had no deadline and no stall detection: three call
// sites used http.DefaultClient, which sets no Timeout, and the transfer was a
// bare io.Copy with no per-read bound. A peer that completed TLS, sent headers
// and then went quiet held `br up` open forever, with a progress bar that
// looked healthy. From a terminal SIGINT recovered it; from the holder or the
// menubar there was no escape (#282).
//
// These tests are written against a local server, never the network, and every
// wait is bounded — a regression here is a HANG, and a hang that CI has to
// discover by job timeout is a bad way to learn about it.

// testNetDeadline caps every wait in this file.
const testNetDeadline = 15 * time.Second

// testNetBudget is the shortened production budget these tests install.
const testNetBudget = 250 * time.Millisecond

// quietServer answers a request the way a wedged peer does. /stall sends a 200
// with one byte and then holds the connection open in silence; /silent never
// answers at all. The handlers unblock when the client gives up, so nothing
// outlives the test.
func quietServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/silent", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// fetchSidecarSHA256 appends ".sha256" to the URL it is given, so the
	// sidecar route has to exist under that name too.
	mux.HandleFunc("/silent.sha256", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	// Release the handlers before Close: httptest.Server.Close waits for
	// outstanding requests.
	return srv, func() {
		close(release)
		srv.Close()
	}
}

// A base image download must give up on a peer that stops sending, and it must
// leave nothing behind at the destination.
func TestDownloadFileAbandonsAStalledServer(t *testing.T) {
	srv, stop := quietServer(t)
	defer stop()

	prev := downloadStallTimeout
	downloadStallTimeout = testNetBudget
	t.Cleanup(func() { downloadStallTimeout = prev })

	dest := filepath.Join(t.TempDir(), "base-image.raw")
	done := make(chan error, 1)
	go func() { done <- downloadFile(t.Context(), srv.URL+"/stall", dest) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("downloadFile reported success for a transfer that never delivered a body")
		}
		if !errors.Is(err, httpfetch.ErrStalled) {
			t.Fatalf("error %v does not wrap httpfetch.ErrStalled", err)
		}
	case <-time.After(testNetDeadline):
		t.Fatal("downloadFile never returned: a stalled peer still hangs the boot")
	}

	if _, err := os.Stat(dest); err == nil {
		t.Error("a stalled download was moved into place")
	}
	if _, err := os.Stat(dest + ".tmp"); err == nil {
		t.Error("the partial body was left behind")
	}
}

// The sidecar is the sharper case: it runs first on every cold boot, purely to
// resolve the cache key, so a stalled one hangs the boot before any output at
// all. It is small and fast, so a flat deadline is the right bound.
func TestFetchSidecarSHA256GivesUpOnASilentServer(t *testing.T) {
	srv, stop := quietServer(t)
	defer stop()

	prev := sidecarTimeout
	sidecarTimeout = testNetBudget
	t.Cleanup(func() { sidecarTimeout = prev })

	done := make(chan error, 1)
	go func() {
		_, err := fetchSidecarSHA256(t.Context(), srv.URL+"/silent")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fetchSidecarSHA256 succeeded against a server that answered nothing")
		}
	case <-time.After(testNetDeadline):
		t.Fatal("fetchSidecarSHA256 never returned: a silent sidecar host still hangs a cold boot")
	}
}
