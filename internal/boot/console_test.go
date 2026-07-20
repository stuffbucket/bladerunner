package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchEvents_TailsAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events := WatchEvents(ctx, path, WatchOptions{PollInterval: 20 * time.Millisecond})

	if _, err := f.WriteString("[    0.0] Linux version 6.x\n"); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := f.WriteString("[    5.0] Reached target multi-user\n"); err != nil {
		t.Fatalf("write2: %v", err)
	}
	if _, err := f.WriteString("[   10.0] cloud-init: Cloud-init finished\n"); err != nil {
		t.Fatalf("write3: %v", err)
	}

	var got []Event
	deadline := time.After(1500 * time.Millisecond)
	for len(got) < 3 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("channel closed early after %d events", len(got))
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %d events", len(got))
		}
	}

	if !got[0].Status.KernelBooted {
		t.Errorf("event[0] expected KernelBooted, got %+v", got[0].Status)
	}
	if !got[1].Status.SystemdReached {
		t.Errorf("event[1] expected SystemdReached, got %+v", got[1].Status)
	}
	if !got[2].Status.CloudInitDone {
		t.Errorf("event[2] expected CloudInitDone, got %+v", got[2].Status)
	}
	if !strings.Contains(got[2].Line, "Cloud-init finished") {
		t.Errorf("event[2] raw line lost: %q", got[2].Line)
	}
}

// TestWatchEvents_FromEnd_SkipsExistingContent verifies that pre-existing
// lines (e.g. a previous boot's shutdown sequence) are not emitted when
// FromEnd is set, while content appended after start IS emitted.
func TestWatchEvents_FromEnd_SkipsExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")

	if err := os.WriteFile(path, []byte(
		"[old] systemd-shutdown[1]: Powering off\n"+
			"[old] reboot: Power down\n",
	), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events := WatchEvents(ctx, path, WatchOptions{
		PollInterval: 20 * time.Millisecond,
		FromEnd:      true,
	})

	// Give the watcher a moment to open and seek past existing content.
	time.Sleep(80 * time.Millisecond)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.WriteString("[new] Linux version 6.x\n"); err != nil {
		t.Fatalf("write new: %v", err)
	}

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("channel closed before event")
		}
		if strings.Contains(ev.Line, "[old]") {
			t.Errorf("expected old content to be skipped, got %q", ev.Line)
		}
		if !strings.Contains(ev.Line, "[new]") {
			t.Errorf("expected new content, got %q", ev.Line)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("no event emitted for newly-appended line")
	}
}

func TestIsNoiseError(t *testing.T) {
	tests := []struct {
		line  string
		noise bool
	}{
		{"error=0", true},
		{"No error reported", true},
		{"real error: disk full", false},
		{"failed to start service", false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := isNoiseError(tt.line); got != tt.noise {
				t.Errorf("isNoiseError(%q) = %v, want %v", tt.line, got, tt.noise)
			}
		})
	}
}

func TestExtractError_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	result := extractError(long)

	if len(result) > 210 {
		t.Errorf("extractError did not truncate: len=%d", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Error("expected truncated string to end with ...")
	}
}
