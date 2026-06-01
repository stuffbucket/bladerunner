package board

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func interactive() *bool { v := true; return &v }
func headless() *bool    { v := false; return &v }

func TestRenderFrame_StagesAndTail(t *testing.T) {
	var buf bytes.Buffer
	b := New([]Stage{
		{ID: "vm", Label: "VM running"},
		{ID: "ci", Label: "cloud-init"},
		{ID: "ssh", Label: "SSH ready"},
	}, Options{
		Out:            &buf,
		Title:          "Starting",
		Interactive:    interactive(),
		TailSize:       4,
		ConsoleLogPath: "/tmp/console.log",
	})
	// Force a stable width so the test isn't terminal-dependent.
	b.width = 100

	b.Complete("vm")
	b.Begin("ci")
	b.Substatus("ci", "running module final")
	b.AppendLog("cloud-init[891]: Running module write-files")
	b.AppendLog("cloud-init[891]: Running module users-groups")

	frame := b.RenderFrame()
	for _, want := range []string{
		"Starting",
		"VM running",
		"cloud-init",
		"running module final",
		"SSH ready",
		"console.log (tail)",
		"Running module write-files",
		"Running module users-groups",
		"full log: /tmp/console.log",
	} {
		if !strings.Contains(stripANSI(frame), want) {
			t.Errorf("frame missing %q\n--- got ---\n%s", want, stripANSI(frame))
		}
	}
}

func TestRenderFrame_FailureShowsError(t *testing.T) {
	var buf bytes.Buffer
	b := New([]Stage{{ID: "incus", Label: "Incus API ready"}}, Options{
		Out:         &buf,
		Interactive: interactive(),
	})
	b.width = 80

	b.Begin("incus")
	b.Fail("incus", errors.New("connect: connection refused"))

	frame := stripANSI(b.RenderFrame())
	if !strings.Contains(frame, "Incus API ready") {
		t.Errorf("missing label: %s", frame)
	}
	if !strings.Contains(frame, "connect: connection refused") {
		t.Errorf("missing error text: %s", frame)
	}
}

func TestNonInteractive_RendersNothingToOutput(t *testing.T) {
	var buf bytes.Buffer
	b := New([]Stage{{ID: "vm", Label: "VM"}}, Options{
		Out:         &buf,
		Interactive: headless(),
	})
	b.Start() // no-op
	b.Begin("vm")
	b.AppendLog("kernel: hello")
	b.Complete("vm")
	b.Stop() // no-op

	if buf.Len() != 0 {
		t.Errorf("expected no terminal output in non-interactive mode, got: %q", buf.String())
	}
}

func TestAppendLog_RingBuffer(t *testing.T) {
	var buf bytes.Buffer
	b := New(nil, Options{Out: &buf, Interactive: interactive(), TailSize: 3})
	b.width = 80
	for i, line := range []string{"a", "b", "c", "d", "e"} {
		b.AppendLog(line)
		if i >= 3 && len(b.tail) != 3 {
			t.Errorf("expected ring buffer to cap at 3, got %d", len(b.tail))
		}
	}
	got := strings.Join(b.tail, ",")
	if got != "c,d,e" {
		t.Errorf("expected last 3 lines [c d e], got [%s]", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abcdefghij", 5, "abcd…"},
		{"hello", 0, "hello"},
		{"hi", 1, "h"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.w)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q; want %q", c.in, c.w, got, c.want)
		}
	}
}

// stripANSI removes basic ANSI SGR sequences for stable substring assertions.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			// Skip ESC [ ... letter
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) {
					c := s[j]
					j++
					if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
						break
					}
				}
				i = j - 1
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
