// Package board renders a buildx-style split-view progress UI: a static panel
// of named stages on top and a live tail of recent log lines underneath. It is
// designed for the `br start` boot sequence where the user needs to see what
// the guest is doing while we wait for cloud-init and Incus to come up.
//
// When the output is not a TTY the renderer downgrades to plain structured
// log lines so the behavior in CI / log capture remains unchanged.
package board

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"golang.org/x/term"
)

// State is the lifecycle state of a stage.
type State int

const (
	StatePending State = iota
	StateRunning
	StateDone
	StateFailed
)

// Stage is a named row in the board's top panel.
type Stage struct {
	ID    string
	Label string
}

// Options configures a Board.
type Options struct {
	// Out is where the rendered frames are written. Defaults to os.Stderr.
	// The board only enables interactive rendering if Out is a *os.File whose
	// fd is a terminal (override with Interactive).
	Out io.Writer

	// Title is rendered above the stage list. Optional.
	Title string

	// TailSize is the number of console log lines kept in the tail panel.
	// Defaults to 8.
	TailSize int

	// Tick is the redraw interval while at least one stage is Running.
	// Defaults to 200ms.
	Tick time.Duration

	// ConsoleLogPath is shown in the footer so users know where to look for
	// the full log after Stop().
	ConsoleLogPath string

	// Interactive forces TTY mode on or off. When nil, autodetect from Out.
	Interactive *bool
}

// Board is a buildx-style stage + tail renderer.
type Board struct {
	out            io.Writer
	outFile        *os.File // populated when Out is a *os.File; needed for SIGWINCH re-query
	interactive    bool
	width          int
	title          string
	consoleLogPath string
	tailSize       int
	tick           time.Duration

	mu     sync.Mutex
	stages []*stageRT
	index  map[string]int
	tail   []string

	spinner   int
	started   bool
	stopCh    chan struct{}
	wg        sync.WaitGroup
	lastLines int

	resizeStop func() // installed by Start when running on a real terminal
}

type stageRT struct {
	Stage
	state     State
	started   time.Time
	finished  time.Time
	substatus string
	err       error
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	defaultTailSize     = 8
	defaultTickInterval = 200 * time.Millisecond
	defaultWidth        = 80
	labelColumn         = 28
	minDividerWidth     = 20
)

var (
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	runningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	doneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failedStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
)

// New constructs a Board with the given stages in display order.
func New(stages []Stage, opts Options) *Board {
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	if opts.TailSize <= 0 {
		opts.TailSize = defaultTailSize
	}
	if opts.Tick <= 0 {
		opts.Tick = defaultTickInterval
	}

	interactive := false
	if opts.Interactive != nil {
		interactive = *opts.Interactive
	} else if f, ok := opts.Out.(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}

	width := defaultWidth
	if f, ok := opts.Out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			width = w
		}
	}

	outFile, _ := opts.Out.(*os.File)
	b := &Board{
		out:            opts.Out,
		outFile:        outFile,
		interactive:    interactive,
		width:          width,
		title:          opts.Title,
		consoleLogPath: opts.ConsoleLogPath,
		tailSize:       opts.TailSize,
		tick:           opts.Tick,
		stages:         make([]*stageRT, 0, len(stages)),
		index:          make(map[string]int, len(stages)),
		tail:           make([]string, 0, opts.TailSize),
		stopCh:         make(chan struct{}),
	}
	for i, s := range stages {
		b.stages = append(b.stages, &stageRT{Stage: s, state: StatePending})
		b.index[s.ID] = i
	}
	return b
}

// Start enables interactive rendering. No-op in non-interactive mode.
func (b *Board) Start() {
	if !b.interactive {
		return
	}
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	fmt.Fprint(b.out, "\x1b[?25l") // hide cursor
	b.mu.Unlock()

	b.wg.Add(1)
	go b.loop()

	// Watch for terminal resizes so wrap-truncation stays correct. The
	// platform-specific installer returns a no-op stop fn on platforms
	// without SIGWINCH (e.g. windows — though bladerunner doesn't target
	// it, the board package builds there).
	b.resizeStop = installResizeWatcher(b)
}

// Stop finalizes the renderer, leaving the last frame visible in scrollback.
// Safe to call multiple times.
func (b *Board) Stop() {
	if !b.interactive {
		return
	}
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	b.started = false
	stopCh := b.stopCh
	resizeStop := b.resizeStop
	b.resizeStop = nil
	b.mu.Unlock()

	if resizeStop != nil {
		resizeStop()
	}
	close(stopCh)
	b.wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.draw(true)
	fmt.Fprint(b.out, "\x1b[?25h") // show cursor
}

// handleResize is invoked by the platform watcher when SIGWINCH fires. It
// re-queries the terminal size, updates the board's width, and forces a
// fresh frame on the next tick (the previous frame's line geometry is no
// longer trustworthy after a resize).
func (b *Board) handleResize() {
	if b.outFile == nil {
		return
	}
	w, _, err := term.GetSize(int(b.outFile.Fd()))
	if err != nil || w <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if w == b.width {
		return
	}
	b.width = w
	// Abandon the previous frame: it may have wrapped at the old width and
	// the cursor is in an unpredictable spot. Print a newline so the new
	// frame starts on a clean line, and reset lastLines so draw() does not
	// try to move up over content that no longer matches.
	fmt.Fprint(b.out, "\n")
	b.lastLines = 0
}

// Begin marks a stage as Running and records its start time. If the stage is
// already running, this is a no-op (preserves the original start time).
func (b *Board) Begin(id string) {
	if !b.interactive {
		logging.L().Info("stage begin", "stage", id, "label", b.labelOf(id))
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stageByID(id)
	if s == nil || s.state == StateRunning {
		return
	}
	s.state = StateRunning
	s.started = time.Now()
	s.substatus = ""
	s.err = nil
}

// Substatus sets a short message rendered next to a Running stage.
func (b *Board) Substatus(id, msg string) {
	if !b.interactive {
		if msg != "" {
			logging.L().Debug("stage status", "stage", id, "status", msg)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.stageByID(id); s != nil {
		s.substatus = msg
	}
}

// Complete marks a stage as Done.
func (b *Board) Complete(id string) {
	if !b.interactive {
		logging.L().Info("stage complete", "stage", id, "label", b.labelOf(id))
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stageByID(id)
	if s == nil {
		return
	}
	if s.started.IsZero() {
		s.started = time.Now()
	}
	s.state = StateDone
	s.finished = time.Now()
	s.substatus = ""
}

// Fail marks a stage as Failed with the supplied error.
func (b *Board) Fail(id string, err error) {
	if !b.interactive {
		logging.L().Error("stage failed", "stage", id, "label", b.labelOf(id), "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stageByID(id)
	if s == nil {
		return
	}
	if s.started.IsZero() {
		s.started = time.Now()
	}
	s.state = StateFailed
	s.finished = time.Now()
	s.err = err
}

// AppendLog adds a single line to the tail panel. Lines longer than the
// terminal width are truncated at render time. No-op in non-interactive mode
// (raw logs already land on disk via console.log).
func (b *Board) AppendLog(line string) {
	if !b.interactive {
		return
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tail) >= b.tailSize {
		b.tail = append(b.tail[:0], b.tail[len(b.tail)-b.tailSize+1:]...)
	}
	b.tail = append(b.tail, line)
}

// Interactive reports whether the board is rendering to a TTY.
func (b *Board) Interactive() bool { return b.interactive }

func (b *Board) labelOf(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.stageByID(id); s != nil {
		return s.Label
	}
	return id
}

func (b *Board) stageByID(id string) *stageRT {
	if i, ok := b.index[id]; ok {
		return b.stages[i]
	}
	return nil
}

func (b *Board) loop() {
	defer b.wg.Done()
	t := time.NewTicker(b.tick)
	defer t.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-t.C:
			b.mu.Lock()
			b.spinner++
			b.draw(false)
			b.mu.Unlock()
		}
	}
}

// draw assumes b.mu is held.
func (b *Board) draw(final bool) {
	if b.lastLines > 0 {
		fmt.Fprintf(b.out, "\r\x1b[%dA\x1b[J", b.lastLines)
	} else {
		fmt.Fprint(b.out, "\r\x1b[J")
	}
	frame := b.renderFrame()
	fmt.Fprint(b.out, frame)
	b.lastLines = strings.Count(frame, "\n")
	if final {
		b.lastLines = 0
	}
}

// RenderFrame returns the static (current) frame as a string. Exposed for
// testing; not part of the supported API.
func (b *Board) RenderFrame() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.renderFrame()
}

func (b *Board) renderFrame() string {
	var sb strings.Builder
	if b.title != "" {
		sb.WriteString(b.applyStyle(titleStyle, b.title))
		sb.WriteByte('\n')
	}
	for _, s := range b.stages {
		sb.WriteString(b.renderStage(s))
		sb.WriteByte('\n')
	}
	if len(b.tail) > 0 {
		sb.WriteString(b.applyStyle(dimStyle, b.divider("console.log (tail)")))
		sb.WriteByte('\n')
		for _, line := range b.tail {
			sb.WriteString(b.applyStyle(dimStyle, truncate(line, b.width)))
			sb.WriteByte('\n')
		}
	}
	if b.consoleLogPath != "" {
		footer := fmt.Sprintf("full log: %s — Ctrl+C to abort", b.consoleLogPath)
		sb.WriteString(b.applyStyle(dimStyle, truncate(footer, b.width)))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (b *Board) renderStage(s *stageRT) string {
	icon, style := b.iconFor(s)
	elapsed := b.elapsedFor(s)

	label := s.Label
	tail := ""
	switch s.state {
	case StateRunning:
		if s.substatus != "" {
			tail = "  " + s.substatus
		}
	case StateFailed:
		if s.err != nil {
			tail = "  " + s.err.Error()
		}
	case StatePending, StateDone:
		// No trailing info.
	}

	padded := label
	if visibleWidth(padded) < labelColumn {
		padded += strings.Repeat(" ", labelColumn-visibleWidth(padded))
	}
	line := fmt.Sprintf("  %s  %s  %s%s", b.applyStyle(style, icon), b.applyStyle(labelStyle, padded), elapsed, tail)
	return truncate(line, b.width)
}

func (b *Board) iconFor(s *stageRT) (string, lipgloss.Style) {
	switch s.state {
	case StateRunning:
		frame := spinnerFrames[b.spinner%len(spinnerFrames)]
		return frame, runningStyle
	case StateDone:
		return "✓", doneStyle
	case StateFailed:
		return "✗", failedStyle
	default:
		return "·", pendingStyle
	}
}

func (b *Board) elapsedFor(s *stageRT) string {
	switch s.state {
	case StatePending:
		return b.applyStyle(dimStyle, "       ")
	case StateRunning:
		d := time.Since(s.started).Round(time.Second)
		return b.applyStyle(dimStyle, fmt.Sprintf("%6s", d))
	default:
		end := s.finished
		if end.IsZero() {
			end = time.Now()
		}
		d := end.Sub(s.started).Round(time.Second)
		return b.applyStyle(dimStyle, fmt.Sprintf("%6s", d))
	}
}

func (b *Board) divider(label string) string {
	if b.width < minDividerWidth {
		return "── " + label + " ──"
	}
	left := "── " + label + " "
	remaining := max(b.width-visibleWidth(left), 0)
	return left + strings.Repeat("─", remaining)
}

func (b *Board) applyStyle(style lipgloss.Style, s string) string {
	if !b.interactive {
		return s
	}
	return style.Render(s)
}

// truncate truncates s to at most w visible columns. It assumes s has no ANSI
// escape codes (apply styling AFTER truncation).
func truncate(s string, w int) string {
	if w <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w < 2 {
		runes := []rune(s)
		return string(runes[:w])
	}
	runes := []rune(s)
	return string(runes[:w-1]) + "…"
}

func visibleWidth(s string) int {
	return utf8.RuneCountInString(s)
}
