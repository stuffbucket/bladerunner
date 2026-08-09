package imagebuild

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

// The base image fetch used http.DefaultClient, which sets no Timeout, and
// copied the body with no per-read bound. The outer 30-minute context deadline
// was the only thing between a wedged mirror and a build that appeared to be
// working — half an hour of a progress-free download reported as progress
// (#282). The bound that matters for a several-hundred-megabyte body is on
// SILENCE, not on total duration.

// stallTestDeadline caps the wait, so a regression fails rather than hangs.
const stallTestDeadline = 15 * time.Second

// stallTestBudget is the shortened production budget the test installs.
const stallTestBudget = 250 * time.Millisecond

// A mirror that sends headers and then goes quiet must be abandoned, and the
// failure must say the transfer stalled rather than blaming a cancellation.
func TestFetchBaseAbandonsAMirrorThatStopsSending(t *testing.T) {
	r := testRelease()
	pinFor(t, r.FileName(), sha512Hex([]byte("a pretend qcow2")))

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	// The handler must be released before Close, which waits for outstanding
	// requests.
	defer srv.Close()
	defer close(release)

	prev := downloadStallTimeout
	downloadStallTimeout = stallTestBudget
	t.Cleanup(func() { downloadStallTimeout = prev })

	dest := filepath.Join(t.TempDir(), "base.qcow2")
	done := make(chan error, 1)
	go func() { done <- FetchBase(t.Context(), r, dest, srv.URL) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FetchBase accepted an image the mirror never finished sending")
		}
		if !errors.Is(err, httpfetch.ErrStalled) {
			t.Fatalf("error %v does not wrap httpfetch.ErrStalled", err)
		}
	case <-time.After(stallTestDeadline):
		t.Fatal("FetchBase never returned: a wedged mirror still hangs a build")
	}

	if _, err := os.Stat(dest); err == nil {
		t.Error("a stalled download was moved into place")
	}
}
