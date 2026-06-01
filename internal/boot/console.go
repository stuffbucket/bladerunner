// Package boot provides console log parsing and boot diagnostics.
package boot

import (
	"bufio"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Event is a single observation made while tailing a console log. Line is the
// raw text (no trailing newline). Status is the accumulated boot status as of
// this line — a stable snapshot the caller may keep references to.
type Event struct {
	Line   string
	Status Status
}

const maxErrorLength = 200

// Status represents the detected boot state from console output.
type Status struct {
	KernelBooted    bool
	SystemdReached  bool
	CloudInitDone   bool
	CloudInitFailed bool
	SSHReady        bool
	IncusReady      bool
	LoginPrompt     bool
	KernelPanic     bool
	EmergencyMode   bool

	// Errors detected during boot
	Errors []string

	// LastActivity timestamp detected
	LastActivity time.Time
}

// Healthy returns true if boot completed without critical failures.
func (s *Status) Healthy() bool {
	return s.CloudInitDone && !s.CloudInitFailed && !s.KernelPanic && !s.EmergencyMode
}

// Summary returns a human-readable summary of boot status.
func (s *Status) Summary() string {
	if s.KernelPanic {
		return "kernel panic detected"
	}
	if s.EmergencyMode {
		return "systemd emergency mode"
	}
	if s.CloudInitFailed {
		return "cloud-init failed"
	}
	if !s.KernelBooted {
		return "kernel not booted"
	}
	if !s.SystemdReached {
		return "waiting for systemd"
	}
	if !s.CloudInitDone {
		return "cloud-init running"
	}
	if !s.SSHReady {
		return "waiting for SSH"
	}
	if !s.IncusReady {
		return "waiting for Incus"
	}
	return "boot complete"
}

// Pattern definitions for boot stage detection.
var (
	patternKernelBoot    = regexp.MustCompile(`(?i)Linux version|Booting Linux`)
	patternSystemdTarget = regexp.MustCompile(`Reached target|Started.*target`)
	patternCloudInitDone = regexp.MustCompile(`(?i)cloud-init.*final|Cloud-init.*finished|ci-info:.*up`)
	patternCloudInitFail = regexp.MustCompile(`(?i)cloud-init.*error|cloud-init.*failed|DataSource.*not found`)
	patternSSHReady      = regexp.MustCompile(`(?i)sshd.*listening|Started.*SSH|ssh\.service.*active`)
	patternIncusReady    = regexp.MustCompile(`(?i)incusd.*ready|incus.*daemon started|Started.*Incus`)
	patternLoginPrompt   = regexp.MustCompile(`(?i)login:|^[a-z]+ login:`)
	patternKernelPanic   = regexp.MustCompile(`(?i)Kernel panic|BUG:|Oops:`)
	patternEmergency     = regexp.MustCompile(`(?i)emergency\.target|You are in emergency mode|systemd-emergency`)
	patternError         = regexp.MustCompile(`(?i)\berror\b.*:|failed to|cannot|unable to`)
)

// ParseReader parses boot status from a reader (console log).
func ParseReader(r io.Reader) *Status {
	status := &Status{}
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		line := scanner.Text()
		parseLine(status, line)
	}

	return status
}

// ParseFile parses boot status from a console log file.
func ParseFile(path string) (*Status, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseReader(f), nil
}

// WatchFile watches a console log file and returns status updates.
// Stops when status.Healthy() returns true or a fatal error is detected.
func WatchFile(path string, pollInterval time.Duration) <-chan *Status {
	ch := make(chan *Status, 1)

	go func() {
		defer close(ch)

		var lastSize int64
		status := &Status{}

		for {
			info, err := os.Stat(path)
			if err != nil {
				time.Sleep(pollInterval)
				continue
			}

			if info.Size() > lastSize {
				f, err := os.Open(path)
				if err != nil {
					time.Sleep(pollInterval)
					continue
				}

				if lastSize > 0 {
					_, _ = f.Seek(lastSize, io.SeekStart)
				}

				scanner := bufio.NewScanner(f)
				for scanner.Scan() {
					parseLine(status, scanner.Text())
				}
				_ = f.Close()

				lastSize = info.Size()
				status.LastActivity = time.Now()

				select {
				case ch <- copyStatus(status):
				default:
				}

				if status.Healthy() || status.KernelPanic || status.EmergencyMode {
					return
				}
			}

			time.Sleep(pollInterval)
		}
	}()

	return ch
}

// WatchOptions configures WatchEvents.
type WatchOptions struct {
	// PollInterval is how often to re-stat the file. Required.
	PollInterval time.Duration
	// FromEnd seeks to the end of the file on initial open so only content
	// appended after WatchEvents starts is emitted. Useful when the log may
	// already contain stale content from a previous run.
	FromEnd bool
}

// WatchEvents tails the file at path and emits one Event per new line. The
// returned channel is closed when ctx is canceled. It blocks while waiting
// for the file to appear and recovers from truncation/rotation by reopening
// when the file shrinks.
//
// Unlike WatchFile, this function does not stop on its own — callers control
// the lifetime via ctx so they can keep streaming console output even after
// boot is "healthy" (the tail is still useful for diagnosing later failures).
func WatchEvents(ctx context.Context, path string, opts WatchOptions) <-chan Event {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	ch := make(chan Event, eventChannelBuffer)
	go runWatchEvents(ctx, path, opts, ch)
	return ch
}

const (
	eventChannelBuffer  = 64
	defaultPollInterval = 250 * time.Millisecond
	readerBufferSize    = 64 * 1024
)

type tailState struct {
	file          *os.File
	reader        *bufio.Reader
	lastSize      int64
	status        Status
	hasOpenedOnce bool
}

func (t *tailState) close() {
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
		t.reader = nil
	}
}

func runWatchEvents(ctx context.Context, path string, opts WatchOptions, ch chan<- Event) {
	defer close(ch)

	t := &tailState{}
	defer t.close()

	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	for {
		t.refreshOpen(path, opts.FromEnd)
		if !t.drainInto(ctx, ch) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *tailState) refreshOpen(path string, fromEnd bool) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if t.file != nil && info.Size() >= t.lastSize {
		return
	}
	t.close()
	f, err := os.Open(path)
	if err != nil {
		return
	}

	// On the very first open, optionally skip past existing content so the
	// caller only sees new lines appended after the watcher started.
	// Reopens triggered by truncation/rotation always start from the new
	// file's beginning regardless — that's the whole point of detecting
	// the shrink.
	var startPos int64
	if fromEnd && !t.hasOpenedOnce {
		if end, serr := f.Seek(0, io.SeekEnd); serr == nil {
			startPos = end
		}
	}
	t.hasOpenedOnce = true

	t.file = f
	t.reader = bufio.NewReaderSize(f, readerBufferSize)
	t.lastSize = startPos
}

// drainInto reads all currently-available complete lines, emitting one Event
// per line. Returns false if ctx was canceled mid-drain so the caller can
// exit. Uses bufio.Reader so it can resume reading after EOF when the file
// gets more data appended (bufio.Scanner becomes permanently done on EOF).
func (t *tailState) drainInto(ctx context.Context, ch chan<- Event) bool {
	if t.reader == nil {
		return true
	}
	for {
		line, err := t.reader.ReadString('\n')
		if line != "" {
			t.lastSize += int64(len(line))
			trimmed := strings.TrimRight(line, "\r\n")
			parseLine(&t.status, trimmed)
			snapshot := copyStatus(&t.status)
			select {
			case ch <- Event{Line: trimmed, Status: *snapshot}:
			case <-ctx.Done():
				return false
			}
		}
		if err != nil {
			// io.EOF or transient error — retry on next tick. The reader
			// will pick up newly-appended bytes without being recreated.
			break
		}
	}
	return true
}

func parseLine(status *Status, line string) {
	if patternKernelBoot.MatchString(line) {
		status.KernelBooted = true
	}
	if patternSystemdTarget.MatchString(line) {
		status.SystemdReached = true
	}
	if patternCloudInitDone.MatchString(line) {
		status.CloudInitDone = true
	}
	if patternCloudInitFail.MatchString(line) {
		status.CloudInitFailed = true
		status.Errors = append(status.Errors, extractError(line))
	}
	if patternSSHReady.MatchString(line) {
		status.SSHReady = true
	}
	if patternIncusReady.MatchString(line) {
		status.IncusReady = true
	}
	if patternLoginPrompt.MatchString(line) {
		status.LoginPrompt = true
	}
	if patternKernelPanic.MatchString(line) {
		status.KernelPanic = true
		status.Errors = append(status.Errors, extractError(line))
	}
	if patternEmergency.MatchString(line) {
		status.EmergencyMode = true
		status.Errors = append(status.Errors, extractError(line))
	}
	if len(status.Errors) < 10 && patternError.MatchString(line) {
		if !isNoiseError(line) {
			status.Errors = append(status.Errors, extractError(line))
		}
	}
}

func extractError(line string) string {
	line = strings.TrimSpace(line)
	if len(line) > maxErrorLength {
		line = line[:maxErrorLength] + "..."
	}
	return line
}

func isNoiseError(line string) bool {
	noisePatterns := []string{
		"error=0",
		"error_code=0",
		"error: 0",
		"no error",
		"success",
	}
	lower := strings.ToLower(line)
	for _, p := range noisePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func copyStatus(s *Status) *Status {
	cp := *s
	cp.Errors = make([]string, len(s.Errors))
	copy(cp.Errors, s.Errors)
	return &cp
}
