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

func (t *TimedProgress) SetStatus(status string) {
	t.mu.Lock()
	t.status = status
	t.mu.Unlock()
}

func (t *TimedProgress) Finish() {
	t.once.Do(func() { close(t.done) })
	t.render(true)
	if t.interactive {
		fmt.Fprint(t.out, "\n")
	}
	L().Info("wait complete", "task", t.label, "elapsed", time.Since(t.start).Round(time.Millisecond).String())
}

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
