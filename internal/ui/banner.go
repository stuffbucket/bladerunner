package ui

import (
	_ "embed"
	"fmt"
	"math"
	"strings"
)

//go:embed banner
var bannerText string

// gradientStops defines the blue-to-green color ramp (24-bit RGB).
var gradientStops = [][3]float64{
	{138, 92, 246}, // purple
	{88, 101, 242}, // indigo
	{59, 130, 246}, // blue
	{6, 182, 212},  // cyan
	{16, 185, 129}, // emerald
	{52, 211, 153}, // green
}

// Banner returns the ASCII banner with a horizontal gradient applied.
// Returns an empty string if stdout is not a TTY.
func Banner() string {
	if !IsTTY() {
		return ""
	}
	return renderBanner()
}

// renderBanner applies the gradient to the banner text unconditionally.
func renderBanner() string {
	lines := strings.Split(strings.TrimRight(bannerText, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}

	// Find the longest line to normalize the gradient across full width.
	maxLen := 0
	for _, line := range lines {
		if len(line) > maxLen {
			maxLen = len(line)
		}
	}
	if maxLen == 0 {
		return ""
	}

	var buf strings.Builder
	buf.WriteString("\n\n")
	for _, line := range lines {
		runes := []rune(line)
		for i, ch := range runes {
			if ch == ' ' {
				buf.WriteRune(' ')
				continue
			}
			r, g, b := gradientColor(float64(i) / float64(maxLen))
			fmt.Fprintf(&buf, "\x1b[38;2;%d;%d;%dm%c", r, g, b, ch)
		}
		buf.WriteString("\x1b[0m\n")
	}
	buf.WriteString("\n")

	return buf.String()
}

// gradientColor interpolates through the gradient stops at position t [0,1].
func gradientColor(t float64) (int, int, int) {
	t = math.Max(0, math.Min(1, t))

	n := len(gradientStops) - 1
	segment := t * float64(n)
	idx := int(segment)
	if idx >= n {
		idx = n - 1
	}
	frac := segment - float64(idx)

	a := gradientStops[idx]
	b := gradientStops[idx+1]

	r := a[0] + (b[0]-a[0])*frac
	g := a[1] + (b[1]-a[1])*frac
	bl := a[2] + (b[2]-a[2])*frac

	return int(r), int(g), int(bl)
}
