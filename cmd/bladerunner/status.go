package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/ui"
)

const (
	labelWidth = 12 // key column width within a panel
	panelWidth = 34 // each panel's inner width
	gapWidth   = 4  // space between panels
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

	left := newPanel("VM")
	left.row("Status", success(status))
	left.rowIf("PID", getConfig(control.ConfigKeyPID))
	left.rowIf("Name", getConfig(control.ConfigKeyName))
	left.rowIf("Arch", getConfig(control.ConfigKeyArch))
	left.sep()
	left.rowIf("CPUs", getConfig(control.ConfigKeyCPUs))
	left.rowGiB("Memory", getConfig(control.ConfigKeyMemoryGiB))
	left.rowGiB("Disk", getConfig(control.ConfigKeyDiskSizeGiB))
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
	right.rowIf("Cloud-init", getConfig(control.ConfigKeyCloudInitISO))
	right.rowIf("Log", getConfig(control.ConfigKeyLogPath))

	fmt.Println(title("Bladerunner Status"))
	fmt.Println(renderPanels(left, right))
	fmt.Printf("  %s %s    %s %s\n",
		subtle("Shell:"), command("br shell"),
		subtle("SSH:"), command("br ssh"))
	fmt.Println()

	return nil
}

// panel accumulates rows of formatted text for a single column.
type panel struct {
	title string
	lines []string
}

func newPanel(name string) *panel {
	return &panel{title: name}
}

func (p *panel) row(label, val string) {
	padded := fmt.Sprintf("%-*s", labelWidth, label)
	p.lines = append(p.lines, fmt.Sprintf("%s  %s", key(padded), val))
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
	p.lines = append(p.lines, subtle(strings.Repeat("─", panelWidth)))
}

func (p *panel) render() string {
	header := key(p.title)
	divider := subtle(strings.Repeat("─", panelWidth))
	body := strings.Join(p.lines, "\n")
	return fmt.Sprintf("%s\n%s\n%s", header, divider, body)
}

// renderPanels places two panels side by side with a gap.
func renderPanels(left, right *panel) string {
	style := lipgloss.NewStyle().Width(panelWidth)
	gap := strings.Repeat(" ", gapWidth)

	l := style.Render(left.render())
	r := style.Render(right.render())

	if !ui.IsTTY() {
		return l + "\n\n" + r
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, "  ", l, gap, r)
}
