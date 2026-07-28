package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var spinnerFrames = []string{"|", "/", "-", "\\"}

// ByteProgress tracks long byte-stream operations such as downloads/copies.
type ByteProgress struct {
	label string
	total int64

	mu sync.Mutex

	start        time.Time
	written      int64
	lastRender   time.Time
	nextLogPct   int
	nextUnknown  time.Time
	spinnerFrame int
	finished     bool

	interactive bool
	out         io.Writer
}

// NewByteProgress starts a byte-count progress line for label. The result is
// an io.Writer: attach it to the copy you want measured (as the second half of
// an io.MultiWriter, or the writer of an io.TeeReader) and every Write advances
// the bar by the bytes that passed through. It measures the stream, not the
// clock — use TimedProgress for a wait that moves no bytes.
//
// total is the expected size. Pass a non-positive value when the size is not
// known, such as a response with no Content-Length, and the line renders a
// spinner with a running count and rate instead of a bar.
//
// Whether stdout is a terminal is decided once, here. In a non-interactive run
// nothing is drawn and progress reaches the log only: at each 10% when total is
// known, every 10 seconds when it is not. The caller must end the line with
// Finish or Fail; until then, Write keeps redrawing it.
func NewByteProgress(label string, total int64) *ByteProgress {
	interactive := term.IsTerminal(int(os.Stdout.Fd()))
	return &ByteProgress{
		label:       label,
		total:       total,
		start:       time.Now(),
		nextLogPct:  10,
		nextUnknown: time.Now().Add(10 * time.Second),
		interactive: interactive,
		out:         os.Stdout,
	}
}

func (p *ByteProgress) Write(b []byte) (int, error) {
	n := len(b)
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.finished {
		return n, nil
	}

	p.written += int64(n)
	p.maybeRenderLocked(false)
	p.maybeLogLocked()
	return n, nil
}

// Finish ends the line for a transfer that completed. It forces a final
// repaint, so the bar shows the true last byte count rather than whatever the
// 150ms render throttle last let through, closes the line with a newline so
// following output does not land on top of it, and logs "task complete" at
// info level with the total and the elapsed time.
//
// Finish and Fail are one-shot and mutually exclusive: the first of them to run
// wins, and later calls — plus any further Write — are ignored. Safe to call
// from any goroutine.
func (p *ByteProgress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	p.maybeRenderLocked(true)
	p.logCompletionLocked(nil)
}

// Fail ends the line for a transfer that broke partway. It repaints and closes
// the line exactly as Finish does — a dangling half-drawn bar is worse than a
// wrong one — but logs at error level with err and the byte count reached. That
// count is the evidence worth keeping: how far the copy got before it failed
// separates a dead link from a truncated one.
//
// Call it from the error path of the copy. Like Finish it is one-shot and safe
// from any goroutine, and whichever of the two runs first is the one that logs.
func (p *ByteProgress) Fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	p.maybeRenderLocked(true)
	p.logCompletionLocked(err)
}

func (p *ByteProgress) maybeRenderLocked(force bool) {
	if !p.interactive {
		return
	}
	if !force && time.Since(p.lastRender) < 150*time.Millisecond {
		return
	}

	elapsed := time.Since(p.start)
	speed := int64(0)
	if elapsed > 0 {
		speed = int64(float64(p.written) / elapsed.Seconds())
	}

	if p.total > 0 {
		fraction := float64(p.written) / float64(p.total)
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}

		line := fmt.Sprintf(
			"\r%s %s %s/%s %s/s",
			p.label,
			renderBar(fraction, 34),
			humanBytes(p.written),
			humanBytes(p.total),
			humanBytes(speed),
		)
		fmt.Fprint(p.out, line)
	} else {
		frame := spinnerFrames[p.spinnerFrame%len(spinnerFrames)]
		p.spinnerFrame++
		line := fmt.Sprintf("\r%s %s %s downloaded %s/s", frame, p.label, humanBytes(p.written), humanBytes(speed))
		fmt.Fprint(p.out, line)
	}

	if force {
		fmt.Fprint(p.out, "\n")
	}

	p.lastRender = time.Now()
}

func (p *ByteProgress) maybeLogLocked() {
	elapsed := time.Since(p.start)
	if p.total > 0 {
		percent := int(float64(p.written) * 100 / float64(p.total))
		if percent >= p.nextLogPct {
			L().Info("progress", "task", p.label, "percent", percent, "written", humanBytes(p.written), "total", humanBytes(p.total), "elapsed", elapsed.Round(time.Second).String())
			for p.nextLogPct <= percent {
				p.nextLogPct += 10
			}
		}
		return
	}

	if time.Now().After(p.nextUnknown) {
		L().Info("progress", "task", p.label, "written", humanBytes(p.written), "elapsed", elapsed.Round(time.Second).String())
		p.nextUnknown = time.Now().Add(10 * time.Second)
	}
}

func (p *ByteProgress) logCompletionLocked(err error) {
	elapsed := time.Since(p.start)
	if err != nil {
		L().Error("task failed", "task", p.label, "written", humanBytes(p.written), "elapsed", elapsed.Round(time.Millisecond).String(), "err", err)
		return
	}

	if p.total > 0 {
		L().Info("task complete", "task", p.label, "written", humanBytes(p.written), "total", humanBytes(p.total), "elapsed", elapsed.Round(time.Millisecond).String())
		return
	}
	L().Info("task complete", "task", p.label, "written", humanBytes(p.written), "elapsed", elapsed.Round(time.Millisecond).String())
}

// TimedProgress tracks waiting operations with a timeout budget.
type TimedProgress struct {
	label   string
	timeout time.Duration
	start   time.Time

	mu     sync.Mutex
	status string
	done   chan struct{}
	once   sync.Once

	frame       int
	interactive bool
	out         io.Writer
}

// NewTimedProgress starts an elapsed-time progress line for label and the
// goroutine that repaints it every 700ms. Use it for a wait, where the work
// offers nothing to measure — an API that is not up yet moves no bytes — as
// opposed to ByteProgress, which advances only when a stream does. Because the
// clock drives it, the line keeps moving even when the thing being waited on is
// completely stuck, which is the point.
//
// timeout is the budget the caller is waiting under. It is used only to draw
// the bar as elapsed/timeout and cancels nothing; enforcing the budget stays
// with the caller's context. Pass 0 for an unbounded wait and the line renders
// a spinner instead of a bar.
//
// The caller MUST end it with Finish or Fail: the repaint goroutine runs until
// one of them closes the done channel. Drawing happens only when stdout is a
// terminal, decided once here; otherwise the ticks are silent and only the
// final state reaches the log.
func NewTimedProgress(label string, timeout time.Duration) *TimedProgress {
	tp := &TimedProgress{
		label:       label,
		timeout:     timeout,
		start:       time.Now(),
		done:        make(chan struct{}),
		interactive: term.IsTerminal(int(os.Stdout.Fd())),
		out:         os.Stdout,
	}

	go tp.loop()
	return tp
}

func (t *TimedProgress) loop() {
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.render(false)
		}
	}
}

// SetStatus replaces the trailing text of the line — the part that says what
// the wait is currently blocked on, such as "attempt=3 connection refused".
// Without it a long wait shows only that time is passing; with it the line
// carries the reason.
//
// It just stores the string under the lock; the repaint goroutine picks it up
// on its next tick, so calling it on every retry is cheap and it is safe from
// any goroutine. A blank status renders as "in progress" rather than an empty
// gap.
func (t *TimedProgress) SetStatus(status string) {
	t.mu.Lock()
	t.status = status
	t.mu.Unlock()
}

// Finish ends a wait that succeeded. It stops the repaint goroutine, paints the
// line once more with whatever SetStatus left on it, closes the line with a
// newline, and logs "wait complete" at info level with the elapsed time.
// Callers usually SetStatus("ready") first so the frame that stays on screen
// reads as success.
//
// The goroutine is stopped under a sync.Once, so a repeat call cannot panic on
// a closed channel — but the repaint and the log are NOT suppressed, and Finish
// and Fail share that Once. Call exactly one of them, exactly once, or the log
// will show a wait that both completed and failed.
func (t *TimedProgress) Finish() {
	t.once.Do(func() { close(t.done) })
	t.render(true)
	if t.interactive {
		fmt.Fprint(t.out, "\n")
	}
	L().Info("wait complete", "task", t.label, "elapsed", time.Since(t.start).Round(time.Millisecond).String())
}

// Fail ends a wait that did not succeed. It stops the repaint goroutine and
// closes the line exactly as Finish does, but logs at error level with err and
// the elapsed time — the pair a later reader needs to tell an exhausted budget
// from a cancellation that arrived early.
//
// The same one-shot caveat as Finish applies: the goroutine is stopped only
// once, but the render and the error log run on every call, so call Finish or
// Fail, not both.
func (t *TimedProgress) Fail(err error) {
	t.once.Do(func() { close(t.done) })
	t.render(true)
	if t.interactive {
		fmt.Fprint(t.out, "\n")
	}
	L().Error("wait failed", "task", t.label, "elapsed", time.Since(t.start).Round(time.Millisecond).String(), "err", err)
}

func (t *TimedProgress) render(force bool) {
	t.mu.Lock()
	status := t.status
	t.mu.Unlock()

	if !t.interactive {
		if force {
			L().Info("wait status", "task", t.label, "status", status, "elapsed", time.Since(t.start).Round(time.Second).String())
		}
		return
	}

	elapsed := time.Since(t.start)
	status = strings.TrimSpace(status)
	if status == "" {
		status = "in progress"
	}

	if t.timeout > 0 {
		fraction := float64(elapsed) / float64(t.timeout)
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}
		line := fmt.Sprintf("\r%s %s %s/%s %s", t.label, renderBar(fraction, 34), elapsed.Round(time.Second), t.timeout.Round(time.Second), status)
		fmt.Fprint(t.out, line)
		return
	}

	frame := spinnerFrames[t.frame%len(spinnerFrames)]
	t.frame++
	line := fmt.Sprintf("\r%s %s %s", frame, t.label, status)
	fmt.Fprint(t.out, line)
}

func renderBar(fraction float64, width int) string {
	if width < 10 {
		width = 10
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}

	full := min(int(fraction*float64(width)), width)
	empty := width - full
	return fmt.Sprintf("[%s%s] %3.0f%%", strings.Repeat("#", full), strings.Repeat("-", empty), fraction*100)
}

func humanBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%dB", v)
	}

	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(v)/float64(div), "KMGTPE"[exp])
}
