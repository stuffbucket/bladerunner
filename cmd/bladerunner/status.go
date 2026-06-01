package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/ui"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

const (
	labelWidth = 12 // key column width within a panel
	panelWidth = 34 // left panel inner width (its content is short and uniform)
	gapWidth   = 4  // space between panels

	// The right (Build) panel hugs its content between these bounds, capped to
	// fit the terminal with a margin on each edge (see responsiveRightWidth).
	rightPanelMin = panelWidth // never narrower than the left panel
	rightPanelMax = 80         // cap so very long URLs don't stretch absurdly
	edgeMargin    = 2          // columns kept clear at each terminal edge
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the status of Bladerunner VM",
	Long:  `Display the current status of the Bladerunner VM and control server.`,
	RunE:  runStatus,
}

func runStatus(_ *cobra.Command, _ []string) error {
	stateDir := config.DefaultStateDir()
	client := control.NewClient(stateDir)

	// Right panel: always build info.
	right := newPanel("Build")
	right.row("Version", version)
	right.row("Commit", commit)
	right.row("Built", date)

	if !client.IsRunning() {
		left := newPanel("VM")
		left.row("Status", errorf("stopped"))

		fmt.Println(title("Bladerunner Status"))
		fmt.Println(renderPanels(left, right))
		fmt.Println(subtle("  Start the VM with:"), command("br start"))
		fmt.Println()
		return nil
	}

	status, err := client.GetStatus()
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	getConfig := func(k string) string {
		v, err := client.GetConfig(k)
		if err != nil {
			return ""
		}
		return v
	}

	// Color the status by health: running is green, an unreachable guest
	// (host alive but guest not answering — e.g. kernel panic) is amber, and
	// anything else (stopped/unknown) is red.
	statusStyle := errorf
	switch status {
	case control.StatusRunning:
		statusStyle = success
	case control.StatusUnreachable:
		statusStyle = warning
	}

	left := newPanel("VM")
	left.row("Status", statusStyle(status))
	left.rowIf("PID", getConfig(control.ConfigKeyPID))
	left.rowIf("Name", getConfig(control.ConfigKeyName))
	left.rowIf("Arch", getConfig(control.ConfigKeyArch))
	left.sep()
	left.rowIf("CPUs", getConfig(control.ConfigKeyCPUs))
	left.rowGiB("Memory", getConfig(control.ConfigKeyMemoryGiB))
	left.rowGiB("Disk", getConfig(control.ConfigKeyDiskSizeGiB))
	if nv := getConfig(control.ConfigKeyNestedVirt); nv != "" {
		// "enabled" = Incus VMs work; "unsupported"/"disabled" = containers only.
		nvStyle := subtle
		switch nv {
		case "enabled":
			nvStyle = success
		case "disabled":
			nvStyle = warning
		}
		left.row("Incus VMs", nvStyle(nv))
	}
	left.sep()
	if p := getConfig(control.ConfigKeyLocalSSHPort); p != "" {
		left.row("SSH", "localhost:"+p)
	}
	if p := getConfig(control.ConfigKeyLocalAPIPort); p != "" {
		left.row("API", "localhost:"+p)
	}
	left.rowIf("Network", getConfig(control.ConfigKeyNetworkMode))

	right.sep()
	right.rowIf("Image", getConfig(control.ConfigKeyBaseImageURL))
	right.rowIf("Path", getConfig(control.ConfigKeyBaseImagePath))
	right.rowIf("Img Ver", guestImageVersionForStatus(getConfig))
	right.rowIf("Hosted", getConfig(control.ConfigKeyUseHostedGuestImage))
	right.rowIf("Cloud-init", getConfig(control.ConfigKeyCloudInitISO))
	right.rowIf("Log", getConfig(control.ConfigKeyLogPath))

	if b := bannerHeader(); b != "" {
		fmt.Print(b) // gradient ASCII banner (includes its own top margin)
	} else {
		fmt.Println(title("Bladerunner Status"))
	}
	fmt.Println(renderPanels(left, right))
	fmt.Printf("  %s %s    %s %s\n",
		subtle("Shell:"), command("br shell"),
		subtle("SSH:"), command("br ssh"))
	fmt.Println()

	return nil
}

// guestImageVersionForStatus reads /etc/bladerunner-image-version via SSH
// when the SSH config path is available. Returns an empty string if the
// VM doesn't expose SSH yet or the file is missing (typical when the
// running base image is the plain Debian genericcloud, not the pre-baked
// bladerunner guest image).
func guestImageVersionForStatus(getConfig func(string) string) string {
	sshConfig := getConfig(control.ConfigKeySSHConfigPath)
	if sshConfig == "" {
		return ""
	}
	cfg := &config.Config{SSHConfigPath: sshConfig}
	v, err := vm.ReadGuestImageVersion(cfg)
	if err != nil {
		return ""
	}
	return v
}

// bannerHeader returns the gradient ASCII banner indented to align with the
// panels' left margin, or "" when stdout isn't a TTY or the terminal is too
// narrow to fit the banner plus that margin (the caller then prints the plain
// text title instead).
func bannerHeader() string {
	b := ui.Banner()
	if b == "" {
		return ""
	}
	if tw := ui.TerminalWidth(); tw > 0 && ui.BannerWidth()+edgeMargin > tw {
		return ""
	}
	return indentLeft(b, edgeMargin)
}

// indentLeft prefixes n spaces to every non-empty line of s. The pad is plain
// spaces written before any ANSI sequences, so styling is preserved.
func indentLeft(s string, n int) string {
	pad := strings.Repeat(" ", n)
	var b strings.Builder
	first := true
	for line := range strings.SplitSeq(s, "\n") {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		if line != "" {
			b.WriteString(pad)
		}
		b.WriteString(line)
	}
	return b.String()
}

// panel accumulates rows for a single status column. Rows are stored
// structured (not pre-rendered) so render() can wrap each value within its own
// column — continuation lines align under the value, not under the label.
type panel struct {
	title string
	rows  []panelRow
}

type panelRow struct {
	label string // raw label; ignored for separators
	value string // value, possibly already styled by the caller
	sep   bool
}

func newPanel(name string) *panel {
	return &panel{title: name}
}

func (p *panel) row(label, val string) {
	p.rows = append(p.rows, panelRow{label: label, value: val})
}

func (p *panel) rowIf(label, val string) {
	if val != "" {
		p.row(label, value(val))
	}
}

func (p *panel) rowGiB(label, val string) {
	if val != "" {
		p.row(label, value(val+" GiB"))
	}
}

func (p *panel) sep() {
	p.rows = append(p.rows, panelRow{sep: true})
}

// labelColWidth is the width consumed by the label column plus its two-space
// gutter, i.e. the column at which values (and their wrapped continuations)
// begin.
const labelColWidth = labelWidth + 2

// contentWidth is the unwrapped width the panel wants: the title, or the label
// column plus the widest value. Used to let the panel hug its content.
func (p *panel) contentWidth() int {
	w := lipgloss.Width(key(p.title))
	for _, r := range p.rows {
		if r.sep {
			continue
		}
		if lw := labelColWidth + lipgloss.Width(r.value); lw > w {
			w = lw
		}
	}
	return w
}

// render lays the panel out at the given inner width. Each row is a fixed-width
// label cell joined to a value cell that wraps within the remaining columns, so
// a wrapped value stays aligned under the value column rather than collapsing
// to the left margin.
func (p *panel) render(width int) string {
	divider := subtle(strings.Repeat("─", width))
	valueWidth := max(width-labelColWidth, 1)

	out := make([]string, 0, len(p.rows)+2)
	out = append(out, key(p.title), divider)
	for _, r := range p.rows {
		if r.sep {
			out = append(out, divider)
			continue
		}
		labelCell := key(fmt.Sprintf("%-*s", labelWidth, r.label))
		valueCell := lipgloss.NewStyle().Width(valueWidth).Render(r.value)
		out = append(out, lipgloss.JoinHorizontal(lipgloss.Top, labelCell, "  ", valueCell))
	}
	return strings.Join(out, "\n")
}

// renderPanels places the two panels side by side with a gap. The left panel
// is fixed; the right (Build) panel hugs its content responsively.
func renderPanels(left, right *panel) string {
	leftWidth := panelWidth
	rightWidth := responsiveRightWidth(right, leftWidth)

	l := lipgloss.NewStyle().Width(leftWidth).Render(left.render(leftWidth))
	r := lipgloss.NewStyle().Width(rightWidth).Render(right.render(rightWidth))

	if !ui.IsTTY() {
		return l + "\n\n" + r
	}

	gap := strings.Repeat(" ", gapWidth)
	return lipgloss.JoinHorizontal(lipgloss.Top, "  ", l, gap, r)
}

// responsiveRightWidth sizes the right panel to hug its content within
// [rightPanelMin, rightPanelMax], capped to fit the terminal with edgeMargin
// columns clear on each side. With unknown terminal width it just honors the
// content/min/max bounds.
func responsiveRightWidth(right *panel, leftWidth int) int {
	w := right.contentWidth()
	w = min(max(w, rightPanelMin), rightPanelMax)
	if term := ui.TerminalWidth(); term > 0 {
		// edgeMargin + leftWidth + gap + right + edgeMargin <= term
		avail := term - edgeMargin - leftWidth - gapWidth - edgeMargin
		w = min(w, avail)
	}
	return max(w, rightPanelMin) // narrow terminal: prefer min over collapsing
}
