// Package ui provides terminal UI components with theme support.
package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Theme represents a color scheme for the UI.
type Theme struct {
	Name    string
	Primary string // blue - titles, keys, headers
	Success string // green
	Warning string // orange/yellow
	Error   string // red
	Muted   string // gray - subtle text
	Value   string // light gray - values
	Command string // command highlight
	CmdBg   string // command background
}

// Predefined themes
var (
	ThemeDefault = Theme{
		Name:    "default",
		Primary: "39",  // light blue
		Success: "42",  // green
		Warning: "214", // orange
		Error:   "196", // red
		Muted:   "243", // gray
		Value:   "252", // light gray
		Command: "229", // yellow
		CmdBg:   "236", // dark gray
	}

	ThemeDracula = Theme{
		Name:    "dracula",
		Primary: "141", // purple
		Success: "84",  // green
		Warning: "228", // yellow
		Error:   "212", // pink
		Muted:   "239", // gray
		Value:   "253", // white
		Command: "117", // cyan
		CmdBg:   "236", // dark gray
	}

	ThemeNord = Theme{
		Name:    "nord",
		Primary: "109", // nord9 - blue
		Success: "150", // nord14 - green
		Warning: "221", // nord13 - yellow
		Error:   "203", // nord11 - red
		Muted:   "243", // nord3 - gray
		Value:   "252", // nord5 - light
		Command: "116", // nord8 - cyan
		CmdBg:   "236", // dark
	}

	ThemeGruvbox = Theme{
		Name:    "gruvbox",
		Primary: "109", // aqua
		Success: "142", // green
		Warning: "214", // orange
		Error:   "167", // red
		Muted:   "243", // gray
		Value:   "250", // light gray
		Command: "175", // purple
		CmdBg:   "236", // dark gray
	}

	currentTheme = ThemeDefault
)

// Styles - initialized with default theme
var (
	titleStyle   lipgloss.Style
	subtleStyle  lipgloss.Style
	successStyle lipgloss.Style
	warningStyle lipgloss.Style
	errorStyle   lipgloss.Style
	keyStyle     lipgloss.Style
	valueStyle   lipgloss.Style
	commandStyle lipgloss.Style
)

func init() {
	SetTheme(ThemeDefault)
}

// SetTheme applies a theme by reinitializing all style variables.
func SetTheme(theme Theme) {
	currentTheme = theme

	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(theme.Primary))

	subtleStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(theme.Muted))

	successStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(theme.Success))

	warningStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(theme.Warning))

	errorStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(theme.Error))

	keyStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(theme.Primary))

	valueStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(theme.Value))

	commandStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(theme.Command)).
		Background(lipgloss.Color(theme.CmdBg)).
		Padding(0, 1)
}

// GetTheme returns the currently active theme.
func GetTheme() Theme {
	return currentTheme
}

// ThemeByName returns a theme by name, or default if not found.
func ThemeByName(name string) Theme {
	switch name {
	case "dracula":
		return ThemeDracula
	case "nord":
		return ThemeNord
	case "gruvbox":
		return ThemeGruvbox
	default:
		return ThemeDefault
	}
}

// ListThemes returns all available theme names.
func ListThemes() []string {
	return []string{"default", "dracula", "nord", "gruvbox"}
}

// IsTTY returns true if stdout is a terminal.
func IsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// styled applies a style only if output is a TTY.
func styled(style lipgloss.Style, s string) string {
	if !IsTTY() {
		return s
	}
	return style.Render(s)
}

// Title returns styled title text.
func Title(s string) string { return styled(titleStyle, s) }

// Subtle returns styled subtle/muted text.
func Subtle(s string) string { return styled(subtleStyle, s) }

// Success returns styled success text.
func Success(s string) string { return styled(successStyle, s) }

// Warning returns styled warning text.
func Warning(s string) string { return styled(warningStyle, s) }

// Error returns styled error text.
func Error(s string) string { return styled(errorStyle, s) }

// Key returns styled key/label text.
func Key(s string) string { return styled(keyStyle, s) }

// Value returns styled value text.
func Value(s string) string { return styled(valueStyle, s) }

// Command returns styled command text with background.
func Command(s string) string { return styled(commandStyle, s) }
